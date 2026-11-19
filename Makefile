VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/muskats/heraion
UNITTEST ?= helm unittest
PKGS     = $(shell go list ./... | grep -v /charts/)

.PHONY: build test lint image chart-deps chart-test

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/heraion ./cmd/heraion

test:
	go test -race $(PKGS)

lint:
	go vet ./...
	golangci-lint run

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

chart-deps:
	helm dependency build charts/heraion

# -count=1: the Go test cache does not notice chart file changes.
# Without the helm plugin: make chart-test UNITTEST=/path/to/untt
chart-test: chart-deps
	helm lint charts/heraion -f charts/heraion/ci/full-values.yaml
	go test -count=1 ./charts/heraion/
	$(UNITTEST) charts/heraion
