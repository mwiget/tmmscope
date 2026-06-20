.PHONY: all build test tidy fmt vet clean exporter-image exporter-buildx exporter-load install

# tmmscope — standalone real-time TMM telemetry (Prometheus + Grafana + sidecar injection).

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
           -X 'github.com/mwiget/tmmscope/internal/version.Version=$(VERSION)' \
           -X 'github.com/mwiget/tmmscope/internal/version.Commit=$(COMMIT)' \
           -X 'github.com/mwiget/tmmscope/internal/version.BuildDate=$(DATE)'

all: build

# The tmmscope CLI (host tool: brings up the stack, injects sidecars).
build:
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/tmmscope ./cmd/tmmscope

install: build
	install -m 0755 bin/tmmscope $(or $(PREFIX),/usr/local)/bin/tmmscope

test:
	go test ./... -count=1

tidy:
	go mod tidy

fmt:
	go fmt ./...

vet:
	go vet ./...

clean:
	rm -rf bin dist

# ── tmm-stat-exporter sidecar image ──────────────────────────────────────────
# The DEFAULT distribution is a multi-arch manifest on ghcr (see exporter-buildx
# / CI). `tmmscope inject` points f5-tmm at that manifest, so each TARGET node
# pulls its own architecture automatically — the tmmscope host's arch is
# irrelevant (an ARM laptop can scope an amd64 cluster and vice versa).
GHCR_IMAGE     ?= ghcr.io/mwiget/tmm-stat-exporter
PLATFORMS      ?= linux/amd64,linux/arm64

# Multi-arch build + push to ghcr (CI uses this on a tag). Requires `docker
# buildx` and a logged-in registry.
exporter-buildx:
	docker buildx build --platform $(PLATFORMS) \
	  -f cmd/tmm-stat-exporter/Dockerfile \
	  -t $(GHCR_IMAGE):$(VERSION) -t $(GHCR_IMAGE):latest \
	  --push .

# ── Local fallback: build + import into k3s nodes (no registry) ───────────────
# Only for clusters that can't pull from ghcr. EXPORTER_ARCH must match the
# TARGET cluster's nodes (which may differ from this build host) or the pod gets
# "exec format error" — Go cross-compiles freely, so set it explicitly.
EXPORTER_IMAGE ?= tmm-stat-exporter:dev
EXPORTER_ARCH  ?= $(shell go env GOARCH)

exporter-image:
	docker build --build-arg TARGETARCH=$(EXPORTER_ARCH) \
	  -f cmd/tmm-stat-exporter/Dockerfile -t $(EXPORTER_IMAGE) .

# Import the locally-built exporter image into k3s nodes. Pass the node
# container names, e.g. `make exporter-load NODES="k3s-calico-server-0 k3s-calico-agent-0 k3s-calico-agent-1"`.
exporter-load: exporter-image
	docker save $(EXPORTER_IMAGE) -o bin/tmm-stat-exporter.tar
	@for n in $(NODES); do \
	  echo "importing $(EXPORTER_IMAGE) into $$n"; \
	  docker cp bin/tmm-stat-exporter.tar $$n:/tmp/img.tar; \
	  docker exec $$n ctr -n k8s.io image import /tmp/img.tar; \
	  docker exec $$n rm -f /tmp/img.tar; \
	done
	@rm -f bin/tmm-stat-exporter.tar
