package observe

// ProcessExitObserver exposes a monotonic count of observed process exits.
// Implementations are deliberately observation-only: they never mutate the
// target system and can therefore be enabled independently of fault injection.
type ProcessExitObserver interface {
	Snapshot() (uint64, error)
	Close() error
}
