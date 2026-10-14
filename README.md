# chaosreplay

A Go-based Chaos Engineering platform for defining, executing, and verifying reproducible failures in distributed systems.

chaosreplay focuses on reproducible experiments, not only fault injection:

- Generates the same execution plan from the same scenario.
- Validates scenarios and exclusive-resource conflicts before execution.
- Records the plan, environment fingerprint, operations, and observations.
- Fails verification when the plan or execution environment differs.
- Limits blast radius with bounded TTLs, reverse-order rollback, and a durable journal.
- Integrates with Kubernetes through a Controller, CRD, and privileged Node Agent.

Supported fault types are network latency, packet loss, process termination, and disk I/O limits. Linux adapters use tc netem, PID namespaces, cgroup v2 io.max, and optional eBPF observation.

> Warning: chaosreplay is a privileged tool that intentionally causes production-like failures. Validate it in an isolated staging environment on a real Linux kernel before using it in a shared or production cluster. Disk I/O support depends on the node kernel, cgroup configuration, runtime, and block device.

## Contents

- [Architecture](#architecture)
- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Scenario format](#scenario-format)
- [Fault types](#fault-types)
- [Reproducibility and records](#reproducibility-and-records)
- [Kubernetes](#kubernetes)
- [Node Agent API](#node-agent-api)
- [eBPF observation](#ebpf-observation)
- [Operations and security](#operations-and-security)
- [Development and verification](#development-and-verification)
- [Scope and limitations](#scope-and-limitations)
- [Documentation](#documentation)
- [License](#license)

## Architecture

    +------------------+       deterministic plan       +------------------+
    | chaosreplay CLI  | ------------------------------> | Kubernetes       |
    | validate/plan/   |                                 | Controller       |
    | record/verify    |                                 +--------+---------+
    +------------------+                                          |
                                                                  | HTTP/mTLS
                                                                  v
                                                        +---------+--------+
                                                        | Node Agent       |
                                                        | tc / namespace   |
                                                        | cgroup / eBPF    |
                                                        +---------+--------+
                                                                  |
                                                                  v
                                                        ConfigMap evidence

| Component | Responsibility |
|---|---|
| chaosreplay | Scenario loading, validation, deterministic planning, record creation and verification |
| chaosreplay-controller | Watches the ChaosExperiment CRD and coordinates Node Agents |
| chaosreplay-agent | Injects and reverts faults on a node and persists active state |
| Linux injector | Fault adapters based on tc, nsenter, and cgroup v2 |
| eBPF observer | Observation-only sched_process_exit tracepoint observer |

The Controller stores execution evidence in a ConfigMap named chaosreplay-[12-char-plan-hash]-[run-id]. The full plan hash remains in record.json, and the CRD status field recordName points to the ConfigMap.

## Requirements

### Local CLI

- Go 1.26 or newer
- Windows, macOS, or Linux for planning and record verification
- A Linux Agent for actual fault injection

### Linux Agent

- Linux kernel
- tc from iproute2
- nsenter for targeting a Pod namespace
- NET_ADMIN for network faults
- Appropriate privileged permissions for process and cgroup operations
- cgroup v2 io controller and a valid block-device major/minor pair for disk I/O limits

### Kubernetes

- Kubernetes 1.26 or newer is recommended
- Permissions to install the CRD, Controller, and Node Agent
- Agent access to hostPID, hostNetwork, and host cgroups
- BTF, eBPF permissions, debugfs, and tracefs for eBPF observation
- mTLS certificates and signed immutable image digests for production

## Quick start

### 1. Validate and plan

    go run ./cmd/chaosreplay validate examples/basic.yaml
    go run ./cmd/chaosreplay plan examples/basic.yaml

validate checks the schema, required fields, fault parameters, timing, and overlapping exclusive resources. plan prints a sorted deterministic execution plan and its plan hash.

### 2. Create and verify a record

    go run ./cmd/chaosreplay record examples/basic.yaml run.json
    go run ./cmd/chaosreplay verify examples/basic.yaml run.json

record creates a plan-only record; it does not inject a fault. Kubernetes execution records additionally contain Agent operations and assertion results.

### 3. Build Linux binaries

    make build
    make linux

On Windows PowerShell:

    go build ./cmd/chaosreplay ./cmd/chaosreplay-agent ./cmd/chaosreplay-controller
    $env:GOOS="linux"; $env:GOARCH="amd64"; go build ./cmd/chaosreplay ./cmd/chaosreplay-agent ./cmd/chaosreplay-controller

## Scenario format

Scenarios may be written in JSON or YAML. YAML is converted to JSON and then processed by the same strict decoder. Unknown fields and multiple JSON documents are rejected.

    apiVersion: chaosreplay/v1
    id: checkout-network-delay
    seed: 42
    target:
      kind: kubernetes
      name: checkout
      namespace: chaosreplay-test
      selector:
        app: checkout
    steps:
      - id: add-latency
        afterMs: 1000
        durationMs: 5000
        fault:
          type: network_latency
          params:
            interface: eth0
            delayMs: 200
            jitterMs: 0
    assertions:
      - name: checkout-healthy
        type: http
        url: http://checkout.chaosreplay-test.svc.cluster.local/
        expectCode: 200

| Field | Description |
|---|---|
| apiVersion | Must be chaosreplay/v1 |
| id | Human-readable scenario identifier |
| seed | Seed for deterministic random choices |
| target | Target kind, name, namespace, and label selector |
| steps[].afterMs | Relative offset from the previous planned event |
| steps[].durationMs | Fault duration; 0 is used for immediate operations |
| steps[].fault | Fault type and type-specific parameters |
| assertions | HTTP or Prometheus verification conditions |

A Kubernetes target requires a namespace and a non-empty selector. The Controller selects running Pods, verifies each Pod UID and Node address, and refuses to guess when either cannot be resolved safely.

## Fault types

### Network latency

    fault:
      type: network_latency
      params:
        interface: eth0
        delayMs: 200
        jitterMs: 20

Uses tc netem inside the target Pod network namespace. Delay must be non-negative, and overlapping faults on the same interface are rejected.

### Packet loss

    fault:
      type: packet_loss
      params:
        interface: eth0
        percent: 10

Loss must be between 0 and 100 percent. The Agent only restores qdisc state that it owns. If an external operator changes the qdisc, the Agent fails closed and reports a rollback error instead of overwriting the change.

### Process termination

    fault:
      type: process_kill
      params:
        pid: 1
        signal: SIGTERM

Terminates the process inside the target Pod PID namespace. PID values are interpreted in that namespace. Terminating PID 1 may restart the Pod, so always inspect assertion and rollback results.

### Disk I/O limit

    fault:
      type: disk_io_limit
      params:
        major: 8
        minor: 0
        readBps: 1048576

Uses cgroup v2 io.max. Major/minor values vary by node and container runtime and must be preflighted. Docker Desktop, Kind, and some VMs may return EINVAL or EIO for unsupported devices; such an experiment must fail rather than be reported as successful.

## Reproducibility and records

Reproducibility is based on fixing and recording:

1. scenarioHash, computed from the canonical scenario JSON using SHA-256.
2. A deterministic plan generated from offsets and seed.
3. Planned fault events, ordering, resource keys, and planHash.
4. OS, architecture, kernel, cgroup mode, BTF/eBPF/tracefs, I/O capability, adapter version, and operation results.

Verification separates:

- Plan equality: did the same scenario produce the same plan?
- Environment equality: does the execution fingerprint match?
- Result verification: did operations and assertions satisfy expectations?

A scenario hash alone cannot make a physical execution identical. Kernel scheduling, hardware, container runtime, network paths, Pod images, and node capabilities must also be controlled and recorded. chaosreplay detects environmental differences instead of silently treating them as an identical replay.

See [REPRODUCIBILITY.md](REPRODUCIBILITY.md) for the record contract.

## Kubernetes

### Development cluster

    kubectl apply -f deploy/rbac.yaml
    kubectl apply -f deploy/crd.yaml
    kubectl apply -f deploy/agent-daemonset.yaml
    kubectl apply -f deploy/controller-deployment.yaml

Or use Kustomize:

    kubectl apply -k deploy/base

The development manifests use an unauthenticated HTTP Agent and :dev images. Do not use them unchanged in a shared or production cluster.

### Run an experiment

    kubectl create namespace chaosreplay-test
    kubectl apply -f integration/kind/workload.yaml
    kubectl apply -f integration/kind/experiment.yaml
    kubectl -n chaosreplay-test get chaosexperiment checkout-network-delay -w

Inspect status and evidence:

    kubectl -n chaosreplay-test get chaosexperiment checkout-network-delay -o jsonpath='{.status.phase}{"\n"}{.status.message}{"\n"}{.status.recordName}{"\n"}'
    kubectl -n chaosreplay-test get configmap <recordName> -o jsonpath='{.data.record.json}' > record.json

The Controller records apply/revert operations and observations for each execution target. CI should inspect record.json operations, assertion results, and rollback errors instead of checking only Succeeded status.

### Enterprise overlay

    kubectl kustomize deploy/overlays/enterprise
    kubectl apply -k deploy/overlays/enterprise

Replace these placeholders before deployment:

- registry.example.com image names
- sha256:REPLACE_WITH_SIGNED_DIGEST
- chaosreplay-agent-tls Secret
- chaosreplay-agent-client-ca Secret
- chaosreplay-controller-client-tls Secret

The Enterprise overlay provides two Controller replicas, Kubernetes Lease leader election, a PDB, mTLS Secret mounts, and immutable image digest configuration. Inject certificates and private keys through an enterprise Secret Manager or Secret delivery system; never commit private keys.

### Controller options

    -kubeconfig                         kubeconfig path; empty means in-cluster
    -interval                           reconcile interval (default 2s)
    -max-concurrent                     maximum simultaneous experiments (default 1)
    -leader-election                    enable Lease leader election (default true)
    -leader-election-namespace          Lease namespace
    -leader-election-id                 Lease name
    -agent-scheme                       http or https
    -agent-port                         Agent port (default 8080)
    -agent-ca                           Agent CA PEM for HTTPS
    -agent-client-cert                  Controller client certificate PEM
    -agent-client-key                   Controller client private key PEM
    -health-listen                      health endpoint address (default :8081)

Controller health endpoints are GET /healthz and GET /readyz.

## Node Agent API

The Agent listens on :8080 by default. In production, provide all three TLS flags to enable TLS 1.3 mutual TLS.

    chaosreplay-agent \
      -listen :8080 \
      -tls-cert /etc/chaosreplay/tls/tls.crt \
      -tls-key /etc/chaosreplay/tls/tls.key \
      -client-ca /etc/chaosreplay/client-ca/ca.crt \
      -state-file /var/lib/chaosreplay/active.json

| Method | Path | Description |
|---|---|---|
| GET | /healthz | Agent status and optional eBPF observer status |
| POST | /v1/apply | Apply a fault: id, fault, optional ttlMs and targetPodUID |
| POST | /v1/revert | Revert the fault identified by id |

The Agent rejects unknown JSON fields and limits request bodies. TTL must be between 0 and 24 hours; expired faults are reverted automatically. On restart, the Agent reads /var/lib/chaosreplay/active.json to recover active faults.

Do not replace targetPodUID with an arbitrary namespace when calling the Agent directly. The Agent fails if it cannot resolve the Pod UID through host cgroups.

## eBPF observation

Enable the observation-only sched_process_exit tracepoint with:

    chaosreplay-agent -observe-process-exits

Example /healthz response:

    {
      "status": "ok",
      "ebpfProcessExitObserver": true,
      "observedProcessExits": 12
    }

The counter is host-wide and is not automatically Pod-scoped. Correlate it with Pod UID, experiment time windows, and operation IDs before using it as evidence for a specific Pod. eBPF is an observation layer, not a fault injection adapter.

The Agent manifests mount host /sys/kernel/debug and /sys/kernel/tracing. Verify BTF, eBPF attach permissions, debugfs/tracefs, Pod Security and Admission policies, kernel lockdown, and seccomp behavior on production nodes.

## Operations and security

Before enabling experiments in a shared cluster:

- Pin signed Controller and Agent images by immutable digest.
- Run Agents only on approved nodes and namespaces.
- Protect the Agent with mTLS and deliver private keys through a Secret Manager.
- Keep Controller RBAC limited to CRD status, Pod reads, Node reads, and leader-election Leases.
- Give the Agent no Kubernetes API permissions.
- Require a bounded TTL, explicit selectors, and an approval process for every experiment.
- Start with max-concurrent=1 and increase it only after staging validation.
- Enable leader election whenever more than one Controller replica is deployed.
- Alert on Failed experiments, rollback errors, environment mismatches, and Agent heartbeat loss.
- Preserve the ConfigMap named by status.recordName as CI evidence.
- Verify that external qdisc or io.max changes cause a safe failure instead of being overwritten.
- Validate network, process, and disk faults independently on a real Linux staging kernel.

Admission policy should restrict namespaces and selectors, fault types and maximum values, process PID allowlists, disk device allowlists, concurrency, execution windows, and approval tickets.

See [docs/OPERATIONS.md](docs/OPERATIONS.md) for the enterprise checklist and [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for system data flow.

## Development and verification

    make test       # unit and integration package tests
    make race       # race detector
    make vet        # go vet
    make build      # build all three binaries
    make linux      # linux/amd64 build
    make manifests  # render Kustomize manifests
    make ci         # test + race + vet + manifests

Equivalent Go commands:

    go test ./...
    go test -race ./...
    go vet ./...
    go build ./cmd/chaosreplay ./cmd/chaosreplay-agent ./cmd/chaosreplay-controller

Kind smoke-test inputs:

- integration/kind/workload.yaml
- integration/kind/experiment.yaml
- integration/kind/experiment-process-kill.yaml
- .github/workflows/ci.yml

The CI smoke test builds and loads both images into Kind, deploys the CRD, Controller, Agent, and workload, and verifies network fault execution and an HTTP assertion. eBPF, tracefs/debugfs, and disk io.max support remain environment-dependent.

## Scope and limitations

The repository provides the core reproducibility and safety boundaries. The following require environment-specific work or external platform integration:

- Disk I/O does not support every container-runtime and device combination; node-level capability preflight is required.
- The eBPF process-exit counter is host-wide and does not automatically attribute events to Pods.
- Prometheus can be used as an assertion target, but the repository does not install a metrics stack, Grafana dashboards, or Alertmanager rules.
- Certificate issuance and rotation, image signing, and image verification require an external PKI, Secret Manager, and image policy.
- Large-scale multi-node, long-duration, and disaster-recovery exercises require organization-specific staging tests.

In this project, reproducible means that the scenario, plan, environment fingerprint, and operation record are fixed and mismatches are detected. It does not mean that hardware, kernel scheduling, or external network behavior becomes physically deterministic.

## Documentation

- [REPRODUCIBILITY.md](REPRODUCIBILITY.md) — hashes, plans, records, environment fingerprints, and verification
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — components and data flow
- [docs/OPERATIONS.md](docs/OPERATIONS.md) — enterprise operations checklist
- [deploy/](deploy) — CRD, RBAC, Controller, Agent, and Kustomize overlays
- [examples/](examples) — CLI JSON/YAML scenarios
- [integration/kind/](integration/kind) — Kind smoke-test workload and experiments

## License

This project is licensed under the [MIT License](LICENSE).

