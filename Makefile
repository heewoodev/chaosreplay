test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

build:
	go build ./cmd/chaosreplay ./cmd/chaosreplay-agent ./cmd/chaosreplay-controller

linux:
	GOOS=linux GOARCH=amd64 go build ./cmd/chaosreplay ./cmd/chaosreplay-agent ./cmd/chaosreplay-controller

manifests:
	kubectl kustomize deploy/base >/dev/null
	kubectl kustomize deploy/overlays/enterprise >/dev/null

ci: test race vet manifests
