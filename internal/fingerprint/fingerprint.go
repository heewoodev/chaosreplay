package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"runtime"
	"strings"
)

// Host is intentionally small and stable. Volatile values such as hostname,
// PID and wall clock are excluded because they would make replay impossible.
type Host struct {
	OS                string `json:"os"`
	Architecture      string `json:"architecture"`
	KernelRelease     string `json:"kernelRelease"`
	CgroupMode        string `json:"cgroupMode"`
	CgroupIOAvailable bool   `json:"cgroupIOAvailable"`
	BTFHash           string `json:"btfHash"`
	TraceFSAvailable  bool   `json:"traceFSAvailable"`
	EBPFAvailable     bool   `json:"ebpfAvailable"`
}

func Capture() Host {
	h := Host{OS: runtime.GOOS, Architecture: runtime.GOARCH}
	h.KernelRelease = readTrim("/proc/sys/kernel/osrelease")
	if controllers, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		h.CgroupMode = "v2"
		for _, controller := range strings.Fields(string(controllers)) {
			if controller == "io" {
				h.CgroupIOAvailable = true
				break
			}
		}
	} else if _, err := os.Stat("/sys/fs/cgroup"); err == nil {
		h.CgroupMode = "unknown"
	} else {
		h.CgroupMode = "unavailable"
	}
	if b, err := os.ReadFile("/sys/kernel/btf/vmlinux"); err == nil {
		sum := sha256.Sum256(b)
		h.BTFHash = hex.EncodeToString(sum[:])
	}
	if _, err := os.Stat("/sys/kernel/tracing/events/sched/sched_process_exit/id"); err == nil {
		h.TraceFSAvailable = true
	}
	if h.BTFHash != "" && h.TraceFSAvailable {
		h.EBPFAvailable = true
	}
	return h
}

func Hash(h Host) string {
	b, _ := json.Marshal(h)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "unavailable"
	}
	return strings.TrimSpace(string(b))
}
