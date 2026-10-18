// Package example demonstrates a deterministic two-activity workflow.
package example

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ariym/mf-v2/flows"
	"github.com/cschleiden/go-workflows/workflow"
)

func Register(r *flows.Registry) error {
	if err := r.RegisterActivity("example_normalize", Normalize); err != nil {
		return err
	}
	if err := r.RegisterActivity("example_greet", Greet); err != nil {
		return err
	}
	return r.RegisterFlow("greet", Greeting, func(raw json.RawMessage) (json.RawMessage, error) {
		var input Input
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, err
		}
		if strings.TrimSpace(input.Name) == "" {
			return nil, fmt.Errorf("input.name is required")
		}
		return raw, nil
	})
}

type Input struct {
	Name string `json:"name"`
}

func Greeting(ctx workflow.Context, input json.RawMessage) (any, error) {
	var in Input
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	name, err := workflow.ExecuteActivity[string](ctx, flows.ActivityOptions(), "example_normalize", in.Name).Get(ctx)
	if err != nil {
		return nil, err
	}
	return workflow.ExecuteActivity[string](ctx, flows.ActivityOptions(), "example_greet", name).Get(ctx)
}
func Normalize(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return strings.TrimSpace(name), nil
}
func Greet(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "Hello, " + name + "!", nil
}
