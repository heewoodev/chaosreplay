package assertion

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"chaosreplay/internal/scenario"
)

func TestHTTPAssertion(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer s.Close()
	r := Check(context.Background(), scenario.Assertion{Name: "ready", Type: "http", URL: s.URL, ExpectCode: http.StatusNoContent}, s.Client())
	if !r.Passed {
		t.Fatalf("expected pass: %+v", r)
	}
}

func TestPrometheusAssertion(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") != "error_rate" {
			t.Errorf("query not forwarded")
		}
		_, _ = w.Write([]byte("# TYPE x gauge\nx 0.01\n"))
	}))
	defer s.Close()
	r := Check(context.Background(), scenario.Assertion{Name: "error-rate", Type: "prometheus", URL: s.URL, Query: "error_rate", Expression: "< 0.02"}, s.Client())
	if !r.Passed {
		t.Fatalf("expected pass: %+v", r)
	}
}

func TestPrometheusAPIJSONAssertion(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") != "error_rate" {
			t.Errorf("query not forwarded")
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":["1700000000","0.01"]}]}}`))
	}))
	defer s.Close()
	r := Check(context.Background(), scenario.Assertion{Name: "error-rate", Type: "prometheus", URL: s.URL, Query: "error_rate", Expression: "< 0.02"}, s.Client())
	if !r.Passed {
		t.Fatalf("expected JSON API pass: %+v", r)
	}
}
