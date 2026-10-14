package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	corefake "k8s.io/client-go/kubernetes/fake"

	"chaosreplay/internal/replay"
	"chaosreplay/internal/scenario"
)

func TestReconcileAddsFinalizerBeforeExecution(t *testing.T) {
	spec := scenario.Scenario{APIVersion: "chaosreplay/v1", ID: "demo", Target: scenario.Target{Kind: "kubernetes", Name: "demo", Namespace: "default", Selector: map[string]string{"app": "demo"}}, Steps: []scenario.Step{{ID: "kill", Fault: scenario.Fault{Type: "process_kill", Params: map[string]any{"pid": 2}}}}}
	encoded, _ := json.Marshal(spec)
	var specMap map[string]any
	_ = json.Unmarshal(encoded, &specMap)
	obj := map[string]any{"apiVersion": "chaosreplay.io/v1alpha1", "kind": "ChaosExperiment", "metadata": map[string]any{"name": "demo", "namespace": "default", "finalizers": []any{"other.example/finalizer"}}, "spec": specMap}
	u := &unstructured.Unstructured{Object: obj}
	d := fake.NewSimpleDynamicClient(runtime.NewScheme(), u)
	c := &Controller{dynamic: d, core: corefake.NewSimpleClientset(&corev1.Pod{}), http: &http.Client{Timeout: time.Second}, interval: time.Second, agentScheme: "http", agentPort: 8080, running: map[string]context.CancelFunc{}}
	c.reconcileOne(context.Background(), "default", "demo")
	got, err := d.Resource(experimentGVR).Namespace("default").Get(context.Background(), "demo", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got.GetFinalizers(), finalizerName) {
		t.Fatalf("finalizer missing: %v", got.GetFinalizers())
	}
	if !contains(got.GetFinalizers(), "other.example/finalizer") {
		t.Fatalf("existing finalizer was removed: %v", got.GetFinalizers())
	}
}

func TestReconcileFailsOrphanedRunningExperiment(t *testing.T) {
	obj := map[string]any{"apiVersion": "chaosreplay.io/v1alpha1", "kind": "ChaosExperiment", "metadata": map[string]any{"name": "orphan", "namespace": "default"}, "status": map[string]any{"phase": "Running"}}
	d := fake.NewSimpleDynamicClient(runtime.NewScheme(), &unstructured.Unstructured{Object: obj})
	c := &Controller{dynamic: d, core: corefake.NewSimpleClientset(), http: &http.Client{Timeout: time.Second}, interval: time.Second, agentScheme: "http", agentPort: 8080, running: map[string]context.CancelFunc{}}
	if err := c.reconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := d.Resource(experimentGVR).Namespace("default").Get(context.Background(), "orphan", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	phase, _, err := unstructuredString(got.Object, "status", "phase")
	if err != nil {
		t.Fatal(err)
	}
	if phase != "Failed" {
		t.Fatalf("expected Failed, got %q", phase)
	}
}

func TestReconcileRunsFullExperiment(t *testing.T) {
	var applied, reverted int
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","ebpfProcessExitObserver":false,"observedProcessExits":0}`))
			return
		}
		if r.URL.Path == "/v1/apply" {
			applied++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"x"}`))
			return
		}
		if r.URL.Path == "/v1/revert" {
			reverted++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"reverted":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer agentServer.Close()
	healthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer healthServer.Close()
	spec := scenario.Scenario{APIVersion: "chaosreplay/v1", ID: "full", Target: scenario.Target{Kind: "kubernetes", Name: "checkout", Namespace: "default", Selector: map[string]string{"app": "checkout"}}, Steps: []scenario.Step{{ID: "latency", DurationMS: 100, Fault: scenario.Fault{Type: "network_latency", Params: map[string]any{"interface": "eth0", "delayMs": 1}}}, {ID: "overlap", AfterMS: 10, DurationMS: 1, Fault: scenario.Fault{Type: "process_kill", Params: map[string]any{"pid": 2}}}}, Assertions: []scenario.Assertion{{Name: "health", Type: "http", URL: healthServer.URL, ExpectCode: http.StatusNoContent}}}
	b, _ := json.Marshal(spec)
	var specMap map[string]any
	_ = json.Unmarshal(b, &specMap)
	obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "chaosreplay.io/v1alpha1", "kind": "ChaosExperiment", "metadata": map[string]any{"name": "full", "namespace": "default", "finalizers": []any{finalizerName}}, "spec": specMap}}
	d := fake.NewSimpleDynamicClient(runtime.NewScheme(), obj)
	core := corefake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "checkout-0", Namespace: "default", UID: "pod-1", Labels: map[string]string{"app": "checkout"}}, Spec: corev1.PodSpec{NodeName: "node-1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}}}})
	c := &Controller{dynamic: d, core: core, http: &http.Client{Timeout: time.Second}, agentScheme: "http", agentPort: 8080, agentBase: func(string) string { return agentServer.URL }, interval: time.Second, running: map[string]context.CancelFunc{}}
	c.reconcileOne(context.Background(), "default", "full")
	got, err := d.Resource(experimentGVR).Namespace("default").Get(context.Background(), "full", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	phase, _, _ := unstructuredString(got.Object, "status", "phase")
	if phase != "Succeeded" {
		t.Fatalf("phase=%q status=%v", phase, got.Object["status"])
	}
	if applied != 2 || reverted != 2 {
		t.Fatalf("apply/revert=%d/%d", applied, reverted)
	}
	records, err := core.CoreV1().ConfigMaps("default").List(context.Background(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/managed-by=chaosreplay"})
	if err != nil || len(records.Items) != 1 {
		t.Fatalf("evidence records=%d err=%v", len(records.Items), err)
	}
	if len(records.Items[0].Labels["chaosreplay.io/plan-hash"]) > 63 {
		t.Fatalf("plan hash label exceeds Kubernetes limit")
	}
	var record replay.Record
	if err := json.Unmarshal([]byte(records.Items[0].Data["record.json"]), &record); err != nil {
		t.Fatal(err)
	}
	if record.Outcome != "succeeded" || record.PlanHash == "" {
		t.Fatalf("invalid evidence record: %+v", record)
	}
	if len(record.Operations) != 4 {
		t.Fatalf("expected apply/revert evidence for both events, got %d operations", len(record.Operations))
	}
	if len(record.Observed) != 2 {
		t.Fatalf("expected before/after observation evidence, got %d entries", len(record.Observed))
	}
	for _, operation := range record.Operations {
		if operation.Target != "pod-1" || operation.Adapter != "agent/v1" || operation.StartedAt.IsZero() || operation.EndedAt.IsZero() {
			t.Fatalf("incomplete operation evidence: %+v", operation)
		}
	}
}
