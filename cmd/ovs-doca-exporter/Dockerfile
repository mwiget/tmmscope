# ovs-doca-exporter — self-contained multi-stage build so the same
# Dockerfile serves local `make exporter-image` and CI multi-arch buildx
# (docker buildx --platform linux/amd64,linux/arm64 honours TARGETARCH).
#
#   docker build -f cmd/ovs-doca-exporter/Dockerfile -t ovs-doca-exporter:dev .
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /ovs-doca-exporter ./cmd/ovs-doca-exporter

FROM scratch
COPY --from=build /ovs-doca-exporter /ovs-doca-exporter
# Runs as root: the ovs-vswitchd unixctl socket under /var/run/openvswitch is
# root-owned, and this only ever issues read-only dpctl/dump-flows calls.
USER 0:0
ENTRYPOINT ["/ovs-doca-exporter"]
