package kube

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"chaosreplay/internal/assertion"
	"chaosreplay/internal/fingerprint"
	"chaosreplay/internal/replay"
	"chaosreplay/internal/scenario"
)

var experimentGVR = schemaGVR()

const finalizerName = "chaosreplay.io/finalizer"

func schemaGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "chaosreplay.io", Version: "v1alpha1", Resource: "chaosexperiments"}
}

type Controller struct {
	dynamic     dynamic.Interface
	core        kubernetes.Interface
	http        *http.Client
	agentScheme string
	agentPort   int
	agentBase   func(string) string
	interval    time.Duration
	mu          sync.Mutex
	running     map[string]context.CancelFunc
	slots       chan struct{}
}

func New(config *rest.Config, interval time.Duration) (*Controller, error) {
	return NewWithAgent(config, interval, AgentOptions{})
}

type AgentOptions struct {
	Scheme         string
	Port           int
	CAFile         string
	ClientCertFile string
	ClientKeyFile  string
	MaxConcurrent  int
}

func NewWithAgent(config *rest.Config, interval time.Duration, options AgentOptions) (*Controller, error) {
	d, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	c, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if options.Scheme == "" {
		options.Scheme = "http"
	}
	if options.Port == 0 {
		options.Port = 8080
	}
	if options.MaxConcurrent <= 0 {
		options.MaxConcurrent = 1
	}
	httpClient := &http.Client{Timeout: 10 * time.Second}
	if options.Scheme == "https" {
		if options.CAFile == "" || options.ClientCertFile == "" || options.ClientKeyFile == "" {
			return nil, fmt.Errorf("https agent requires ca, client certificate and key")
		}
		caBytes, err := os.ReadFile(options.CAFile)
		if err != nil {
			return nil, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caBytes) {
			return nil, fmt.Errorf("parse agent CA")
		}
		cert, err := tls.LoadX509KeyPair(options.ClientCertFile, options.ClientKeyFile)
		if err != nil {
			return nil, err
		}
		httpClient.Transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{cert}}}
	}
	agentScheme := options.Scheme
	agentPort := options.Port
	return &Controller{dynamic: d, core: c, http: httpClient, agentScheme: agentScheme, agentPort: agentPort, agentBase: func(addr string) string { return agentScheme + "://" + addr + ":" + strconv.Itoa(agentPort) }, interval: interval, running: map[string]context.CancelFunc{}, slots: make(chan struct{}, options.MaxConcurrent)}, nil
}

func Config(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

func (c *Controller) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	if err := c.reconcileAll(ctx); err != nil {
		log.Printf("chaosreplay reconcile: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := c.reconcileAll(ctx); err != nil {
				log.Printf("chaosreplay reconcile: %v", err)
			}
		}
	}
}

