//go:build !linux

package observe

import "fmt"

type unsupportedProcessExitObserver struct{}

func NewProcessExitObserver() (ProcessExitObserver, error) {
	return nil, fmt.Errorf("eBPF process-exit observation requires Linux")
}

func (unsupportedProcessExitObserver) Snapshot() (uint64, error) {
	return 0, fmt.Errorf("eBPF process-exit observation requires Linux")
}
func (unsupportedProcessExitObserver) Close() error { return nil }
