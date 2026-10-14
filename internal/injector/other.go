//go:build !linux

package injector

import (
	"context"
	"fmt"

	"chaosreplay/internal/scenario"
)

// Linux is intentionally unavailable on non-Linux hosts. The CLI can still
// validate and plan scenarios on Windows/macOS, but cannot inject faults.
type Linux struct{}

func NewLinux() *Linux { return &Linux{} }

func NewLinuxWithState(_ string) (*Linux, error) { return &Linux{}, nil }

func AttachPodTarget(f scenario.Fault, podUID string) (scenario.Fault, error) {
	return f, fmt.Errorf("pod target resolution is unsupported on this platform: %s", podUID)
}

func (*Linux) Apply(_ context.Context, id string, _ scenario.Fault) (Applied, error) {
	return Applied{}, fmt.Errorf("Linux fault injection unavailable on this platform: %s", id)
}

func (*Linux) Revert(_ context.Context, id string) error {
	return fmt.Errorf("Linux fault injection unavailable on this platform: %s", id)
}
