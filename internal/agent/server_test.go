package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"chaosreplay/internal/injector"
	"chaosreplay/internal/observe"
	"chaosreplay/internal/scenario"
)

type fakeInjector struct {
	mu     sync.Mutex
	active map[string]bool
}

type fakeObserver struct{}

func (fakeObserver) Snapshot() (uint64, error) { return 7, nil }
func (fakeObserver) Close() error              { return nil }

func (f *fakeInjector) Apply(_ context.Context, id string, _ scenario.Fault) (injector.Applied, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active == nil {
		f.active = map[string]bool{}
	}
	f.active[id] = true
	return injector.Applied{ID: id}, nil
}

var _ observe.ProcessExitObserver = fakeObserver{}

func TestHealthReportsObservationCounter(t *testing.T) {
	s := httptest.NewServer(Server{Injector: &fakeInjector{}, Observer: fakeObserver{}}.Handler())
	defer s.Close()
	resp, err := http.Get(s.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status %d", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["observedProcessExits"] != float64(7) || got["ebpfProcessExitObserver"] != true {
		t.Fatalf("unexpected health response: %#v", got)
	}
}

func TestServerRejectsTrailingJSON(t *testing.T) {
	s := httptest.NewServer(Server{Injector: &fakeInjector{}}.Handler())
	defer s.Close()
	resp, err := http.Post(s.URL+"/v1/revert", "application/json", bytes.NewBufferString(`{"id":"x"} {"extra":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("trailing JSON status=%d", resp.StatusCode)
	}
}

func TestServerRejectsUnsafeTTLBeforeApply(t *testing.T) {
	f := &fakeInjector{}
	s := httptest.NewServer(Server{Injector: f}.Handler())
	defer s.Close()
	b, _ := json.Marshal(Request{ID: "unsafe", TTLMS: -1, Fault: scenario.Fault{Type: "process_kill"}})
	resp, err := http.Post(s.URL+"/v1/apply", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsafe TTL status=%d", resp.StatusCode)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active["unsafe"] {
		t.Fatal("fault was applied before unsafe TTL was rejected")
	}
}
func (f *fakeInjector) Revert(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.active, id)
	return nil
}

func TestServerApplyAndRevert(t *testing.T) {
	f := &fakeInjector{}
	s := httptest.NewServer(Server{Injector: f}.Handler())
	defer s.Close()
	b, _ := json.Marshal(Request{ID: "x", Fault: scenario.Fault{Type: "process_kill"}})
	resp, err := http.Post(s.URL+"/v1/apply", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply status %d", resp.StatusCode)
	}
	b, _ = json.Marshal(map[string]string{"id": "x"})
	resp, err = http.Post(s.URL+"/v1/revert", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revert status %d", resp.StatusCode)
	}
}
