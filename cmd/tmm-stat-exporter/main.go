// Command tmm-stat-exporter publishes F5 tmm's live tmstat counters as
// Prometheus metrics. It runs as a sidecar in the f5-tmm pod and reads the
// tmstat shared-memory segment (/var/tmstat/blade/tmm0) directly via
// internal/tmstat (no tmctl, no cgo).
//
// TMM hooks inbound TCP on its dataplane interfaces (eth0 + net1), so the
// sidecar's listening port is unreachable from outside the pod and cannot be
// scraped. Instead the exporter PUSHES outbound: when -remote-write is set it
// samples tmstat every -interval and sends a Prometheus remote_write request to
// that endpoint (e.g. Prometheus's --web.enable-remote-write-receiver). The
// local /metrics endpoint is still served for in-netns debugging.
//
// Each table becomes a metric family f5tmm_<table>_<column>; key columns become
// labels. Rows sharing a key are aggregated (rule-2 columns summed) so label
// sets stay unique.
//
// Every metric is emitted as a GAUGE (no _total suffix, TYPE gauge). tmstat's
// `rule` field marks counters, gauges (cur_conns, cpu_usage) and even constants
// (hz) all as rule 2, so it cannot reliably classify counter-vs-gauge — and
// some fields look like percentages but aren't (cpu_usage_1sec reads ~2992
// under load, not a 0-100%). Emitting raw values as gauges is the honest
// choice: dashboards apply rate() to the genuinely-monotonic series (bytes,
// packets, tot_conns) and read the instantaneous ones directly, and CPU% is
// derived from the tm_*_cycles counters rather than the misleading cpu_usage_*.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mwiget/tmmscope/internal/tmstat"
)

