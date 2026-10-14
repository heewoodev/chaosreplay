package scenario

import (
	"math"
	"os"
	"testing"
)

func TestHashStable(t *testing.T) {
	s := Scenario{APIVersion: "chaosreplay/v1", ID: "x", Seed: 1, Target: Target{Kind: "docker", Name: "a"}, Steps: []Step{{ID: "a", Fault: Fault{Type: "process_kill"}}}}
	a, err := Hash(s)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Hash(s)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("hash changed: %s != %s", a, b)
	}
}

func TestLoadYAML(t *testing.T) {
	s, err := Load("../../examples/basic.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "checkout-network-and-process" || len(s.Steps) != 2 {
		t.Fatalf("unexpected scenario: %+v", s)
	}
}

func TestLoadRejectsUnknownJSONField(t *testing.T) {
	path := t.TempDir() + string(os.PathSeparator) + "bad.json"
	content := []byte(`{"apiVersion":"chaosreplay/v1","id":"x","seed":1,"target":{"kind":"docker","name":"x"},"steps":[{"id":"s","fault":{"type":"process_kill"}}],"unknown":true}`)
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown field was accepted")
	}
}

func TestLoadRejectsTrailingJSON(t *testing.T) {
	path := t.TempDir() + string(os.PathSeparator) + "trailing.json"
	content := []byte(`{"apiVersion":"chaosreplay/v1","id":"x","target":{"kind":"docker","name":"x"},"steps":[{"id":"s","fault":{"type":"process_kill","params":{"pid":2}}}]} {"extra":true}`)
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("trailing JSON was accepted")
	}
}

func TestValidateRejectsInvalidFaultParams(t *testing.T) {
	s := Scenario{APIVersion: "chaosreplay/v1", ID: "bad", Target: Target{Kind: "kubernetes", Name: "x"}, Steps: []Step{{ID: "delay", Fault: Fault{Type: "network_latency", Params: map[string]any{"interface": "eth0", "delayMs": -1}}}}}
	if err := Validate(s); err == nil {
		t.Fatal("invalid delay was accepted")
	}
}

func TestValidateRejectsOverlappingExclusiveFaults(t *testing.T) {
	s := Scenario{APIVersion: "chaosreplay/v1", ID: "overlap", Target: Target{Kind: "kubernetes", Name: "x"}, Steps: []Step{
		{ID: "one", DurationMS: 100, Fault: Fault{Type: "network_latency", Params: map[string]any{"interface": "eth0", "delayMs": 10}}},
		{ID: "two", AfterMS: 10, DurationMS: 10, Fault: Fault{Type: "packet_loss", Params: map[string]any{"interface": "eth0", "percent": 5}}},
	}}
	if err := Validate(s); err == nil {
		t.Fatal("expected conflicting network faults to be rejected")
	}
}

func TestValidateRequiresScopedKubernetesTarget(t *testing.T) {
	s := Scenario{APIVersion: "chaosreplay/v1", ID: "scope", Target: Target{Kind: "kubernetes", Name: "x"}, Steps: []Step{{ID: "kill", Fault: Fault{Type: "process_kill", Params: map[string]any{"pid": 2}}}}}
	if err := Validate(s); err == nil {
		t.Fatal("expected an unscoped Kubernetes target to be rejected")
	}
}

func TestValidateRejectsNonFiniteNumbers(t *testing.T) {
	s := Scenario{APIVersion: "chaosreplay/v1", ID: "finite", Target: Target{Kind: "docker", Name: "x"}, Steps: []Step{{ID: "loss", Fault: Fault{Type: "packet_loss", Params: map[string]any{"interface": "eth0", "percent": math.NaN()}}}}}
	if err := Validate(s); err == nil {
		t.Fatal("non-finite number was accepted")
	}
}
