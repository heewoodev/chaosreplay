package scenario

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"

	"sigs.k8s.io/yaml"
)

type Scenario struct {
	APIVersion string      `json:"apiVersion"`
	ID         string      `json:"id"`
	Seed       int64       `json:"seed"`
	Target     Target      `json:"target"`
	Steps      []Step      `json:"steps"`
	Assertions []Assertion `json:"assertions,omitempty"`
}

type Target struct {
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	Namespace string            `json:"namespace,omitempty"`
	Selector  map[string]string `json:"selector,omitempty"`
}

type Assertion struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	URL        string `json:"url,omitempty"`
	Query      string `json:"query,omitempty"`
	ExpectCode int    `json:"expectCode,omitempty"`
	Expression string `json:"expression,omitempty"`
}

type Step struct {
	ID         string `json:"id"`
	AfterMS    int64  `json:"afterMs"`
	DurationMS int64  `json:"durationMs"`
	Fault      Fault  `json:"fault"`
}

type Fault struct {
	Type   string         `json:"type"`
	Params map[string]any `json:"params"`
}

func Load(path string) (Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Scenario{}, err
	}
	var s Scenario
	if ext := filepath.Ext(path); ext == ".yaml" || ext == ".yml" {
		converted, err := yaml.YAMLToJSON(b)
		if err != nil {
			return Scenario{}, fmt.Errorf("decode scenario yaml: %w", err)
		}
		b = converted
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Scenario{}, fmt.Errorf("decode scenario: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Scenario{}, fmt.Errorf("decode scenario: multiple JSON values")
		}
		return Scenario{}, fmt.Errorf("decode scenario: trailing data: %w", err)
	}
	if err := Validate(s); err != nil {
		return Scenario{}, err
	}
	return s, nil
}

func Validate(s Scenario) error {
	if s.APIVersion != "chaosreplay/v1" {
		return fmt.Errorf("apiVersion must be chaosreplay/v1")
	}
	if s.ID == "" || s.Target.Kind == "" || s.Target.Name == "" {
		return fmt.Errorf("id, target.kind and target.name are required")
	}
	if (s.Target.Kind == "kubernetes" || s.Target.Kind == "pod") && (s.Target.Namespace == "" || len(s.Target.Selector) == 0) {
		return fmt.Errorf("Kubernetes targets require target.namespace and a non-empty target.selector")
	}
	if len(s.Steps) == 0 {
		return fmt.Errorf("at least one step is required")
	}
	for i, a := range s.Assertions {
		if a.Name == "" {
			return fmt.Errorf("assertion %d has no name", i)
		}
		switch a.Type {
		case "http", "prometheus":
		default:
			return fmt.Errorf("assertion %q has unsupported type %q", a.Name, a.Type)
		}
		if a.Type == "http" && a.URL == "" {
			return fmt.Errorf("http assertion %q requires url", a.Name)
		}
		if a.Type == "prometheus" && (a.Query == "" || a.URL == "") {
			return fmt.Errorf("prometheus assertion %q requires url and query", a.Name)
		}
	}
	seen := map[string]bool{}
	type activeFault struct {
		stepID string
		start  int64
		end    int64
		key    string
	}
	active := make([]activeFault, 0, len(s.Steps))
	var elapsed int64
	for i, st := range s.Steps {
		if st.ID == "" || seen[st.ID] {
			return fmt.Errorf("step %d has a missing or duplicate id", i)
		}
		seen[st.ID] = true
		if st.AfterMS < 0 || st.DurationMS < 0 {
			return fmt.Errorf("step %q has negative timing", st.ID)
		}
		elapsed += st.AfterMS
		switch st.Fault.Type {
		case "network_latency":
			if _, err := requiredString(st.Fault, "interface"); err != nil {
				return fmt.Errorf("step %q: %w", st.ID, err)
			}
			if delay, err := requiredInteger(st.Fault, "delayMs"); err != nil || delay < 0 {
				return fmt.Errorf("step %q requires non-negative delayMs", st.ID)
			}
		case "packet_loss":
			if _, err := requiredString(st.Fault, "interface"); err != nil {
				return fmt.Errorf("step %q: %w", st.ID, err)
			}
			loss, err := requiredNumber(st.Fault, "percent")
			if err != nil || loss < 0 || loss > 100 {
				return fmt.Errorf("step %q requires percent between 0 and 100", st.ID)
			}
		case "process_kill":
			pid, err := requiredInteger(st.Fault, "pid")
			if err != nil || pid < 1 {
				return fmt.Errorf("step %q requires pid >= 1", st.ID)
			}
		case "disk_io_limit":
			readBps, err := requiredInteger(st.Fault, "readBps")
			if err != nil || readBps < 1 {
				return fmt.Errorf("step %q requires positive readBps", st.ID)
			}
			if major, err := requiredInteger(st.Fault, "major"); err != nil || major < 0 {
				return fmt.Errorf("step %q requires non-negative major", st.ID)
			}
			if minor, err := requiredInteger(st.Fault, "minor"); err != nil || minor < 0 {
				return fmt.Errorf("step %q requires non-negative minor", st.ID)
			}
		default:
			return fmt.Errorf("step %q has unsupported fault type %q", st.ID, st.Fault.Type)
		}
		key, err := faultResourceKey(st.Fault)
		if err != nil {
			return fmt.Errorf("step %q: %w", st.ID, err)
		}
		current := activeFault{stepID: st.ID, start: elapsed, end: elapsed + st.DurationMS, key: key}
		for _, previous := range active {
			if previous.key != current.key {
				continue
			}
			overlaps := current.start < previous.end && previous.start < current.end
			if st.DurationMS == 0 {
				overlaps = current.start >= previous.start && current.start <= previous.end
			} else if previous.end == previous.start {
				overlaps = previous.start >= current.start && previous.start <= current.end
			}
			if overlaps {
				return fmt.Errorf("steps %q and %q overlap on exclusive resource %q", previous.stepID, st.ID, key)
			}
		}
		active = append(active, current)
	}
	return nil
}

