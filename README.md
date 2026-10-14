# chaosreplay

재현 가능한 분산 시스템 장애를 정의하고, 실행하고, 검증하는 Go 기반 Chaos Engineering 플랫폼입니다.

chaosreplay의 핵심은 단순한 장애 주입이 아닙니다.

- 동일한 시나리오에서 동일한 실행 계획을 생성합니다.
- 실행 전에 시나리오와 exclusive resource 충돌을 검증합니다.
- 실행 기록에 계획, 환경 지문, 작업 결과, 관측값을 남깁니다.
- 계획이나 실행 환경이 달라지면 검증을 실패시킵니다.
- TTL, reverse-order rollback, durable journal로 장애가 남을 위험을 줄입니다.
- Kubernetes에서는 Controller가 대상 Pod를 선택하고 Node Agent에 작업을 전달합니다.

현재 제공하는 장애 유형은 네트워크 지연, 패킷 손실, 프로세스 종료, 디스크 I/O 제한입니다. Linux의 tc netem, PID namespace, cgroup v2 io.max, 선택적 eBPF 관측을 사용합니다.

> 주의: 이 프로젝트는 운영 장애를 실제로 발생시키는 privileged 도구입니다. 운영 클러스터에 적용하기 전에 격리된 staging 환경과 실제 Linux 커널에서 검증하십시오. 특히 디스크 I/O 제한은 노드 커널, cgroup 설정, block device에 따라 지원 여부가 달라집니다.

## 목차

