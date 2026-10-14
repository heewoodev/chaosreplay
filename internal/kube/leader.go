package kube

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// RunLeaderElection guards the reconcile loop with a Kubernetes Lease. This
// prevents two controller replicas from injecting the same experiment.
func RunLeaderElection(ctx context.Context, config *rest.Config, namespace, lockName string, run func(context.Context) error) error {
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	identity, err := os.Hostname()
	if err != nil || identity == "" {
		return fmt.Errorf("get leader identity: %w", err)
	}
	lock, err := resourcelock.New("leases", namespace, lockName, client.CoreV1(), client.CoordinationV1(), resourcelock.ResourceLockConfig{Identity: identity})
	if err != nil {
		return err
	}
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock: lock, LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				if err := run(leaderCtx); err != nil && leaderCtx.Err() == nil {
					log.Printf("chaosreplay leader loop: %v", err)
				}
			},
			OnStoppedLeading: func() { log.Printf("chaosreplay lost leader lease") },
			OnNewLeader:      func(identity string) { log.Printf("chaosreplay leader: %s", identity) },
		}, ReleaseOnCancel: true, Name: lockName,
	})
	if err != nil {
		return err
	}
	elector.Run(ctx)
	return ctx.Err()
}
