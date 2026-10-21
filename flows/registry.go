// Package flows is the extension point for compiled Go workflows and activities.
// Workflows must be deterministic; perform side effects in registered activities.
package flows

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/cschleiden/go-workflows/registry"
	"github.com/cschleiden/go-workflows/worker"
	"github.com/cschleiden/go-workflows/workflow"
)

type Flow func(workflow.Context, json.RawMessage) (any, error)

// Prepare validates and normalizes JSON before durable submission. It runs outside replay.
type Prepare func(json.RawMessage) (json.RawMessage, error)
type Registry struct {
	worker      *worker.Worker
	lifetime    context.Context
	definitions map[string]Prepare
}

func New(w *worker.Worker, lifetime context.Context) *Registry {
	return &Registry{w, lifetime, map[string]Prepare{}}
}
func (r *Registry) RegisterFlow(name string, fn Flow, prepare Prepare) error {
	if name == "" || fn == nil {
		return fmt.Errorf("flow name and function are required")
	}
	if _, ok := r.definitions[name]; ok {
		return fmt.Errorf("flow %q already registered", name)
	}
	if e := r.worker.RegisterWorkflow(fn, registry.WithName(name)); e != nil {
		return e
	}
	r.definitions[name] = prepare
	return nil
}

// RegisterActivity registers a function taking context.Context as its first parameter.
// The wrapper cancels its context when the server stops; activities must honor cancellation.
func (r *Registry) RegisterActivity(name string, fn any) error {
	value := reflect.ValueOf(fn)
	if !value.IsValid() || value.Kind() != reflect.Func || value.IsNil() {
		return fmt.Errorf("activity must be a non-nil function")
	}
	t := value.Type()
	contextType := reflect.TypeOf((*context.Context)(nil)).Elem()
	if t.NumIn() == 0 || t.In(0) != contextType {
		return fmt.Errorf("activity must accept context.Context first")
	}
	wrapped := reflect.MakeFunc(t, func(args []reflect.Value) []reflect.Value {
		ctx, cancel := context.WithCancel(args[0].Interface().(context.Context))
		stop := context.AfterFunc(r.lifetime, cancel)
		defer stop()
		defer cancel()
		if r.lifetime.Err() != nil {
			cancel()
		}
		args[0] = reflect.ValueOf(ctx)
		return value.Call(args)
	})
	return r.worker.RegisterActivity(wrapped.Interface(), registry.WithName(name))
}
func (r *Registry) Prepare(name string, input json.RawMessage) (json.RawMessage, error) {
	prepare, ok := r.definitions[name]
	if !ok {
		return nil, fmt.Errorf("unknown flow %q", name)
	}
	if len(input) == 0 || !json.Valid(input) {
		return nil, fmt.Errorf("input must be valid JSON")
	}
	if prepare != nil {
		return prepare(input)
	}
	return input, nil
}
func (r *Registry) Has(name string) bool { _, ok := r.definitions[name]; return ok }
func ActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{RetryOptions: workflow.RetryOptions{MaxAttempts: 3, FirstRetryInterval: time.Second, MaxRetryInterval: 5 * time.Second, BackoffCoefficient: 2}}
}
