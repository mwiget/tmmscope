# AGENTS.md — tmmscope

Standalone real-time F5 TMM telemetry: a Go CLI that runs a local Prometheus +
Grafana stack and injects the `tmm-stat-exporter` sidecar into TMM clusters.
Deliberately self-contained — no `bnk-forge`, no operator dependency.

## Layout

```
cmd/tmmscope/            # the CLI (host tool): up/down/status/endpoint/inject/eject/open
cmd/tmm-stat-exporter/   # the sidecar binary (runs in the f5-tmm pod) + multi-stage Dockerfile
internal/stack/          # compose render, port negotiation, discovery file (endpoints.json)
internal/inject/         # kubectl strategic-merge sidecar injection (direct-patch path)
internal/tmstat/         # tmstat shared-memory segment reader (used by the exporter)
internal/assets/         # go:embed Prometheus + Grafana provisioning + the TMM dashboard
internal/version/        # build metadata
```

## Build / test

```bash
make build            # bin/tmmscope
make test             # go test ./...
make exporter-image   # local single-arch sidecar image (dev/test)
make exporter-buildx  # multi-arch push to ghcr (CI does this on a tag)
```

No external Go deps beyond `github.com/golang/snappy` (the exporter's
remote_write encoder). The CLI shells out to `docker` and `kubectl` rather than
linking client-go — keep it that way unless there's a strong reason.

## Key invariants

- **Discovery contract** (`internal/stack` `Endpoints`): the JSON shape from
  `tmmscope endpoint --json` and `endpoints.json` is what `tmmlitectl` /
  `ocibnkctl` program against. Changing it is a breaking change — update the
  README's schema block in the same commit.
- **Ports are negotiated, not fixed.** Default 9491/3000, walk upward if taken,
  persist the choice. Producers must read the port, never hard-code it.
- **Push, not scrape.** TMM hooks inbound TCP on its dataplane interfaces, so the
  exporter pushes `remote_write` outbound. No probes on the sidecar (a kubelet
  probe to the pod IP can't reach it and would mark the whole tmm pod NotReady).
- **Cross-arch by design.** The host running tmmscope may differ in arch from the
  target cluster. The exporter is a multi-arch ghcr manifest so each node pulls
  its own arch. The local build+import fallback must target the *cluster's* arch.
- **Additive.** Injection runs alongside FLO's native observer/otel pipeline; it
  never modifies or replaces it.
- The sidecar spec in `internal/inject` is intentionally identical to the one
  `tmmlitectl` injects today, so `tmmscope inject` is a drop-in replacement
  (re-injecting an already-instrumented cluster is a no-op).

## Roadmap

1. Receiver (`up`/`down`/`status`/`endpoint`) — **done**
2. Canonical exporter + multi-arch ghcr publish — **done** (publish on first tag)
3. `inject`/`eject` direct-patch — **done**
4. `inject` admission-webhook path for operator-managed FLO/BNK pods — **done**
   (auto-detected via ownerReferences; self-signed cert, no cert-manager)
5. `tmmlitectl` / `ocibnkctl` discover tmmscope (and optionally drop their own
   injection in favor of `tmmscope inject`) — **planned, decision deferred**
