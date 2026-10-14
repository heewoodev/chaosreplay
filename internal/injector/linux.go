//go:build linux

package injector

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"chaosreplay/internal/scenario"
)

// Linux uses small, explicit adapters. Every command is argv-based and checked.
// No shell is involved, so scenario values cannot become shell syntax.
type Linux struct {
	mu        sync.Mutex
	active    map[string]scenario.Fault
	previous  map[string][]byte
	applied   map[string][]byte
	statePath string
}

func NewLinux() *Linux {
	return &Linux{active: map[string]scenario.Fault{}, previous: map[string][]byte{}, applied: map[string][]byte{}}
}

func AttachPodTarget(f scenario.Fault, podUID string) (scenario.Fault, error) {
	if podUID == "" {
		return f, fmt.Errorf("pod UID is required")
	}
	pid, err := ResolvePodUID(podUID)
	if err != nil {
		return f, err
	}
	if f.Params == nil {
		f.Params = map[string]any{}
	}
	f.Params["namespacePid"] = pid
	if f.Type == "disk_io_limit" {
		cgroupPath, err := ResolvePodCgroup(pid)
		if err != nil {
			return f, err
		}
		f.Params["cgroupPath"] = cgroupPath
		f.Params["resolvedTarget"] = true
	}
	return f, nil
}

func ResolvePodUID(uid string) (int64, error) {
	variants := []string{strings.ToLower(uid), strings.ToLower(strings.ReplaceAll(uid, "-", "_")), strings.ToLower(strings.ReplaceAll(uid, "-", ""))}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		pid, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cgroup"))
		if err != nil {
			continue
		}
		text := strings.ToLower(string(b))
		normalized := strings.ReplaceAll(text, "-", "_")
		for _, variant := range variants {
			if strings.Contains(text, variant) || strings.Contains(normalized, variant) {
				return pid, nil
			}
		}
	}
	return 0, fmt.Errorf("no host process found for pod UID %q", uid)
}

func ResolvePodCgroup(pid int64) (string, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.FormatInt(pid, 10), "cgroup"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "0::") {
			path := strings.TrimSpace(strings.TrimPrefix(line, "0::"))
			if path == "" {
				return "", fmt.Errorf("pod process %d resolves to the cgroup root", pid)
			}
			resolved := filepath.Join("/sys/fs/cgroup", path)
			if resolved == "/sys/fs/cgroup" || !strings.HasPrefix(resolved, "/sys/fs/cgroup"+string(os.PathSeparator)) {
				return "", fmt.Errorf("pod process %d resolved to an unsafe cgroup path", pid)
			}
			return resolved, nil
		}
	}
	return "", fmt.Errorf("pod process %d has no cgroup v2 path", pid)
}

type journal struct {
	Active   map[string]scenario.Fault `json:"active"`
	Previous map[string][]byte         `json:"previous,omitempty"`
	Applied  map[string][]byte         `json:"applied,omitempty"`
}