func (c *Controller) reconcileAll(ctx context.Context) error {
	list, err := c.dynamic.Resource(experimentGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := range list.Items {
		item := list.Items[i]
		key := item.GetNamespace() + "/" + item.GetName()
		phase, _, _ := unstructuredString(item.Object, "status", "phase")
		if item.GetDeletionTimestamp() != nil {
			c.mu.Lock()
			cancel := c.running[key]
			_, busy := c.running[key]
			c.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			if !busy && contains(item.GetFinalizers(), finalizerName) {
				_ = c.removeFinalizer(ctx, item.GetNamespace(), item.GetName())
			}
			continue
		}
		if phase == "Running" {
			c.mu.Lock()
			_, busy := c.running[key]
			c.mu.Unlock()
			if !busy {
				// A controller restart loses the in-memory run lease. Do not
				// silently replay a possibly half-applied experiment; the Agent
				// TTL watchdog owns cleanup and the CRD is failed closed.
				_ = c.status(ctx, c.dynamic.Resource(experimentGVR).Namespace(item.GetNamespace()), item.GetName(), "Failed", "controller restarted during experiment; Agent TTL cleanup required", map[string]any{"recovered": true})
			}
			continue
		}
		if phase == "" || phase == "Pending" {
			if c.slots != nil {
				select {
				case c.slots <- struct{}{}:
				default:
					continue
				}
			}
			c.mu.Lock()
			_, busy := c.running[key]
			var runCtx context.Context
			var cancel context.CancelFunc
			if !busy {
				runCtx, cancel = context.WithCancel(ctx)
				c.running[key] = cancel
			}
			c.mu.Unlock()
			if !busy {
				go func(namespace, name, key string, runCtx context.Context) {
					defer func() {
						c.mu.Lock()
						delete(c.running, key)
						c.mu.Unlock()
						if c.slots != nil {
							<-c.slots
						}
					}()
					c.reconcileOne(runCtx, namespace, name)
				}(item.GetNamespace(), item.GetName(), key, runCtx)
			} else if c.slots != nil {
				<-c.slots
			}
		}
	}
	return nil
}

func (c *Controller) reconcileOne(ctx context.Context, namespace, name string) {
	res := c.dynamic.Resource(experimentGVR).Namespace(namespace)
	obj, err := res.Get(ctx, name, metav1.GetOptions{})
	if err != nil || obj.GetDeletionTimestamp() != nil {
		return
	}
	if !contains(obj.GetFinalizers(), finalizerName) {
		finalizers := append([]string{}, obj.GetFinalizers()...)
		finalizers = append(finalizers, finalizerName)
		patch, patchErr := json.Marshal(map[string]any{"metadata": map[string]any{"finalizers": finalizers}})
		if patchErr == nil {
			_, _ = res.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
		}
		return
	}
	phase, _, _ := unstructuredString(obj.Object, "status", "phase")
	if phase == "Running" || phase == "Succeeded" || phase == "Failed" {
		return
	}
	_ = c.status(ctx, res, name, "Running", "reconciling", nil)
	var s scenario.Scenario
	b, _ := json.Marshal(obj.Object["spec"])
	if err := json.Unmarshal(b, &s); err != nil {
		_ = c.status(ctx, res, name, "Failed", err.Error(), nil)
		return
	}
	if err := scenario.Validate(s); err != nil {
		_ = c.status(ctx, res, name, "Failed", err.Error(), nil)
		return
	}
	p, err := replay.Build(s)
	if err != nil {
		_ = c.status(ctx, res, name, "Failed", err.Error(), nil)
		return
	}
	pHash, _ := replay.HashPlan(p)
	env := fingerprint.Capture()
	runID := newRunID()
	record := replay.Record{Plan: p, PlanHash: pHash, RunID: runID, Outcome: "running", RecordedAt: time.Now().UTC(), Adapter: "kubernetes-controller/v1", Environment: env, EnvironmentHash: fingerprint.Hash(env)}
	recordName := evidenceName(pHash, runID)
	finalized := false
	defer func() {
		if !finalized {
			record.Outcome = "failed"
			_ = c.saveRecord(context.Background(), namespace, recordName, record)
		}
	}()
	if err := c.saveRecord(ctx, namespace, recordName, record); err != nil {
		_ = c.status(ctx, res, name, "Failed", "cannot persist evidence record: "+err.Error(), map[string]any{"planHash": pHash})
		return
	}
	pods, err := c.targets(ctx, s)
	if err != nil {
		_ = c.status(ctx, res, name, "Failed", err.Error(), map[string]any{"planHash": pHash, "recordName": recordName})
		return
	}
	if len(pods) == 0 {
		_ = c.status(ctx, res, name, "Failed", "target selector matched no pods", map[string]any{"planHash": pHash, "recordName": recordName})
		return
	}
	operations, observations, runErr := c.runTargets(ctx, pods, p)
	record.Operations = append(record.Operations, operations...)
	record.Observed = append(record.Observed, observations...)
	if runErr != nil {
		_ = c.saveRecord(context.Background(), namespace, recordName, record)
		_ = c.status(ctx, res, name, "Failed", runErr.Error(), map[string]any{"planHash": pHash, "recordName": recordName})
		return
	}
	for _, a := range s.Assertions {
		result := assertion.Check(ctx, a, c.http)
		record.Assertions = append(record.Assertions, replay.AssertionResult{Name: result.Name, Passed: result.Passed, Detail: result.Detail})
		if !result.Passed {
			record.Outcome = "failed"
			_ = c.saveRecord(ctx, namespace, recordName, record)
			_ = c.status(ctx, res, name, "Failed", "assertion failed: "+result.Detail, map[string]any{"planHash": pHash, "recordName": recordName, "assertion": result})
			return
		}
	}
	record.Outcome = "succeeded"
	if err := c.saveRecord(ctx, namespace, recordName, record); err != nil {
		_ = c.status(ctx, res, name, "Failed", "cannot finalize evidence record: "+err.Error(), map[string]any{"planHash": pHash, "recordName": recordName})
		return
	}
	finalized = true
	_ = c.status(ctx, res, name, "Succeeded", "experiment completed", map[string]any{"planHash": pHash, "scenarioHash": p.ScenarioHash, "targetCount": len(pods), "recordName": recordName})
}

type podRunResult struct {
	operations []replay.Operation
	observed   []string
	err        error
}

func (c *Controller) runTargets(ctx context.Context, pods []corev1.Pod, p replay.Plan) ([]replay.Operation, []string, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Leave room for the per-node health sample and TLS handshake. The
	// relative event offsets remain deterministic from this common barrier.
	startAt := time.Now().Add(2 * time.Second)
	results := make(chan podRunResult, len(pods))
	var wg sync.WaitGroup
	for _, pod := range pods {
		pod := pod
		wg.Add(1)
		go func() {
			defer wg.Done()
			operations, observations, err := c.runPod(runCtx, pod, p, startAt)
			if err != nil {
				cancel()
			}
			results <- podRunResult{operations: operations, observed: observations, err: err}
		}()
	}
	wg.Wait()
	close(results)
	var operations []replay.Operation
	var observations []string
	for result := range results {
		operations = append(operations, result.operations...)
		observations = append(observations, result.observed...)
		if result.err != nil {
			cancel()
			return operations, observations, result.err
		}
	}
	return operations, observations, nil
}

func (c *Controller) targets(ctx context.Context, s scenario.Scenario) ([]corev1.Pod, error) {
	if s.Target.Kind != "kubernetes" && s.Target.Kind != "pod" {
		return nil, fmt.Errorf("unsupported Kubernetes target kind %q", s.Target.Kind)
	}
	selector := labels.Set(s.Target.Selector).AsSelector().String()
	if selector == "" {
		return nil, fmt.Errorf("target.selector is required")
	}
	list, err := c.core.CoreV1().Pods(s.Target.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	running := make([]corev1.Pod, 0, len(list.Items))
	for _, pod := range list.Items {
		if pod.Status.Phase == corev1.PodRunning && pod.UID != "" {
			running = append(running, pod)
		}
	}
	return running, nil
}

func (c *Controller) runPod(ctx context.Context, pod corev1.Pod, p replay.Plan, startAt time.Time) (operations []replay.Operation, observations []string, err error) {
	if pod.Spec.NodeName == "" {
		return nil, nil, fmt.Errorf("pod %s has no node", pod.Name)
	}
	node, err := c.core.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	addr := ""
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			addr = a.Address
			break
		}
	}
	if addr == "" {
		return nil, nil, fmt.Errorf("node %s has no InternalIP", node.Name)
	}
	base := c.agentScheme + "://" + addr + ":" + strconv.Itoa(c.agentPort)
	if c.agentBase != nil {
		base = c.agentBase(addr)
	}
	applied := []string{}
	var appliedMu sync.Mutex
	var operationsMu sync.Mutex
	var observationsMu sync.Mutex
	appendOperation := func(operation replay.Operation) {
		operationsMu.Lock()
		operations = append(operations, operation)
		operationsMu.Unlock()
	}
	appendObservation := func(value string) {
		observationsMu.Lock()
		observations = append(observations, value)
		observationsMu.Unlock()
	}
	before, beforeErr := c.agentObservation(ctx, base, string(pod.UID))
	if beforeErr != nil {
		appendObservation(fmt.Sprintf("target=%s phase=before error=%s", pod.UID, beforeErr))
	} else {
		appendObservation(before)
	}
	defer func() {
		appliedMu.Lock()
		ids := append([]string{}, applied...)
		appliedMu.Unlock()
		sort.Strings(ids)
		for i := len(ids) - 1; i >= 0; i-- {
			started := time.Now().UTC()
			err := agentCall(c.http, context.Background(), base+"/v1/revert", map[string]any{"id": ids[i]})
			appendOperation(replay.Operation{ID: ids[i], Target: string(pod.UID), Action: "revert-recovery", Adapter: "agent/v1", StartedAt: started, EndedAt: time.Now().UTC(), Error: errorString(err)})
		}
		after, afterErr := c.agentObservation(context.Background(), base, string(pod.UID))
		if afterErr != nil {
			appendObservation(fmt.Sprintf("target=%s phase=after error=%s", pod.UID, afterErr))
		} else {
			appendObservation(after)
		}
	}()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type eventResult struct {
		operations []replay.Operation
		err        error
	}
	results := make(chan eventResult, len(p.Events))
	var wg sync.WaitGroup
	startGates := make([]chan struct{}, len(p.Events))
	completionGates := make([]chan struct{}, len(p.Events))
	for index, event := range p.Events {
		event := event
		startGates[index] = make(chan struct{})
		completionGates[index] = make(chan struct{})
		var previousGate <-chan struct{}
		if index > 0 && p.Events[index-1].StartMS == event.StartMS {
			previousGate = startGates[index-1]
		}
		var previousCompletion <-chan struct{}
		resource := faultResourceKey(event.Fault)
		for previous := index - 1; previous >= 0; previous-- {
			if faultResourceKey(p.Events[previous].Fault) == resource && p.Events[previous].EndMS >= event.StartMS {
				previousCompletion = completionGates[previous]
				break
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(completionGates[index])
			eventOperations := []replay.Operation{}
			waitFor := startAt.Add(time.Duration(event.StartMS) * time.Millisecond).Sub(time.Now())
			if waitFor < -100*time.Millisecond {
				results <- eventResult{err: fmt.Errorf("missed deterministic start for step %s by %s", event.StepID, -waitFor)}
				cancel()
				return
			}
			if waitFor > 0 {
				timer := time.NewTimer(waitFor)
				defer timer.Stop()
				select {
				case <-runCtx.Done():
					results <- eventResult{err: runCtx.Err()}
					return
				case <-timer.C:
				}
			}
			if previousGate != nil {
				select {
				case <-runCtx.Done():
					results <- eventResult{err: runCtx.Err()}
					return
				case <-previousGate:
				}
			}
			if previousCompletion != nil {
				select {
				case <-runCtx.Done():
					results <- eventResult{err: runCtx.Err()}
					return
				case <-previousCompletion:
				}
			}
			id := p.ScenarioHash + "/" + string(pod.UID) + "/" + event.StepID
			applyStarted := time.Now().UTC()
			applyErr := agentCall(c.http, runCtx, base+"/v1/apply", map[string]any{"id": id, "fault": event.Fault, "ttlMs": event.EndMS - event.StartMS + 5000, "targetPodUID": string(pod.UID)})
			eventOperations = append(eventOperations, replay.Operation{ID: id, StepID: event.StepID, Target: string(pod.UID), Action: "apply", Adapter: "agent/v1", StartedAt: applyStarted, EndedAt: time.Now().UTC(), Error: errorString(applyErr)})
			if applyErr != nil {
				results <- eventResult{operations: eventOperations, err: applyErr}
				cancel()
				return
			}
			appliedMu.Lock()
			applied = append(applied, id)
			appliedMu.Unlock()
			close(startGates[index])
			if event.EndMS > event.StartMS {
				timer := time.NewTimer(time.Duration(event.EndMS-event.StartMS) * time.Millisecond)
				defer timer.Stop()
				select {
				case <-runCtx.Done():
					results <- eventResult{operations: eventOperations, err: runCtx.Err()}
					return
				case <-timer.C:
				}
			}
			revertStarted := time.Now().UTC()
			revertErr := agentCall(c.http, runCtx, base+"/v1/revert", map[string]any{"id": id})
			eventOperations = append(eventOperations, replay.Operation{ID: id, StepID: event.StepID, Target: string(pod.UID), Action: "revert", Adapter: "agent/v1", StartedAt: revertStarted, EndedAt: time.Now().UTC(), Error: errorString(revertErr)})
			if revertErr != nil {
				results <- eventResult{operations: eventOperations, err: revertErr}
				cancel()
				return
			}
			appliedMu.Lock()
			for i, activeID := range applied {
				if activeID == id {
					applied = append(applied[:i], applied[i+1:]...)
					break
				}
			}
			appliedMu.Unlock()
			results <- eventResult{operations: eventOperations}
		}()
	}
	wg.Wait()
	close(results)
	for result := range results {
		operations = append(operations, result.operations...)
		if result.err != nil && !errors.Is(result.err, context.Canceled) {
			return operations, observations, result.err
		}
	}
	if err := ctx.Err(); err != nil {
		return operations, observations, err
	}
	return operations, observations, nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type agentHealth struct {
	Status                  string `json:"status"`
	EBPFProcessExitObserver bool   `json:"ebpfProcessExitObserver"`
	ObservedProcessExits    uint64 `json:"observedProcessExits"`
}

func (c *Controller) agentObservation(ctx context.Context, base, target string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("agent health status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var health agentHealth
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	if err := decoder.Decode(&health); err != nil {
		return "", fmt.Errorf("decode agent health: %w", err)
	}
	return fmt.Sprintf("target=%s phase=health status=%s ebpf=%t processExits=%d", target, health.Status, health.EBPFProcessExitObserver, health.ObservedProcessExits), nil
}

func faultResourceKey(f scenario.Fault) string {
	switch f.Type {
	case "network_latency", "packet_loss":
		return "network:" + fmt.Sprint(f.Params["interface"])
	case "process_kill":
		return "process:" + fmt.Sprint(f.Params["pid"])
	case "disk_io_limit":
		return "disk:" + fmt.Sprint(f.Params["major"]) + ":" + fmt.Sprint(f.Params["minor"])
	default:
		return f.Type
	}
}

func agentCall(client *http.Client, ctx context.Context, endpoint string, body any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agent %s: %s", endpoint, string(msg))
	}
	return nil
}

func (c *Controller) status(ctx context.Context, res dynamic.ResourceInterface, name, phase, message string, extra map[string]any) error {
	st := map[string]any{"phase": phase, "message": message, "updatedAt": time.Now().UTC().Format(time.RFC3339Nano)}
	for k, v := range extra {
		st[k] = v
	}
	obj := map[string]any{"status": st}
	b, _ := json.Marshal(obj)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		_, lastErr = res.Patch(ctx, name, types.MergePatchType, b, metav1.PatchOptions{}, "status")
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
		}
	}
	return lastErr
}

