package injector

import (
	"context"
	"fmt"

	"chaosreplay/internal/scenario"
)

type Applied struct {
	ID       string `json:"id"`
	StepID   string `json:"stepId"`
	Adapter  string `json:"adapter"`
	Reverted bool   `json:"reverted"`
}

type Injector interface {
	Apply(context.Context, string, scenario.Fault) (Applied, error)
	Revert(context.Context, string) error
}

type Unsupported struct{}

func (Unsupported) Apply(_ context.Context, id string, _ scenario.Fault) (Applied, error) {
	return Applied{}, fmt.Errorf("fault injector is unsupported on this platform: %s", id)
}
func (Unsupported) Revert(_ context.Context, id string) error {
	return fmt.Errorf("fault injector is unsupported on this platform: %s", id)
}
