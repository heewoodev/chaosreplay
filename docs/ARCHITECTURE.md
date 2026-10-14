# Runtime architecture

`chaosreplay` is split into four trust boundaries:

1. **Control plane**: validates the scenario, calculates the plan hash, resolves Kubernetes targets and coordinates lifecycle.
2. **Node agent**: the only component allowed to touch a Linux network namespace, cgroup or process. It runs with host PID/network visibility, resolves the selected Pod UID through host cgroups, enters the Pod network/PID namespace with `nsenter`, accepts one fault at a time by ID, persists an active-fault journal, has a TTL watchdog, and must revert it before the experiment can finish.
3. **Observation plane**: HTTP and Prometheus provide deterministic assertions; the optional eBPF process-exit counter provides node-level observation evidence. Assertions fail closed when transport, parsing or expression evaluation fails.
4. **Evidence store**: the CLI writes JSON records to files; the Kubernetes controller writes one `chaosreplay-<plan-hash>-<run-id>` ConfigMap per execution containing scenario/plan/environment hashes, outcome, assertion results, and per-target apply/revert operations. High-volume telemetry remains outside the Kubernetes API.

Event execution uses the plan's absolute offsets: overlapping events run concurrently, while events with the same start offset use sequence barriers for deterministic apply order. A failure cancels the event group and triggers reverse cleanup.

The Kubernetes integration should use a `ChaosExperiment` CRD and a Go controller. The CRD is the desired state; the controller owns a finalizer and writes only `.status`. The node agent should be deployed as a tightly scoped DaemonSet and addressed through authenticated, node-local communication. Do not put high-volume telemetry in the Kubernetes API; store execution evidence in object storage and put only references and verdicts in CRD status.

No operation is successful on a partial result. Apply, observe, revert and persist are all required. On process termination or controller restart, the recovery loop must scan active IDs and revert expired leases before accepting a new experiment.