func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("%x", b[:])
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

func evidenceName(planHash, runID string) string {
	if len(planHash) > 12 {
		planHash = planHash[:12]
	}
	if len(runID) > 16 {
		runID = runID[:16]
	}
	return "chaosreplay-" + planHash + "-" + runID
}

func (c *Controller) saveRecord(ctx context.Context, namespace, name string, record replay.Record) error {
	sort.Strings(record.Observed)
	sort.SliceStable(record.Operations, func(i, j int) bool {
		left, right := record.Operations[i], record.Operations[j]
		if left.Target != right.Target {
			return left.Target < right.Target
		}
		if left.StepID != right.StepID {
			return left.StepID < right.StepID
		}
		if left.Action != right.Action {
			return left.Action < right.Action
		}
		return left.ID < right.ID
	})
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data := map[string]string{"record.json": string(b)}
	api := c.core.CoreV1().ConfigMaps(namespace)
	cm, err := api.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "chaosreplay", "chaosreplay.io/run-id": labelValue(record.RunID), "chaosreplay.io/plan-hash": labelValue(record.PlanHash)}}, Data: data}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	cm.Data = data
	_, err = api.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func labelValue(value string) string {
	if len(value) > 63 {
		return value[:63]
	}
	return value
}

func (c *Controller) removeFinalizer(ctx context.Context, namespace, name string) error {
	res := c.dynamic.Resource(experimentGVR).Namespace(namespace)
	obj, err := res.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	remaining := make([]string, 0, len(obj.GetFinalizers()))
	for _, f := range obj.GetFinalizers() {
		if f != finalizerName {
			remaining = append(remaining, f)
		}
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"finalizers": remaining}})
	if err != nil {
		return err
	}
	_, err = res.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func contains(values []string, wanted string) bool {
	for _, v := range values {
		if v == wanted {
			return true
		}
	}
	return false
}

func unstructuredString(obj map[string]any, path ...string) (string, bool, error) {
	v, ok, err := unstructured.NestedString(obj, path...)
	return v, ok, err
}
