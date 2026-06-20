# tmm-stat-webhook — self-contained multi-stage build (serves local
# `make webhook-image` and CI multi-arch buildx via TARGETARCH).
#
#   docker build -f cmd/tmm-stat-webhook/Dockerfile -t tmm-stat-webhook:dev .
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /tmm-stat-webhook ./cmd/tmm-stat-webhook

FROM scratch
COPY --from=build /tmm-stat-webhook /tmm-stat-webhook
USER 65532:65532
ENTRYPOINT ["/tmm-stat-webhook"]
