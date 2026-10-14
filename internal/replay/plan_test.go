package replay

import (
	"testing"

	"chaosreplay/internal/fingerprint"
	"chaosreplay/internal/scenario"
)

func TestVerifyRecordRejectsTamperedPlanHash(t *testing.T) {
	s := scenario.Scenario{APIVersion: "chaosreplay/v1", ID: "x", Seed: 1, Target: scenario.Target{Kind: "docker", Name: "x"}, Steps: []scenario.Step{{ID: "kill", Fault: scenario.Fault{Type: "process_kill"}}}}
	p, err := Build(s)
	if err != nil {
		t.Fatal(err)
	}
	env := fingerprint.Capture()
	r := Record{Plan: p, PlanHash: "tampered", Environment: env, EnvironmentHash: fingerprint.Hash(env)}
	if err := VerifyRecord(s, r); err == nil {
		t.Fatal("tampered plan hash was accepted")
	}
}
