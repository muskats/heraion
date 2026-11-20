VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= docker.io/muskats/heraion

.PHONY: build test lint unit chart-test e2e ci image chart-deps clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/heraion ./cmd/heraion

# The same entry points CI uses; see scripts/ci.sh.
test: unit
unit:
	scripts/test-unit.sh
lint:
	scripts/lint.sh
chart-test:
	scripts/test-chart.sh
e2e:
	scripts/test-e2e.sh
ci:
	scripts/ci.sh

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

chart-deps:
	scripts/chart-deps.sh

clean:
	rm -rf bin build .tools
