package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ariym/mf-v2/flows"
	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/jobs"
	"github.com/ariym/mf-v2/internal/process"
	"github.com/ariym/mf-v2/internal/store"
	"github.com/google/uuid"
)

// RegisterCustomFlows is called once before the worker starts. Assign it in cmd/mf.
var RegisterCustomFlows func(*flows.Registry) error

func Run(ctx context.Context, c config.Config, instance string, out io.Writer) error {
	lock, e := process.Acquire(c.StateDir())
	if e != nil {
		return e
	}
	defer lock.Close()
	s, e := store.Open(c.SQLitePath)
	if e != nil {
		return e
	}
	defer s.Close()
	listener, e := net.Listen("tcp", c.Address())
	if e != nil {
		return fmt.Errorf("cannot bind server_ip %q and server_port %d: %w", c.ServerIP, c.ServerPort, e)
	}
	defer listener.Close()
	control, e := process.ControlListener(c.StateDir())
	if e != nil {
		return e
	}
	defer control.Close()
	logger := slog.New(slog.NewTextHandler(out, nil))
	engine, e := jobs.New(s, filepath.Dir(c.Path), logger)
	if e != nil {
		return e
	}
	defer engine.Abort()
	if RegisterCustomFlows != nil {
		if e = RegisterCustomFlows(engine.Registry); e != nil {
			return e
		}
	}
	if e = engine.Start(); e != nil {
		return e
	}
	if instance == "" {
		instance = uuid.NewString()
	}
	var ready atomic.Bool
	stop := make(chan struct{})
	var stopOnce sync.Once
	cm := http.NewServeMux()
	cm.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, process.State{PID: os.Getpid(), Instance: instance, Ready: ready.Load()})
	})
	cm.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		n, e := engine.RequestStop(r.Context(), r.URL.Query().Get("force") == "true")
		if e != nil {
			apiError(w, 500, e)
			return
		}
		if n > 0 {
			apiError(w, 409, fmt.Errorf("(%d) jobs running. if you still want to stop run ‘mf daemon stop -f’", n))
			return
		}
		ready.Store(false)
		jsonResponse(w, 200, map[string]bool{"stopping": true})
		stopOnce.Do(func() { close(stop) })
	})
	api := &http.Server{Handler: API(c, s, engine), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	ctrl := &http.Server{Handler: cm, ReadHeaderTimeout: 2 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- api.Serve(listener) }()
	go func() { errs <- ctrl.Serve(control) }()
	if e = process.WriteState(c.StateDir(), process.State{PID: os.Getpid(), Instance: instance, Ready: true}); e != nil {
		engine.Abort()
		api.Close()
		ctrl.Close()
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = engine.Wait(cleanup)
		return e
	}
	ready.Store(true)
	defer os.Remove(filepath.Join(c.StateDir(), "pid.json"))
	logger.Info("server ready", "address", c.Address(), "app_name", c.AppName, "pid", os.Getpid())
	var runErr error
	select {
	case <-ctx.Done():
	case <-stop:
	case runErr = <-errs:
		if errors.Is(runErr, http.ErrServerClosed) {
			runErr = nil
		}
	}
	ready.Store(false)
	engine.Abort()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	apiErr := api.Shutdown(shutdown)
	if apiErr != nil {
		api.Close()
	}
	_ = ctrl.Shutdown(shutdown)
	_ = ctrl.Close()
	workerErr := engine.Wait(shutdown)
	logger.Info("server stopped", "unfinished_jobs", "resume on restart")
	return errors.Join(runErr, apiErr, workerErr)
}