func NewLinuxWithState(path string) (*Linux, error) {
	l := &Linux{active: map[string]scenario.Fault{}, previous: map[string][]byte{}, applied: map[string][]byte{}, statePath: path}
	if err := l.recoverState(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Linux) recoverState() error {
	b, err := os.ReadFile(l.statePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var j journal
	if err := json.Unmarshal(b, &j); err != nil {
		return fmt.Errorf("decode injector journal: %w", err)
	}
	for id, f := range j.Active {
		switch f.Type {
		case "network_latency", "packet_loss":
			dev, e := paramString(f, "interface")
			if e != nil {
				return e
			}
			name, prefix, e := tcCommand(f)
			if e != nil {
				return e
			}
			if e := deleteNetem(context.Background(), name, prefix, dev); e != nil {
				return fmt.Errorf("recover %s: %w", id, e)
			}
		case "disk_io_limit":
			if old, exists := j.Previous[id]; exists {
				path, e := paramString(f, "cgroupPath")
				if e != nil {
					return e
				}
				desired := j.Applied[id]
				if len(desired) == 0 {
					desired, e = desiredIO(f, old)
					if e != nil {
						return e
					}
				}
				if e := restoreIO(filepath.Join(path, "io.max"), old, desired); e != nil {
					return fmt.Errorf("recover %s: %w", id, e)
				}
			}
		}
	}
	return os.Remove(l.statePath)
}

func (l *Linux) persist() error {
	if l.statePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(l.statePath), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(journal{Active: l.active, Previous: l.previous, Applied: l.applied})
	if err != nil {
		return err
	}
	tmp := l.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, l.statePath)
}

func (l *Linux) Apply(ctx context.Context, id string, f scenario.Fault) (Applied, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.active[id]; ok {
		return Applied{}, fmt.Errorf("fault %q already active", id)
	}
	var err error
	switch f.Type {
	case "network_latency", "packet_loss":
		err = l.applyNet(ctx, f)
	case "process_kill":
		err = l.kill(f)
	case "disk_io_limit":
		err = l.applyIO(id, f)
	default:
		err = fmt.Errorf("unsupported fault type %q", f.Type)
	}
	if err != nil {
		return Applied{}, err
	}
	l.active[id] = f
	if err := l.persist(); err != nil {
		delete(l.active, id)
		if f.Type == "network_latency" || f.Type == "packet_loss" {
			if dev, e := paramString(f, "interface"); e == nil {
				if name, prefix, e2 := tcCommand(f); e2 == nil {
					_ = deleteNetem(ctx, name, prefix, dev)
				}
			}
		}
		if f.Type == "disk_io_limit" {
			if path, e := paramString(f, "cgroupPath"); e == nil {
				if old, ok := l.previous[id]; ok {
					_ = restoreIO(filepath.Join(path, "io.max"), old, l.applied[id])
				}
			}
		}
		delete(l.previous, id)
		delete(l.applied, id)
		return Applied{}, fmt.Errorf("persist injector journal: %w", err)
	}
	return Applied{ID: id, Adapter: "linux/v1"}, nil
}

func (l *Linux) Revert(ctx context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.active[id]
	if !ok {
		// Revert is idempotent so controller retries and the agent TTL
		// watchdog can safely race with each other.
		return nil
	}
	var err error
	if f.Type == "network_latency" || f.Type == "packet_loss" {
		dev, e := paramString(f, "interface")
		if e != nil {
			return e
		}
		name, prefix, e := tcCommand(f)
		if e != nil {
			return e
		}
		err = deleteNetem(ctx, name, prefix, dev)
	}
	if f.Type == "disk_io_limit" {
		path, e := paramString(f, "cgroupPath")
		if e != nil {
			return e
		}
		if old, exists := l.previous[id]; exists {
			err = restoreIO(filepath.Join(path, "io.max"), old, l.applied[id])
			if err == nil {
				delete(l.previous, id)
				delete(l.applied, id)
			}
		}
	}
	if err == nil {
		delete(l.active, id)
		delete(l.previous, id)
		delete(l.applied, id)
		err = l.persist()
	}
	return err
}

func (l *Linux) applyNet(ctx context.Context, f scenario.Fault) error {
	dev, err := paramString(f, "interface")
	if err != nil {
		return err
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`).MatchString(dev) {
		return fmt.Errorf("invalid network interface")
	}
	name, prefix, err := tcCommand(f)
	if err != nil {
		return err
	}
	if existing, err := runOutput(ctx, name, append(prefix, "qdisc", "show", "dev", dev)...); err != nil {
		return err
	} else if strings.TrimSpace(existing) != "" && !strings.Contains(existing, "qdisc noqueue") {
		return fmt.Errorf("refusing to replace existing qdisc on %s", dev)
	}
	args := []string{"qdisc", "replace", "dev", dev, "root", "netem"}
	if f.Type == "network_latency" {
		delay, e := paramInt(f, "delayMs")
		if e != nil {
			return e
		}
		if delay < 0 {
			return fmt.Errorf("delayMs must be non-negative")
		}
		args = append(args, "delay", strconv.FormatInt(delay, 10)+"ms")
		if jitter, ok := f.Params["jitterMs"]; ok {
			j, e := intValue(jitter)
			if e != nil || j < 0 {
				return fmt.Errorf("invalid jitterMs")
			}
			args = append(args, strconv.FormatInt(j, 10)+"ms")
		}
	}
	if f.Type == "packet_loss" {
		loss, e := paramNumber(f, "percent")
		if e != nil || loss < 0 || loss > 100 {
			return fmt.Errorf("percent must be between 0 and 100")
		}
		args = append(args, "loss", fmt.Sprintf("%g%%", loss))
	}
	return run(ctx, name, append(prefix, args...)...)
}

func deleteNetem(ctx context.Context, name string, prefix []string, dev string) error {
	current, err := runOutput(ctx, name, append(prefix, "qdisc", "show", "dev", dev)...)
	if err != nil {
		return err
	}
	if !strings.Contains(current, "qdisc netem") {
		return fmt.Errorf("refusing to remove qdisc on %s: current qdisc is not chaosreplay netem", dev)
	}
	return run(ctx, name, append(prefix, "qdisc", "del", "dev", dev, "root")...)
}

func tcCommand(f scenario.Fault) (string, []string, error) {
	if raw, ok := f.Params["namespacePid"]; ok {
		pid, err := intValue(raw)
		if err != nil || pid < 2 {
			return "", nil, fmt.Errorf("invalid namespacePid")
		}
		return "nsenter", []string{"-t", strconv.FormatInt(pid, 10), "-n", "--", "tc"}, nil
	}
	return "tc", nil, nil
}

func (l *Linux) kill(f scenario.Fault) error {
	pid, err := paramInt(f, "pid")
	if err != nil || pid < 1 {
		return fmt.Errorf("process_kill requires pid >= 1")
	}
	sig := "TERM"
	if raw, ok := f.Params["signal"].(string); ok && raw != "" {
		sig = strings.TrimPrefix(strings.ToUpper(raw), "SIG")
	}
	if sig != "TERM" && sig != "KILL" && sig != "INT" {
		return fmt.Errorf("signal must be SIGTERM, SIGKILL or SIGINT")
	}
	if namespacePID, ok := f.Params["namespacePid"]; ok {
		n, e := intValue(namespacePID)
		if e != nil || n < 2 {
			return fmt.Errorf("invalid namespacePid")
		}
		// Keep the agent mount namespace so the executable is independent of
		// the target image (minimal/distroless images often have no kill binary).
		// Only the PID namespace is entered; /usr/bin/kill is supplied by the
		// privileged Agent image.
		return run(context.Background(), "nsenter", "-t", strconv.FormatInt(n, 10), "-p", "--", "/usr/bin/kill", "-"+sig, strconv.FormatInt(pid, 10))
	}
	if pid == 1 {
		return fmt.Errorf("refusing to kill host PID 1 without a Pod namespace")
	}
	return run(context.Background(), "kill", "-"+sig, strconv.FormatInt(pid, 10))
}

func (l *Linux) applyIO(id string, f scenario.Fault) error {
	path, err := paramString(f, "cgroupPath")
	if err != nil {
		return err
	}
	major, e := paramInt(f, "major")
	if e != nil {
		return e
	}
	minor, e := paramInt(f, "minor")
	if e != nil {
		return e
	}
	bps, e := paramInt(f, "readBps")
	if e != nil || bps < 1 {
		return fmt.Errorf("readBps must be positive")
	}
	path = filepath.Clean(path)
	const localRoot = "/sys/fs/cgroup/chaosreplay"
	resolved, _ := f.Params["resolvedTarget"].(bool)
	if !resolved && path != localRoot && !strings.HasPrefix(path, localRoot+string(os.PathSeparator)) {
		return fmt.Errorf("cgroupPath must be under %s or resolved from a Pod target", localRoot)
	}
	ioPath := filepath.Join(path, "io.max")
	old, err := os.ReadFile(ioPath)
	if err != nil {
		return fmt.Errorf("read existing io.max: %w", err)
	}
	desired, err := desiredIOValues(old, major, minor, bps)
	if err != nil {
		return err
	}
	l.previous[id] = old
	l.applied[id] = desired
	if err := os.WriteFile(ioPath, desired, 0600); err != nil {
		delete(l.previous, id)
		delete(l.applied, id)
		return err
	}
	return nil
}

func desiredIO(f scenario.Fault, old []byte) ([]byte, error) {
	major, err := paramInt(f, "major")
	if err != nil {
		return nil, err
	}
	minor, err := paramInt(f, "minor")
	if err != nil {
		return nil, err
	}
	bps, err := paramInt(f, "readBps")
	if err != nil || bps < 1 {
		return nil, fmt.Errorf("readBps must be positive")
	}
	return desiredIOValues(old, major, minor, bps)
}

func desiredIOValues(old []byte, major, minor, bps int64) ([]byte, error) {
	key := fmt.Sprintf("%d:%d", major, minor)
	line := fmt.Sprintf("%s rbps=%d", key, bps)
	lines := strings.Split(strings.TrimSuffix(string(old), "\n"), "\n")
	found := false
	kept := make([]string, 0, len(lines)+1)
	for _, candidate := range lines {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		fields := strings.Fields(candidate)
		if len(fields) > 0 && fields[0] == key {
			kept = append(kept, line)
			found = true
			continue
		}
		kept = append(kept, candidate)
	}
	if !found {
		kept = append(kept, line)
	}
	return []byte(strings.Join(kept, "\n") + "\n"), nil
}

func restoreIO(path string, previous, applied []byte) error {
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(applied) > 0 && strings.TrimSpace(string(current)) != strings.TrimSpace(string(applied)) {
		return fmt.Errorf("refusing to restore %s: cgroup io.max changed outside chaosreplay", path)
	}
	return os.WriteFile(path, previous, 0600)
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func paramString(f scenario.Fault, key string) (string, error) {
	v, ok := f.Params[key]
	s, good := v.(string)
	if !ok || !good || s == "" {
		return "", fmt.Errorf("%s requires %s", f.Type, key)
	}
	return s, nil
}
func paramInt(f scenario.Fault, key string) (int64, error) {
	v, ok := f.Params[key]
	if !ok {
		return 0, fmt.Errorf("%s requires %s", f.Type, key)
	}
	n, err := intValue(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return n, nil
}
func paramNumber(f scenario.Fault, key string) (float64, error) {
	v, ok := f.Params[key]
	if !ok {
		return 0, fmt.Errorf("%s requires %s", f.Type, key)
	}
	return numberValue(v)
}
func intValue(v any) (int64, error) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n >= float64(1<<63) || n < -float64(1<<63) {
			return 0, fmt.Errorf("not integer")
		}
		return int64(n), nil
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	default:
		return 0, fmt.Errorf("not integer")
	}
}
func numberValue(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, fmt.Errorf("not finite")
		}
		return n, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	default:
		return 0, fmt.Errorf("not number")
	}
}
