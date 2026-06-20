# tmm-stat-exporter sidecar — self-contained multi-stage build so the same
# Dockerfile serves local `make exporter-image` and CI multi-arch buildx
# (docker buildx --platform linux/amd64,linux/arm64 honours TARGETARCH).
#
#   docker build -f cmd/tmm-stat-exporter/Dockerfile -t tmm-stat-exporter:dev .
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /tmm-stat-exporter ./cmd/tmm-stat-exporter

FROM scratch
COPY --from=build /tmm-stat-exporter /tmm-stat-exporter
USER 65532:65532
ENTRYPOINT ["/tmm-stat-exporter"]
