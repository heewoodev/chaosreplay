package main

import (
	"context"
	"flag"
	"os/signal"
	"syscall"

	"chaosreplay/internal/agent"
	"chaosreplay/internal/injector"
	"chaosreplay/internal/observe"
)

func main() {
	addr := flag.String("listen", ":8080", "listen address")
	cert := flag.String("tls-cert", "", "server certificate PEM; enables mTLS when set")
	key := flag.String("tls-key", "", "server private key PEM")
	clientCA := flag.String("client-ca", "", "client CA PEM required for mTLS")
	stateFile := flag.String("state-file", "/var/lib/chaosreplay/active.json", "durable fault journal")
	observeExits := flag.Bool("observe-process-exits", false, "attach an observation-only eBPF sched_process_exit probe")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	injectorImpl, err := injector.NewLinuxWithState(*stateFile)
	if err != nil {
		panic(err)
	}
	var observer observe.ProcessExitObserver
	if *observeExits {
		observer, err = observe.NewProcessExitObserver()
		if err != nil {
			panic(err)
		}
		defer observer.Close()
	}
	var runErr error
	if *cert != "" || *key != "" || *clientCA != "" {
		if *cert == "" || *key == "" || *clientCA == "" {
			panic("--tls-cert, --tls-key and --client-ca are required together")
		}
		runErr = agent.RunTLSWithObserver(ctx, *addr, injectorImpl, observer, *cert, *key, *clientCA)
	} else {
		runErr = agent.RunWithObserver(ctx, *addr, injectorImpl, observer)
	}
	if runErr != nil {
		panic(runErr)
	}
}