func main() {
	listen := flag.String("listen", envOr("TMSTAT_LISTEN", ":9099"), "HTTP listen address for the local /metrics + /healthz endpoints")
	segment := flag.String("segment", envOr("TMSTAT_SEGMENT", "/var/tmstat/blade/tmm0"), "path to the tmstat segment file")
	tablesCSV := flag.String("tables", envOr("TMSTAT_TABLES", "tmm_stat,virtual_server_stat,pool_member_stat,interface_stat"), "comma-separated tmstat tables to export")
	remoteWrite := flag.String("remote-write", os.Getenv("TMSTAT_REMOTE_WRITE_URL"), "Prometheus remote_write URL to push to (empty = serve /metrics only)")
	interval := flag.Duration("interval", envDuration("TMSTAT_PUSH_INTERVAL", 2*time.Second), "remote_write push interval")
	extLabels := flag.String("labels", os.Getenv("TMSTAT_EXTERNAL_LABELS"), "extra label set added to every series, comma-separated k=v (e.g. cluster=calico)")
	flag.Parse()

	e := &exporter{
		segment:  *segment,
		tables:   splitNonEmpty(*tablesCSV),
		extra:    parseLabels(*extLabels),
		rwURL:    *remoteWrite,
		interval: *interval,
	}

	http.HandleFunc("/metrics", e.handleMetrics)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := os.Stat(*segment); err != nil {
			http.Error(w, "segment not readable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})

	if e.rwURL != "" {
		log.Printf("tmm-stat-exporter: remote_write to %s every %s (labels %v)", e.rwURL, e.interval, e.extra)
		go e.pushLoop(context.Background())
	}
	log.Printf("tmm-stat-exporter: serving %s on %s (segment %s)", strings.Join(e.tables, ","), *listen, *segment)
	srv := &http.Server{Addr: *listen, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

type exporter struct {
	segment  string
	tables   []string
	extra    []label // external labels added to every series
	rwURL    string
	interval time.Duration
}

// sample is one metric data point: a fully-qualified metric name, its key
// labels, and the value. All metrics are gauges (see the package doc).
type sample struct {
	metric string
	labels []label
	value  float64
}

type label struct{ name, value string }

// collect reads the segment and produces every sample across the configured
// tables. Order is stable (table, then column, then row) so the text emitter can
// group a metric family under a single TYPE line.
func (e *exporter) collect() ([]sample, error) {
	data, err := os.ReadFile(e.segment)
	if err != nil {
		return nil, err
	}
	seg, err := tmstat.Parse(data)
	if err != nil {
		return nil, err
	}
	var out []sample
	for _, name := range e.tables {
		t := seg.Table(name)
		if t == nil {
			continue
		}
		rows, err := seg.Rows(name)
		if err != nil {
			continue
		}
		var keyCols, valCols []tmstat.Column
		for _, c := range t.Columns {
			switch {
			case c.IsKey():
				keyCols = append(keyCols, c)
			case c.IsNumeric():
				valCols = append(valCols, c)
			}
		}
		// Drop tmm's internal objects: it names control-plane pools / virtuals
		// with a leading underscore (_kmd_pool, _tmm_apmd_pool, …) — noise for
		// users, never their traffic. Filtering here keeps them out of Prometheus
		// entirely (not just hidden in a panel).
		rows = dropInternal(rows, keyCols)
		prefix := "f5tmm_" + strings.TrimSuffix(name, "_stat") + "_"
		for _, c := range valCols {
			metric := prefix + sanitize(c.Name)
			for _, r := range rows {
				lbls := make([]label, 0, len(keyCols))
				for _, kc := range keyCols {
					v := r.Value(kc)
					// tmstat addresses (pool_member.addr, virtual_server
					// .destination/.source) are 20-byte colon-hex — decode to a
					// human IP so the dashboard labels are meaningful.
					if kc.IsAddress() {
						if ip, ok := tmstat.DecodeAddr(v); ok {
							v = ip
						}
					}
					lbls = append(lbls, label{sanitize(kc.Name), v})
				}
				out = append(out, sample{metric: metric, labels: lbls, value: r.Float(c)})
			}
		}
	}
	return out, nil
}

// dropInternal removes rows that belong to tmm's own control plane rather than
// user traffic, on two signals:
//   - a key whose name starts with "_" — tmm's convention for internal objects
//     (control-plane pools/virtuals: _kmd_pool, _tmm_apmd_pool, …)
//   - an address key that decodes to a link-local address — 169.254.x.x
//     (IPv4) or fe80:: (IPv6) — tmm's internal self/management addresses,
//     never user-facing.
//
// Filters in place (reuses the backing array).
func dropInternal(rows []tmstat.Row, keyCols []tmstat.Column) []tmstat.Row {
	out := rows[:0]
	for _, r := range rows {
		internal := false
		for _, kc := range keyCols {
			v := r.Value(kc)
			if strings.HasPrefix(v, "_") {
				internal = true
				break
			}
			if kc.IsAddress() {
				if ip, ok := tmstat.DecodeAddr(v); ok && isLinkLocal(ip) {
					internal = true
					break
				}
			}
		}
		if !internal {
			out = append(out, r)
		}
	}
	return out
}

// isLinkLocal reports whether a decoded IP is link-local: 169.254.0.0/16
// (IPv4) or fe80::/10 (IPv6). tmm uses these for internal self/management
// addresses, never user traffic.
func isLinkLocal(ip string) bool {
	return strings.HasPrefix(ip, "169.254.") || strings.HasPrefix(ip, "fe80:")
}

func (e *exporter) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	start := time.Now()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	samples, err := e.collect()
	up := 1
	if err != nil {
		up = 0
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# TYPE f5tmm_up gauge\nf5tmm_up %d\n", up)
	fmt.Fprintf(&b, "# TYPE f5tmm_scrape_duration_seconds gauge\nf5tmm_scrape_duration_seconds %f\n", time.Since(start).Seconds())
	if err != nil {
		fmt.Fprintf(&b, "# collect error: %v\n", err)
	}
	prev := ""
	for _, s := range samples {
		if s.metric != prev {
			fmt.Fprintf(&b, "# TYPE %s gauge\n", s.metric)
			prev = s.metric
		}
		b.WriteString(s.metric)
		writeTextLabels(&b, s.labels)
		b.WriteByte(' ')
		b.WriteString(strconv.FormatFloat(s.value, 'g', -1, 64))
		b.WriteByte('\n')
	}
	_, _ = w.Write([]byte(b.String()))
}

// pushLoop samples and remote-writes on a fixed interval until ctx is done.
func (e *exporter) pushLoop(ctx context.Context) {
	t := time.NewTicker(e.interval)
	defer t.Stop()
	client := &http.Client{Timeout: 10 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.pushOnce(ctx, client); err != nil {
				log.Printf("remote_write: %v", err)
			}
		}
	}
}

func (e *exporter) pushOnce(ctx context.Context, client *http.Client) error {
	samples, collectErr := e.collect()
	// Always push f5tmm_up so Prometheus shows the exporter is alive even when
	// the segment is briefly unreadable (e.g. tmm still creating it after a
	// restart): up=1 when the segment parsed, up=0 otherwise.
	up := 1.0
	if collectErr != nil {
		up, samples = 0, nil
	}
	samples = append(samples, sample{metric: "f5tmm_up", value: up})
	tsMillis := time.Now().UnixMilli()
	body := buildWriteRequest(samples, e.extra, tsMillis)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.rwURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	req.Header.Set("User-Agent", "tmm-stat-exporter")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("remote_write %s returned %s", e.rwURL, resp.Status)
	}
	return collectErr // surface a segment-read problem after up=0 is pushed
}

func writeTextLabels(b *strings.Builder, labels []label) {
	if len(labels) == 0 {
		return
	}
	b.WriteByte('{')
	for i, l := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.name)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(l.value))
		b.WriteByte('"')
	}
	b.WriteByte('}')
}

// sanitize maps a tmstat column name to a valid Prometheus metric/label name
// ([a-zA-Z_:][a-zA-Z0-9_:]*): non-conforming bytes (e.g. '.') become '_'.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_', c == ':':
			b.WriteByte(c)
		case c >= '0' && c <= '9':
			if i == 0 {
				b.WriteByte('_')
			}
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	r := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n")
	return r.Replace(s)
}

// parseLabels parses "k=v,k2=v2" into sanitized label pairs.
func parseLabels(s string) []label {
	var out []label
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out = append(out, label{sanitize(strings.TrimSpace(k)), strings.TrimSpace(v)})
	}
	return out
}

func splitNonEmpty(csv string) []string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
