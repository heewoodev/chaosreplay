# Enterprise operation checklist

Before enabling experiments against a shared cluster:

- Use a signed, immutable image digest for both controller and agent.
- Run the agent only on explicitly approved nodes and namespaces.
- Enable mTLS (`--tls-cert`, `--tls-key`, `--client-ca`) and configure the controller with the matching client certificate.
- Store certificates in the enterprise secret manager; never commit private keys.
- Give the controller only the CRD status, Pod read, and Node read RBAC shown in `deploy/rbac.yaml`.
- Give the agent no Kubernetes API permissions. Its privileged access is limited to the node by DaemonSet placement and admission policy.
- Verify the node runtime exposes Pod cgroups in `/proc/<pid>/cgroup`; the agent refuses to guess a target namespace when the Pod UID cannot be resolved.
- Require a target selector and a bounded TTL for every experiment.
- Keep Controller `--max-concurrent=1` by default; raise it only after proving node and service isolation in staging.
- Run at least two Controller replicas only with Kubernetes Lease leader election enabled; the enterprise overlay does this and grants only Lease CRUD permissions.
- Keep the agent watchdog enabled. The controller also performs reverse-order rollback.
- If a node operator changes a managed qdisc or `io.max` while an experiment is
  active, the Agent refuses to overwrite that external change and reports a
  rollback error for manual remediation.
- Alert on `Failed`, rollback errors, environment mismatch, and agent heartbeat loss.
- Monitor controller `/healthz` and `/readyz`; Agent probes use TCP because the
  production Agent listener is fully protected by mTLS.
- Collect the ConfigMap named in `.status.recordName` as the CI evidence artifact before deleting the experiment namespace.
- Verify the record's `operations` array is complete before treating a run as
  auditable; each planned event should have an apply and revert operation (or
  an explicit error/recovery operation).
- Run in staging with a real Linux kernel before production use.
- Enable `--observe-process-exits` only after validating kernel BTF, eBPF
  permissions, debugfs/tracefs mounts, and admission/security policy. The
  probe is observation-only and host-wide, not inherently Pod-scoped. The
  Agent DaemonSet mounts `/sys/kernel/debug` and `/sys/kernel/tracing` from
  the node for this purpose.
- Disk I/O experiments additionally require cgroup v2's `io` controller and a
  node-supported block device major/minor pair. Do not hardcode a device pair
  across runtimes; preflight the target node and reject the experiment when
  `io.max` returns `EINVAL` or `EIO`.

The current local manifest is intentionally a development transport (`http`, `:dev` images). Production deployment must apply an environment-specific overlay that mounts the mTLS Secrets, uses pinned images, and changes the controller flags to `--agent-scheme=https`.

The repository CI Kind job is the reference smoke test. It creates a real cluster, builds both images, deploys the CRD/controller/agent, mounts tracefs/debugfs, enables the observation-only eBPF probe, applies a test workload, injects network delay into the selected Pod namespace, and waits for an HTTP assertion plus successful status.
