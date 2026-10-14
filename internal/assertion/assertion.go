package assertion

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"chaosreplay/internal/scenario"
)

type Result struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// Check evaluates the deterministic, transport-level assertions supported by
// the core. Prometheus scalar expressions intentionally accept only a small
// grammar; unsupported expressions fail closed instead of being guessed.
func Check(ctx context.Context, a scenario.Assertion, client *http.Client) Result {
	switch a.Type {
	case "http":
		return checkHTTP(ctx, a, client)
	case "prometheus":
		return checkProm(ctx, a, client)
	default:
		return Result{Name: a.Name, Detail: "unsupported assertion type"}
	}
}

func checkHTTP(ctx context.Context, a scenario.Assertion, client *http.Client) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return Result{Name: a.Name, Detail: err.Error()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{Name: a.Name, Detail: err.Error()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	want := a.ExpectCode
	if want == 0 {
		want = http.StatusOK
	}
	pass := resp.StatusCode == want
	return Result{Name: a.Name, Passed: pass, Detail: fmt.Sprintf("status=%d expected=%d", resp.StatusCode, want)}
}

func checkProm(ctx context.Context, a scenario.Assertion, client *http.Client) Result {
	u, err := url.Parse(a.URL)
	if err != nil {
		return Result{Name: a.Name, Detail: err.Error()}
	}
	query := u.Query()
	query.Set("query", a.Query)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Result{Name: a.Name, Detail: err.Error()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{Name: a.Name, Detail: err.Error()}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{Name: a.Name, Detail: err.Error()}
	}
	value, err := prometheusScalar(string(b))
	if err != nil {
		return Result{Name: a.Name, Detail: err.Error()}
	}
	pass, err := compare(value, a.Expression)
	if err != nil {
		return Result{Name: a.Name, Detail: err.Error()}
	}
	return Result{Name: a.Name, Passed: pass, Detail: fmt.Sprintf("value=%g expression=%s", value, a.Expression)}
}

func prometheusScalar(body string) (float64, error) {
	var api struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &api) == nil && api.Status != "" {
		if api.Status != "success" {
			return 0, fmt.Errorf("Prometheus API status=%s", api.Status)
		}
		if len(api.Data.Result) == 0 || len(api.Data.Result[0].Value) < 2 {
			return 0, fmt.Errorf("Prometheus query returned no scalar")
		}
		var raw string
		if err := json.Unmarshal(api.Data.Result[0].Value[1], &raw); err == nil {
			return strconv.ParseFloat(raw, 64)
		}
		var number float64
		if err := json.Unmarshal(api.Data.Result[0].Value[1], &number); err == nil {
			return number, nil
		}
		return 0, fmt.Errorf("Prometheus value is not numeric")
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 {
			return strconv.ParseFloat(fields[len(fields)-1], 64)
		}
	}
	return 0, fmt.Errorf("no Prometheus scalar found")
}
func compare(v float64, expr string) (bool, error) {
	fields := strings.Fields(expr)
	if len(fields) != 2 {
		return false, fmt.Errorf("expression must be '<operator> <number>'")
	}
	n, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return false, err
	}
	switch fields[0] {
	case "<":
		return v < n, nil
	case "<=":
		return v <= n, nil
	case ">":
		return v > n, nil
	case ">=":
		return v >= n, nil
	case "==":
		return v == n, nil
	case "!=":
		return v != n, nil
	default:
		return false, fmt.Errorf("unsupported comparison %q", fields[0])
	}
}
