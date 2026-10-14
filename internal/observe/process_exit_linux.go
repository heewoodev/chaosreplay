//go:build linux

package observe

import (
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

// processExitObserver attaches to sched_process_exit and increments a single
// kernel map value. It intentionally observes all host processes; target
// scoping remains the controller/agent responsibility and is recorded in the
// scenario evidence. This avoids silently claiming that an unfiltered probe
// is pod-scoped.
type processExitObserver struct {
	counts *ebpf.Map
	prog   *ebpf.Program
	link   link.Link
}

func NewProcessExitObserver() (ProcessExitObserver, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("remove eBPF memlock limit: %w", err)
	}
	counts, err := ebpf.NewMap(&ebpf.MapSpec{
		Name:       "chaosreplay_exit_counts",
		Type:       ebpf.Array,
		KeySize:    4,
		ValueSize:  8,
		MaxEntries: 1,
	})
	if err != nil {
		return nil, fmt.Errorf("create eBPF counter map: %w", err)
	}
	prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:    "chaosreplay_process_exit",
		Type:    ebpf.TracePoint,
		License: "GPL",
		Instructions: asm.Instructions{
			// key = 0 at FP[-4]
			asm.StoreImm(asm.RFP, -4, 0, asm.Word),
			asm.Mov.Reg(asm.R2, asm.RFP),
			asm.Add.Imm(asm.R2, -4),
			asm.LoadMapPtr(asm.R1, counts.FD()),
			asm.FnMapLookupElem.Call(),
			asm.JEq.Imm(asm.R0, 0, "exit"),
			asm.Mov.Imm(asm.R1, 1),
			asm.StoreXAdd(asm.R0, asm.R1, asm.DWord),
			asm.Mov.Imm(asm.R0, 0).WithSymbol("exit"),
			asm.Return(),
		},
	})
	if err != nil {
		_ = counts.Close()
		return nil, fmt.Errorf("load eBPF process-exit program: %w", err)
	}
	tp, err := link.Tracepoint("sched", "sched_process_exit", prog, nil)
	if err != nil {
		_ = prog.Close()
		_ = counts.Close()
		return nil, fmt.Errorf("attach sched_process_exit tracepoint: %w", err)
	}
	return &processExitObserver{counts: counts, prog: prog, link: tp}, nil
}

func (o *processExitObserver) Snapshot() (uint64, error) {
	var count uint64
	key := uint32(0)
	if err := o.counts.Lookup(&key, &count); err != nil {
		return 0, fmt.Errorf("read eBPF process-exit counter: %w", err)
	}
	return count, nil
}

func (o *processExitObserver) Close() error {
	var first error
	if o.link != nil {
		if err := o.link.Close(); err != nil && first == nil {
			first = err
		}
	}
	if o.prog != nil {
		if err := o.prog.Close(); err != nil && first == nil {
			first = err
		}
	}
	if o.counts != nil {
		if err := o.counts.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
