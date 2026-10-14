package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"chaosreplay/internal/injector"
	"chaosreplay/internal/observe"
	"chaosreplay/internal/scenario"
)

type Request struct {
	ID           string         `json:"id"`
	Fault        scenario.Fault `json:"fault"`
	TTLMS        int64          `json:"ttlMs,omitempty"`
	TargetPodUID string         `json:"targetPodUID,omitempty"`
}
type Server struct {
	Injector injector.Injector
	Observer observe.ProcessExitObserver
}

func (s Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/v1/apply", s.apply)
	mux.HandleFunc("/v1/revert", s.revert)
	return mux
}
func (s Server) health(w http.ResponseWriter, _ *http.Request) {
	result := map[string]any{"status": "ok", "ebpfProcessExitObserver": s.Observer != nil}
	if s.Observer != nil {
		count, err := s.Observer.Snapshot()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		result["observedProcessExits"] = count
	}
	writeJSON(w, result)
}
func (s Server) apply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if !decodeSingle(dec, &req) || req.ID == "" {
		http.Error(w, "invalid request", 400)
		return
	}
	fault := req.Fault
	// These fields are agent-generated and never trusted from the wire.
	if fault.Params != nil {
		delete(fault.Params, "namespacePid")
		delete(fault.Params, "resolvedTarget")
	}
	var err error
	if req.TargetPodUID != "" {
		fault, err = injector.AttachPodTarget(fault, req.TargetPodUID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if req.TTLMS < 0 {
		http.Error(w, "ttlMs must be non-negative", http.StatusBadRequest)
		return
	}
	if req.TTLMS > 24*60*60*1000 {
		http.Error(w, "ttlMs exceeds maximum", http.StatusBadRequest)
		return
	}
	out, err := s.Injector.Apply(r.Context(), req.ID, fault)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if req.TTLMS > 0 {
		go func(id string, ttl int64) {
			<-time.After(time.Duration(ttl) * time.Millisecond)
			_ = s.Injector.Revert(context.Background(), id)
		}(req.ID, req.TTLMS)
	}
	writeJSON(w, out)
}
func (s Server) revert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if !decodeSingle(dec, &req) || req.ID == "" {
		http.Error(w, "invalid request", 400)
		return
	}
	if err := s.Injector.Revert(r.Context(), req.ID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"id": req.ID, "reverted": true})
}
func decodeSingle(dec *json.Decoder, target any) bool {
	if err := dec.Decode(target); err != nil {
		return false
	}
	var extra any
	return dec.Decode(&extra) == io.EOF
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func Run(ctx context.Context, addr string, i injector.Injector) error {
	return RunWithObserver(ctx, addr, i, nil)
}

func RunWithObserver(ctx context.Context, addr string, i injector.Injector, o observe.ProcessExitObserver) error {
	return run(ctx, addr, i, o, nil, "", "")
}

// RunTLS enables mutual TLS. A privileged node agent must not expose an
// unauthenticated control endpoint in an enterprise cluster.
func RunTLS(ctx context.Context, addr string, i injector.Injector, certFile, keyFile, clientCAFile string) error {
	return RunTLSWithObserver(ctx, addr, i, nil, certFile, keyFile, clientCAFile)
}

func RunTLSWithObserver(ctx context.Context, addr string, i injector.Injector, o observe.ProcessExitObserver, certFile, keyFile, clientCAFile string) error {
	caBytes, err := os.ReadFile(clientCAFile)
	if err != nil {
		return fmt.Errorf("read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return fmt.Errorf("parse client CA")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	return run(ctx, addr, i, o, tlsConfig, certFile, keyFile)
}

func run(ctx context.Context, addr string, i injector.Injector, o observe.ProcessExitObserver, tlsConfig *tls.Config, certFile, keyFile string) error {
	srv := &http.Server{Addr: addr, Handler: Server{Injector: i, Observer: o}.Handler()}
	if tlsConfig != nil {
		srv.TLSConfig = tlsConfig
	}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	fmt.Println("chaosreplay-agent listening on", addr)
	if tlsConfig != nil {
		return srv.ListenAndServeTLS(certFile, keyFile)
	}
	return srv.ListenAndServe()
}