- [구성](#구성)
- [요구 사항](#요구-사항)
- [빠른 시작](#빠른-시작)
- [시나리오 형식](#시나리오-형식)
- [장애 유형](#장애-유형)
- [실행 기록과 재현성](#실행-기록과-재현성)
- [Kubernetes 실행](#kubernetes-실행)
- [Node Agent API](#node-agent-api)
- [eBPF 관측](#ebpf-관측)
- [운영 및 보안](#운영-및-보안)
- [검증과 개발 명령](#검증과-개발-명령)
- [현재 범위와 제한](#현재-범위와-제한)
- [문서](#문서)

## 구성

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

| 구성 요소 | 역할 |
|---|---|
| chaosreplay | 시나리오 로드, 정적 검증, 결정적 계획 생성, 실행 기록 생성·검증 |
| chaosreplay-controller | ChaosExperiment CRD를 감시하고 대상 Pod의 Node Agent를 조정 |
| chaosreplay-agent | 노드에서 장애를 주입·복구하고 durable journal에 상태 저장 |
| Linux injector | tc, nsenter, cgroup v2를 이용한 장애 adapter |
| eBPF observer | sched_process_exit tracepoint 기반 프로세스 종료 관측. 장애 주입용이 아님 |

Controller는 실행 증거를 chaosreplay-[12-char-plan-hash]-[run-id] 형식의 ConfigMap에 저장하고, CRD status의 recordName으로 해당 ConfigMap을 가리킵니다. 전체 plan hash는 record.json 안에 보존됩니다.

## 요구 사항

### 로컬 CLI

- Go 1.26 이상
- Windows, macOS, Linux에서 계획 생성과 기록 검증 가능
- 실제 Linux 장애 주입은 Linux Agent 필요

### Linux Agent

- Linux kernel
- tc (iproute2)
- Pod namespace 대상 주입 시 nsenter
- 네트워크 장애 시 NET_ADMIN 권한
- 프로세스 종료·cgroup 제어 시 적절한 privileged 권한
- 디스크 I/O 제한 시 cgroup v2의 io controller와 유효한 block device major/minor

### Kubernetes

- Kubernetes 1.26 이상 권장
- CRD, Controller, Node Agent를 배포할 권한
- Agent는 hostPID, hostNetwork, host cgroup 접근이 필요
- eBPF 관측 시 BTF, eBPF 권한, debugfs/tracefs 필요
- 운영에서는 mTLS 인증서와 서명된 immutable image digest 필요

## 빠른 시작

### 1. 시나리오 검증 및 계획 생성

    go run ./cmd/chaosreplay validate examples/basic.yaml
    go run ./cmd/chaosreplay plan examples/basic.yaml

validate는 스키마, 필수 필드, fault parameter, timing, 동일 exclusive resource의 겹침을 검증합니다. plan은 정렬된 실행 이벤트와 plan hash를 JSON으로 출력합니다.

### 2. 실행 기록 생성 및 검증

    go run ./cmd/chaosreplay record examples/basic.yaml run.json
    go run ./cmd/chaosreplay verify examples/basic.yaml run.json

record는 실제 장애를 주입하지 않는 plan-only 기록입니다. Kubernetes Controller가 생성하는 운영 실행 기록에는 Agent 작업과 assertion 결과가 포함됩니다.

### 3. Linux 바이너리 빌드

    make build
    make linux

Windows PowerShell에서는 다음처럼 빌드할 수 있습니다.

    go build ./cmd/chaosreplay ./cmd/chaosreplay-agent ./cmd/chaosreplay-controller
    $env:GOOS="linux"; $env:GOARCH="amd64"; go build ./cmd/chaosreplay ./cmd/chaosreplay-agent ./cmd/chaosreplay-controller

## 시나리오 형식

JSON 또는 YAML을 사용할 수 있습니다. YAML은 JSON으로 변환한 뒤 동일한 strict decoder로 검증합니다. 알 수 없는 필드와 여러 JSON 문서는 거부됩니다.

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

| 필드 | 설명 |
|---|---|
| apiVersion | 반드시 chaosreplay/v1 |
| id | 사람이 읽는 시나리오 식별자 |
| seed | 결정적 랜덤 선택이 필요한 경우 사용할 seed |
| target | 대상 종류, 이름, namespace, label selector |
| steps[].afterMs | 이전 계획 기준 상대 offset |
| steps[].durationMs | fault 유지 시간. 0은 즉시 작업 |
| steps[].fault | 장애 유형과 유형별 parameter |
| assertions | HTTP 또는 Prometheus 검증 조건 |

Kubernetes target은 namespace와 비어 있지 않은 selector가 필수입니다. Controller는 selector에 맞는 실행 중 Pod를 찾고, 각 Pod의 UID와 Node 주소를 확인한 뒤 작업을 전달합니다. Pod UID 또는 Node를 안전하게 확인할 수 없으면 추측하여 실행하지 않습니다.

## 장애 유형

### 네트워크 지연

    fault:
      type: network_latency
      params:
        interface: eth0
        delayMs: 200
        jitterMs: 20

대상 Pod network namespace의 tc netem을 사용합니다. 지연은 0 이상이어야 하며, 같은 interface에 동시에 겹치는 fault는 거부됩니다.

### 패킷 손실

    fault:
      type: packet_loss
      params:
        interface: eth0
        percent: 10

손실률은 0~100 사이입니다. Agent는 자신이 관리한 netem 상태만 복구하며, 외부에서 qdisc가 바뀌었으면 덮어쓰지 않고 rollback 오류를 반환합니다.

### 프로세스 종료

    fault:
      type: process_kill
      params:
        pid: 1
        signal: SIGTERM

대상 Pod의 PID namespace 안에서 프로세스를 종료합니다. PID는 대상 namespace 기준으로 해석됩니다. workload의 PID 1을 종료하면 Pod가 재시작될 수 있으므로 assertion과 rollback 결과를 함께 확인하십시오.

### 디스크 I/O 제한

    fault:
      type: disk_io_limit
      params:
        major: 8
        minor: 0
        readBps: 1048576

cgroup v2 io.max를 사용합니다. major/minor는 노드와 container runtime마다 다르므로 환경에 맞게 preflight해야 합니다. Docker Desktop, Kind, 일부 VM에서는 해당 device에 io.max를 적용할 수 없어 EINVAL 또는 EIO가 발생할 수 있습니다. 이 경우 실험을 성공으로 간주하지 않고 실패시켜야 합니다.

## 실행 기록과 재현성

재현성은 다음을 함께 고정하는 방식으로 설계되어 있습니다.

1. 정규화된 시나리오 JSON의 SHA-256으로 scenarioHash를 생성합니다.
2. 시나리오의 offset과 seed로 동일한 deterministic plan을 생성합니다.
3. plan에는 fault event, 실행 순서, resource key, planHash가 포함됩니다.
4. record에는 계획뿐 아니라 OS, architecture, kernel, cgroup mode, BTF/eBPF/tracefs 및 I/O capability 같은 환경 지문과 작업 결과를 저장합니다.

검증은 다음을 구분합니다.

- 계획 동일성: 같은 시나리오가 같은 plan을 만들었는가
- 환경 동일성: 실행 환경 지문이 기대한 환경과 일치하는가
- 결과 검증: assertion과 Agent 작업 결과가 기대 조건을 만족하는가

완전히 동일한 결과를 보장하려면 같은 시나리오 hash만으로는 충분하지 않습니다. 커널, container runtime, 네트워크 경로, 대상 Pod 이미지와 노드 capability도 함께 고정하고 record를 보관해야 합니다. 환경이 달라진 실행은 자동으로 동일한 재현으로 간주하지 않습니다.

상세한 record 계약은 [REPRODUCIBILITY.md](REPRODUCIBILITY.md)를 참고하십시오.

## Kubernetes 실행

### 개발 클러스터

    kubectl apply -f deploy/rbac.yaml
    kubectl apply -f deploy/crd.yaml
    kubectl apply -f deploy/agent-daemonset.yaml
    kubectl apply -f deploy/controller-deployment.yaml

또는 Kustomize를 사용할 수 있습니다.

    kubectl apply -k deploy/base

개발 manifest는 http Agent와 :dev image를 사용합니다. 공유 또는 운영 클러스터에서는 그대로 사용하지 마십시오.

### 실험 적용

    kubectl create namespace chaosreplay-test
    kubectl apply -f integration/kind/workload.yaml
    kubectl apply -f integration/kind/experiment.yaml
    kubectl -n chaosreplay-test get chaosexperiment checkout-network-delay -w

완료 후 status와 record를 확인합니다.

    kubectl -n chaosreplay-test get chaosexperiment checkout-network-delay -o jsonpath='{.status.phase}{"\n"}{.status.message}{"\n"}{.status.recordName}{"\n"}'
    kubectl -n chaosreplay-test get configmap <recordName> -o jsonpath='{.data.record.json}' > record.json

Controller는 assertion을 포함한 모든 실행 대상에 대해 apply/revert 작업과 관측값을 record에 남깁니다. CI에서는 Succeeded만 확인하지 말고 record.json의 operations, assertion 결과, rollback 오류도 검증하십시오.

### Enterprise overlay

    kubectl kustomize deploy/overlays/enterprise
    kubectl apply -k deploy/overlays/enterprise

배포 전에 다음 placeholder를 교체해야 합니다.

- registry.example.com/... image 이름
- sha256:REPLACE_WITH_SIGNED_DIGEST
- chaosreplay-agent-tls Secret
- chaosreplay-agent-client-ca Secret
- chaosreplay-controller-client-tls Secret

Enterprise overlay는 Controller 2 replicas, Kubernetes Lease leader election, PDB, mTLS Secret mount, immutable image digest를 위한 구조를 제공합니다. 인증서와 private key는 저장소에 커밋하지 말고 외부 Secret Manager 또는 조직의 Secret delivery 시스템으로 주입하십시오.

### Controller 옵션

    -kubeconfig                         kubeconfig 경로. 비어 있으면 in-cluster
    -interval                           reconcile 주기 (기본 2s)
    -max-concurrent                     동시에 실행할 최대 실험 수 (기본 1)
    -leader-election                    Lease leader election 사용 여부 (기본 true)
    -leader-election-namespace          Lease namespace
    -leader-election-id                 Lease 이름
    -agent-scheme                       http 또는 https
    -agent-port                         Agent port (기본 8080)
    -agent-ca                           HTTPS Agent CA PEM
    -agent-client-cert                  HTTPS client certificate PEM
    -agent-client-key                   HTTPS client private key PEM
    -health-listen                      /healthz, /readyz 주소 (기본 :8081)

Controller health endpoint는 GET /healthz와 GET /readyz입니다.

## Node Agent API

Agent는 기본적으로 :8080에서 실행합니다. 운영에서는 세 가지 TLS flag를 모두 지정해 TLS 1.3 mutual TLS를 활성화하십시오.

    chaosreplay-agent \
      -listen :8080 \
      -tls-cert /etc/chaosreplay/tls/tls.crt \
      -tls-key /etc/chaosreplay/tls/tls.key \
      -client-ca /etc/chaosreplay/client-ca/ca.crt \
      -state-file /var/lib/chaosreplay/active.json

| Method | Path | 설명 |
|---|---|---|
| GET | /healthz | Agent 상태와 선택적 eBPF 관측 상태 |
| POST | /v1/apply | fault 적용. id, fault, 선택적 ttlMs, targetPodUID |
| POST | /v1/revert | id에 해당하는 fault 복구 |

Agent는 요청 JSON의 알 수 없는 필드를 거부하고 body 크기를 제한합니다. TTL은 0 이상 24시간 이하이며, 만료 시 자동 revert합니다. 재시작 후에는 /var/lib/chaosreplay/active.json journal을 읽어 active fault를 복구합니다.

직접 Agent를 호출하는 경우에도 targetPodUID를 임의의 namespace로 대체하지 마십시오. Agent는 Pod UID를 host cgroup에서 해석하지 못하면 실패합니다.

## eBPF 관측

다음 flag로 observation-only sched_process_exit tracepoint를 활성화합니다.

    chaosreplay-agent -observe-process-exits

/healthz 응답 예:

    {
      "status": "ok",
      "ebpfProcessExitObserver": true,
      "observedProcessExits": 12
    }

이 카운터는 host-wide 관측값입니다. 그 자체로 특정 Pod의 프로세스 종료 증거가 아니므로, Pod UID·실험 시간창·작업 ID와의 별도 correlation이 필요합니다. eBPF는 장애 주입 adapter가 아니라 관측 계층입니다.

Agent Pod에는 eBPF 사용을 위해 host /sys/kernel/debug와 /sys/kernel/tracing이 mount되어 있습니다. 운영 노드에서 BTF, eBPF attach 권한, debugfs/tracefs, Pod Security/Admission 정책, kernel lockdown/seccomp 정책을 사전 확인하십시오.

## 운영 및 보안

운영 도입 전 다음을 필수 확인하십시오.

- Controller와 Agent image를 서명하고 immutable digest로 고정
- Agent를 승인된 node와 namespace에만 배치
- Agent는 mTLS로만 노출하고 인증서 private key는 Secret Manager에서 주입
- Controller RBAC를 CRD status, Pod read, Node read, leader Lease로 최소화
- Agent에는 Kubernetes API 권한을 부여하지 않음
- 모든 실험에 bounded TTL, 명시적인 selector, 승인 절차 적용
- 처음에는 max-concurrent=1로 운영하고 staging 검증 후에만 늘림
- Controller를 여러 개 실행할 때 leader election을 반드시 활성화
- Failed, rollback error, environment mismatch, Agent heartbeat loss를 alerting
- record ConfigMap을 CI evidence로 보관
- 외부 operator가 관리 qdisc 또는 io.max를 바꾸면 Agent가 덮어쓰지 않고 실패하는지 확인
- 실제 Linux kernel에서 네트워크·프로세스·디스크 fault를 각각 staging 검증

실험 admission policy에는 허용된 namespace와 selector, fault type와 최대치, process PID allowlist, disk device allowlist, 동시 실행 수, 실행 시간 window, 승인 ticket을 포함하는 것을 권장합니다.

자세한 운영 절차는 [docs/OPERATIONS.md](docs/OPERATIONS.md), 구성과 데이터 흐름은 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)를 참고하십시오.

## 검증과 개발 명령

    make test       # 단위/통합 패키지 테스트
    make race       # race detector
    make vet        # go vet
    make build      # 로컬 바이너리 3종
    make linux      # linux/amd64 빌드
    make manifests  # Kustomize 렌더링
    make ci         # test + race + vet + manifests

Go 명령으로 실행하면 다음과 같습니다.

    go test ./...
    go test -race ./...
    go vet ./...
    go build ./cmd/chaosreplay ./cmd/chaosreplay-agent ./cmd/chaosreplay-controller

Kind 기반 smoke test의 입력은 다음 경로에 있습니다.

- integration/kind/workload.yaml
- integration/kind/experiment.yaml
- integration/kind/experiment-process-kill.yaml
- .github/workflows/ci.yml

CI smoke test는 image를 빌드하고 Kind에 로드한 뒤 CRD, Controller, Agent, workload를 배포하고 네트워크 장애와 assertion 결과를 확인합니다. eBPF, tracefs/debugfs, disk io.max 지원은 실행 환경에 따라 다르므로 CI 결과를 모든 운영 노드의 capability 증명으로 해석해서는 안 됩니다.

## 현재 범위와 제한

현재 구현은 핵심 재현성·안전성 경계를 제공합니다. 다음 항목은 환경별 추가 작업 또는 운영 플랫폼 연동이 필요합니다.

- 디스크 I/O는 모든 container runtime과 device 조합을 지원하지 않습니다. 노드별 capability preflight가 필요합니다.
- eBPF 프로세스 종료 카운터는 host-wide이며 자동 Pod attribution을 제공하지 않습니다.
- Prometheus는 assertion 대상으로 사용할 수 있지만, 자체 metric 수집기·Grafana dashboard·Alertmanager rule을 설치하지 않습니다.
- 인증서 발급·순환과 image 서명·검증은 외부 PKI, Secret Manager, image policy와 연동해야 합니다.
- 대규모 다중 노드, 장시간 실행, 장애 복구 훈련은 조직의 staging 규모에 맞춘 별도 부하 검증이 필요합니다.

이 저장소의 재현 가능은 계획·시나리오·환경·작업 기록을 고정하고 불일치를 검출한다는 의미입니다. 하드웨어, 커널 스케줄링, 외부 네트워크와 같은 비결정적 요소까지 물리적으로 동일하게 만드는 것을 의미하지 않습니다.

## 문서

- [REPRODUCIBILITY.md](REPRODUCIBILITY.md) — hash, plan, record, 환경 지문과 검증 규칙
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — 구성 요소와 데이터 흐름
- [docs/OPERATIONS.md](docs/OPERATIONS.md) — 엔터프라이즈 운영 체크리스트
- [deploy/](deploy) — CRD, RBAC, Controller, Agent, Kustomize overlay
- [examples/](examples) — CLI용 JSON/YAML 시나리오
- [integration/kind/](integration/kind) — Kind smoke test용 workload와 실험

## 라이선스

현재 저장소에는 별도 라이선스 파일이 포함되어 있지 않습니다. 공개 배포 전 라이선스와 보안 책임 범위를 명시하십시오.