func faultResourceKey(f Fault) (string, error) {
	switch f.Type {
	case "network_latency", "packet_loss":
		dev, err := requiredString(f, "interface")
		if err != nil {
			return "", err
		}
		return "network:" + dev, nil
	case "process_kill":
		pid, err := requiredInteger(f, "pid")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("process:%d", int64(pid)), nil
	case "disk_io_limit":
		major, err := requiredInteger(f, "major")
		if err != nil {
			return "", err
		}
		minor, err := requiredInteger(f, "minor")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("disk:%d:%d", int64(major), int64(minor)), nil
	default:
		return "", fmt.Errorf("unsupported fault type %q", f.Type)
	}
}

func requiredString(f Fault, key string) (string, error) {
	value, ok := f.Params[key].(string)
	if !ok || value == "" {
		return "", fmt.Errorf("requires %s", key)
	}
	return value, nil
}
func requiredNumber(f Fault, key string) (float64, error) {
	value, ok := f.Params[key]
	if !ok {
		return 0, fmt.Errorf("requires %s", key)
	}
	switch n := value.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, fmt.Errorf("%s must be finite", key)
		}
		return n, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	default:
		return 0, fmt.Errorf("%s must be numeric", key)
	}
}
func requiredInteger(f Fault, key string) (float64, error) {
	n, err := requiredNumber(f, key)
	if err != nil || math.Trunc(n) != n || n >= float64(1<<63) || n < -float64(1<<63) {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return n, nil
}

func Canonical(s Scenario) ([]byte, error) {
	// Struct field order is fixed; sort map keys recursively through JSON encoding's ordering.
	return json.Marshal(s)
}

func Hash(s Scenario) (string, error) {
	b, err := Canonical(s)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func StableFaultTypes(s Scenario) []string {
	seen := map[string]bool{}
	for _, st := range s.Steps {
		seen[st.Fault.Type] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
