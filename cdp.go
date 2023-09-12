package runn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

const cdpNewKey = "new"

const (
	cdpTimeoutByStep = 60 * time.Second
	cdpWindowWidth   = 1920
	cdpWindowHeight  = 1080
)

type cdpRunner struct {
	name          string
	ctx           context.Context
	cancel        context.CancelFunc
	store         map[string]any
	operator      *operator
	opts          []chromedp.ExecAllocatorOption
	timeoutByStep time.Duration
}

type CDPActions []CDPAction

type CDPAction struct {
	Fn   string
	Args map[string]any
}

func newCDPRunner(name, remote string) (*cdpRunner, error) {
	if remote != cdpNewKey {
		return nil, errors.New("remote connect mode is planned, but not yet implemented")
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.WindowSize(cdpWindowWidth, cdpWindowHeight),
	)

	if os.Getenv("RUNN_DISABLE_HEADLESS") != "" {
		opts = append(opts,
			chromedp.Flag("headless", false),
			chromedp.Flag("hide-scrollbars", false),
			chromedp.Flag("mute-audio", false),
		)
	}

	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, _ := chromedp.NewContext(allocCtx)
	return &cdpRunner{
		name:          name,
		ctx:           ctx,
		cancel:        cancel,
		store:         map[string]any{},
		opts:          opts,
		timeoutByStep: cdpTimeoutByStep,
	}, nil
}

func (rnr *cdpRunner) Close() error {
	if rnr.cancel == nil {
		return nil
	}
	rnr.cancel()
	rnr.cancel = nil
	return nil
}

func (rnr *cdpRunner) Renew() error {
	if err := rnr.Close(); err != nil {
		return err
	}
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), rnr.opts...)
	ctx, _ := chromedp.NewContext(allocCtx)
	rnr.ctx = ctx
	rnr.cancel = cancel
	rnr.store = map[string]any{}
	return nil
}

func (rnr *cdpRunner) Run(_ context.Context, cas CDPActions) error {
	rnr.operator.capturers.captureCDPStart(rnr.name)
	defer rnr.operator.capturers.captureCDPEnd(rnr.name)

	// Set a timeout (cdpTimeoutByStep) for each Step because Chrome operations may get stuck depending on the actions: specified.
	called := false
	defer func() {
		called = true
	}()
	timer := time.NewTimer(rnr.timeoutByStep)
	go func() {
		<-timer.C
		if !called {
			rnr.Close()
		}
	}()

	before := []chromedp.Action{
		chromedp.EmulateViewport(cdpWindowWidth, cdpWindowHeight),
	}
	if err := chromedp.Run(rnr.ctx, before...); err != nil {
		return err
	}
	for i, ca := range cas {
		rnr.operator.capturers.captureCDPAction(ca)
		k, fn, err := findCDPFn(ca.Fn)
		if err != nil {
			return fmt.Errorf("actions[%d] error: %w", i, err)
		}
		if k == "latestTab" {
			infos, err := chromedp.Targets(rnr.ctx)
			if err != nil {
				return err
			}
			latestCtx, _ := chromedp.NewContext(rnr.ctx, chromedp.WithTargetID(infos[0].TargetID))
			rnr.ctx = latestCtx
			continue
		}
		as, err := rnr.evalAction(ca)
		if err != nil {
			return fmt.Errorf("actions[%d] error: %w", i, err)
		}
		if err := chromedp.Run(rnr.ctx, as...); err != nil {
			return fmt.Errorf("actions[%d] error: %w", i, err)
		}
		ras := fn.Args.ResArgs()
		if len(ras) > 0 {
			// capture
			res := map[string]any{}
			for _, arg := range ras {
				v := rnr.store[arg.Key]
				switch vv := v.(type) {
				case *string:
					res[arg.Key] = *vv
				case *map[string]string:
					res[arg.Key] = *vv
				case *[]byte:
					res[arg.Key] = *vv
				default:
					res[arg.Key] = vv
				}
			}
			rnr.operator.capturers.captureCDPResponse(ca, res)
		}
	}

	// record
	r := map[string]any{}
	for k, v := range rnr.store {
		switch vv := v.(type) {
		case *string:
			r[k] = *vv
		case *map[string]string:
			r[k] = *vv
		case *[]byte:
			r[k] = *vv
		default:
			r[k] = vv
		}
	}
	rnr.operator.record(r)

	rnr.store = map[string]any{} // clear

	return nil
}

func (rnr *cdpRunner) evalAction(ca CDPAction) ([]chromedp.Action, error) {
	_, fn, err := findCDPFn(ca.Fn)
	if err != nil {
		return nil, err
	}

	// path resolution for setUploadFile.path
	if ca.Fn == "setUploadFile" {
		p, ok := ca.Args["path"]
		if !ok {
			return nil, fmt.Errorf("invalid action: %v: arg '%s' not found", ca, "path")
		}
		pp, ok := p.(string)
		if !ok {
			return nil, fmt.Errorf("invalid action: %v", ca)
		}
		if !strings.HasPrefix(pp, "/") {
			ca.Args["path"] = filepath.Join(rnr.operator.root, pp)
		}
	}

	fv := reflect.ValueOf(fn.Fn)
	vs := []reflect.Value{}
	for i, a := range fn.Args {
		switch a.Typ {
		case CDPArgTypeArg:
			v, ok := ca.Args[a.Key]
			if !ok {
				return nil, fmt.Errorf("invalid action: %v: arg '%s' not found", ca, a.Key)
			}
			if v == nil {
				return nil, fmt.Errorf("invalid action arg: %s.%s = %v", ca.Fn, a.Key, v)
			}
			vs = append(vs, reflect.ValueOf(v))
		case CDPArgTypeRes:
			k := a.Key
			switch reflect.TypeOf(fn.Fn).In(i).Elem().Kind() {
			case reflect.String:
				var v string
				rnr.store[k] = &v
				vs = append(vs, reflect.ValueOf(&v))
			case reflect.Map:
				// ex. attributes
				v := map[string]string{}
				rnr.store[k] = &v
				vs = append(vs, reflect.ValueOf(&v))
			case reflect.Slice:
				var v []byte
				rnr.store[k] = &v
				vs = append(vs, reflect.ValueOf(&v))
			default:
				return nil, fmt.Errorf("invalid action: %v", ca)
			}
		default:
			return nil, fmt.Errorf("invalid action: %v", ca)
		}
	}
	res := fv.Call(vs)
	a, ok := res[0].Interface().(chromedp.Action)
	if ok {
		return []chromedp.Action{a}, nil
	}
	as, ok := res[0].Interface().([]chromedp.Action)
	if ok {
		return as, nil
	}
	return nil, fmt.Errorf("invalid action: %v", ca)
}
