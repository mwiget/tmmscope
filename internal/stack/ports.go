package stack

import (
	"fmt"
	"net"
)

// Default well-known host ports. Documented so producers (tmmlitectl,
// ocibnkctl) know the starting point; the actual port is auto-negotiated and
// must be read back via `tmmscope endpoint --json` or endpoints.json.
const (
	DefaultPrometheusPort = 9491 // remote_write receiver (matches the original bnk-forge stack)
	DefaultGrafanaPort    = 3000 // Grafana UI
	maxPortProbe          = 50   // how far to walk from the default before giving up
)

// preferredPort resolves the port to aim for: an explicit request (flag) wins,
// otherwise a currently-claimed port (keep a running stack stable), otherwise
// the well-known default.
func preferredPort(explicit, claimed, def int) int {
	switch {
	case explicit != 0:
		return explicit
	case claimed != 0:
		return claimed
	default:
		return def
	}
}

// portFree reports whether the TCP port can be bound on all interfaces.
func portFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// pickPort returns preferred if it is free or already claimed by us (claimed
// == the port we recorded last run), otherwise the next free port walking up
// from start. claimed lets `up` be idempotent: a running stack's own port is
// "free" from our point of view even though the bind would fail.
func pickPort(preferred, start, claimed int, ours bool) (int, error) {
	if preferred == claimed && ours {
		return preferred, nil
	}
	if portFree(preferred) {
		return preferred, nil
	}
	for p := start; p < start+maxPortProbe; p++ {
		if p == preferred {
			continue
		}
		if p == claimed && ours {
			return p, nil
		}
		if portFree(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port found in [%d, %d)", start, start+maxPortProbe)
}
