package replay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"chaosreplay/internal/fingerprint"
	"chaosreplay/internal/scenario"
)

type Event struct {
	Sequence int            `json:"sequence"`
	StepID   string         `json:"stepId"`
	StartMS  int64          `json:"startMs"`
	EndMS    int64          `json:"endMs"`
	Fault    scenario.Fault `json:"fault"`
}

type Plan struct {
	ScenarioID   string  `json:"scenarioId"`
	ScenarioHash string  `json:"scenarioHash"`
	Seed         int64   `json:"seed"`
	Events       []Event `json:"events"`
}

type Record struct {
	Plan            Plan              `json:"plan"`
	PlanHash        string            `json:"planHash"`
	RunID           string            `json:"runId"`
	Outcome         string            `json:"outcome"`
	RecordedAt      time.Time         `json:"recordedAt"`
	Adapter         string            `json:"adapter"`
	Environment     fingerprint.Host  `json:"environment"`
	EnvironmentHash string            `json:"environmentHash"`
	Operations      []Operation       `json:"operations,omitempty"`
	Assertions      []AssertionResult `json:"assertions,omitempty"`
	Observed        []string          `json:"observed,omitempty"`
}

type Operation struct {
	ID        string    `json:"id"`
	StepID    string    `json:"stepId"`
	Target    string    `json:"target"`
	Action    string    `json:"action"`
	Adapter   string    `json:"adapter"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type AssertionResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

func Build(s scenario.Scenario) (Plan, error) {
	h, err := scenario.Hash(s)
	if err != nil {
		return Plan{}, err
	}
	var now int64
	events := make([]Event, 0, len(s.Steps))
	for i, st := range s.Steps {
		now += st.AfterMS
		events = append(events, Event{Sequence: i + 1, StepID: st.ID, StartMS: now, EndMS: now + st.DurationMS, Fault: st.Fault})
	}
	p := Plan{ScenarioID: s.ID, ScenarioHash: h, Seed: s.Seed, Events: events}
	return p, nil
}

func HashPlan(p Plan) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func Verify(s scenario.Scenario, p Plan) error {
	expected, err := Build(s)
	if err != nil {
		return err
	}
	gotHash, err := HashPlan(p)
	if err != nil {
		return err
	}
	expectedHash, err := HashPlan(expected)
	if err != nil {
		return err
	}
	if gotHash != expectedHash {
		return fmt.Errorf("plan mismatch: expected %s, got %s", expectedHash, gotHash)
	}
	return nil
}

func VerifyRecord(s scenario.Scenario, r Record) error {
	if err := Verify(s, r.Plan); err != nil {
		return err
	}
	expectedPlanHash, err := HashPlan(r.Plan)
	if err != nil {
		return err
	}
	if r.PlanHash != expectedPlanHash {
		return fmt.Errorf("record has an invalid plan hash")
	}
	if r.EnvironmentHash == "" || r.EnvironmentHash != fingerprint.Hash(r.Environment) {
		return fmt.Errorf("record has an invalid environment fingerprint")
	}
	current := fingerprint.Capture()
	if fingerprint.Hash(current) != r.EnvironmentHash {
		return fmt.Errorf("environment mismatch: recorded=%s current=%s", r.EnvironmentHash, fingerprint.Hash(current))
	}
	return nil
}
