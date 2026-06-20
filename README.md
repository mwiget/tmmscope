# tmmscope

**Standalone real-time F5 TMM telemetry — Prometheus + Grafana, plus one-command
sidecar injection.** A single Go binary that stands up a local Prometheus
(remote_write receiver) and Grafana (pre-provisioned *TMM Real-Time* dashboard),
and injects the `tmm-stat-exporter` sidecar into any TMM cluster so its live
`tmstat` counters stream straight to that dashboard.

It runs entirely on its own — no `bnk-forge`, no operator, no cloud. Point it at
a cluster, watch CPU / throughput / connections / per-pool-member load move in
real time during a test or an incident.

```
  ┌─ tmmscope up ──────────────┐         ┌─ tmmscope inject ─────────────┐
  │  Prometheus  :9491 (RW recv)│  ◀────  │  tmm-stat-exporter sidecar    │
  │  Grafana     :3000 (dash)   │ remote_ │  reads /var/tmstat, pushes    │
  └─────────────────────────────┘ write   │  every 2s (cluster=<name>)    │
        ^ host, any arch                   └───────────────────────────────┘
                                              in f5-tmm pod, any arch
```

## Why it exists

TMM real-time monitoring and the `bnk-forge` control plane are different
concerns. Bundling a full Prometheus + Grafana stack into `bnk-forge` bloated a
troubleshooting tool with an observability platform. `tmmscope` is that
observability slice, extracted: it works standalone, and the cluster CLIs
(`tmmlitectl`, `ocibnkctl`) can discover and stream to it without depending on
`bnk-forge` being up.

## Install

```bash
go install github.com/mwiget/tmmscope/cmd/tmmscope@latest
# or grab a release binary, or: git clone … && make build && make install
```

Requires `docker` (with compose v2) for the stack and `kubectl` for injection.

## Quickstart

```bash
tmmscope up                        # start Prometheus + Grafana (auto-ports)
tmmscope inject --context calico   # add the exporter to that cluster's f5-tmm
tmmscope open                      # open the TMM Real-Time dashboard
```

## Commands

| Command | What it does |
|---|---|
| `tmmscope up` | Render + start the stack, negotiate ports, write the discovery file |
| `tmmscope down [--purge]` | Stop the stack (`--purge` also drops the data volumes) |
| `tmmscope status` | Running state + active ports |
| `tmmscope endpoint [--json]` | Print the discovery endpoints (the producer contract) |
| `tmmscope inject` | Add the `tmm-stat-exporter` sidecar to a cluster's `f5-tmm` |
| `tmmscope eject` | Remove the sidecar |
| `tmmscope open` | Open the dashboard in a browser |

## Ports & the discovery contract

`tmmscope` prefers the well-known ports **9491** (Prometheus remote_write
receiver) and **3000** (Grafana). If either is taken it walks upward to the next
free port and **persists** the choice, so a running stack stays stable across
re-runs. Because the ports can move, producers must *discover* them rather than
hard-code `9491`.

Two equivalent discovery sources:

1. **Invoke** `tmmscope endpoint --json` (if the binary is on `$PATH`).
2. **Read** the file `$XDG_CONFIG_HOME/tmmscope/endpoints.json`
   (default `~/.config/tmmscope/endpoints.json`), written on every `up`.

Both yield the same document:

```json
{
  "running": true,
  "prometheus": {
    "port": 9491,
    "url": "http://localhost:9491",
    "remote_write_url": "http://localhost:9491/api/v1/write",
    "remote_write_path": "/api/v1/write"
  },
  "grafana": {
    "port": 3000,
    "url": "http://localhost:3000",
    "dashboard_url": "http://localhost:3000/d/tmm-realtime"
  },
  "updated_at": "2026-06-20T09:55:43Z"
}
```

The host in the URLs is always `localhost` — a hint for local use. **A producer
running inside a cluster substitutes its own host-gateway IP and keeps the
`port`.** For docker-based k3s clusters that gateway is the bnk-edge bridge
(`192.168.<octet>.1`) or the docker bridge (`172.17.0.1`); the pod reaches the
host's published port there.

## Injection & architecture (Intel + ARM)

`tmmscope inject` adds a locked-down, best-effort sidecar (non-root, read-only
rootfs, no probes) that mounts the existing `f5tmstat` volume read-only and
pushes `remote_write` outbound — TMM hooks *inbound* TCP on its dataplane
interfaces, so the sidecar cannot be scraped; it pushes. It does not touch FLO's
native observer/otel pipeline — it runs alongside.

The exporter ships as a **multi-arch manifest** at
`ghcr.io/mwiget/tmm-stat-exporter` (linux/amd64 + linux/arm64). The target
node's containerd pulls the matching architecture automatically, so **the
tmmscope host's architecture is irrelevant** — an Apple-Silicon laptop can scope
an amd64 cluster and vice versa.

```bash
# default: pull the multi-arch image from ghcr, auto-derive the remote_write URL
tmmscope inject --context calico

# explicit target / endpoint / label
tmmscope inject --kubeconfig ./kubeconfig --namespace default \
  --deployment f5-tmm --cluster calico \
  --remote-write-url http://192.168.98.1:9491/api/v1/write

tmmscope eject --context calico
```

Two cluster shapes:

- **Direct patch** *(implemented)* — for a plain `f5-tmm` Deployment (e.g.
  `tmmlitectl` clusters): a strategic-merge patch adds the sidecar container.
- **Admission webhook** *(planned)* — for operator-managed FLO/BNK pods, where a
  direct patch would be reconciled away; a mutating webhook injects the sidecar
  at pod creation.

### Offline / air-gapped fallback

When a cluster can't pull from ghcr, build and import the image into the nodes,
matching the **cluster's** node architecture (not the build host's):

```bash
make exporter-load EXPORTER_ARCH=arm64 \
  NODES="k3s-calico-server-0 k3s-calico-agent-0 k3s-calico-agent-1"
tmmscope inject --context calico --image tmm-stat-exporter:dev
```

## License

MIT — see [LICENSE](LICENSE). Nothing here is confidential; the TMM dashboard is
free to publish.
