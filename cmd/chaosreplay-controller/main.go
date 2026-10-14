package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"chaosreplay/internal/kube"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig path; empty means in-cluster")
	interval := flag.Duration("interval", 2*time.Second, "reconcile interval")
	maxConcurrent := flag.Int("max-concurrent", 1, "maximum simultaneous experiments")
	leaderElection := flag.Bool("leader-election", true, "guard reconcile loop with a Kubernetes Lease")
	leaderNamespace := flag.String("leader-election-namespace", "chaosreplay-system", "namespace for the leader Lease")
	leaderName := flag.String("leader-election-id", "chaosreplay-controller", "leader Lease name")
	agentScheme := flag.String("agent-scheme", "http", "agent scheme: http or https")
	agentPort := flag.Int("agent-port", 8080, "agent port")
	agentCA := flag.String("agent-ca", "", "agent CA PEM for https")
	agentCert := flag.String("agent-client-cert", "", "controller client certificate PEM for https")
	agentKey := flag.String("agent-client-key", "", "controller client key PEM for https")
	healthListen := flag.String("health-listen", ":8081", "health endpoint listen address")
	flag.Parse()
	config, err := kube.Config(*kubeconfig)
	if err != nil {
		log.Fatal(err)
	}
	c, err := kube.NewWithAgent(config, *interval, kube.AgentOptions{Scheme: *agentScheme, Port: *agentPort, CAFile: *agentCA, ClientCertFile: *agentCert, ClientKeyFile: *agentKey, MaxConcurrent: *maxConcurrent})
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	health := &http.Server{Addr: *healthListen, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || (r.URL.Path != "/healthz" && r.URL.Path != "/readyz") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	})}
	go func() {
		if err := health.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("controller health endpoint: %v", err)
		}
	}()
	defer health.Shutdown(context.Background())
	var runErr error
	if *leaderElection {
		runErr = kube.RunLeaderElection(ctx, config, *leaderNamespace, *leaderName, c.Run)
	} else {
		runErr = c.Run(ctx)
	}
	if runErr != nil && ctx.Err() == nil {
		log.Fatal(runErr)
	}
}
