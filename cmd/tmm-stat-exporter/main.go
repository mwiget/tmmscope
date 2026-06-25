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
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	dssmAddr := flag.String("dssm-addr", envOr("TMMTOK_DSSM_ADDR", "f5-dssm-db:6379"), "DSSM Redis address for iRule `table` token counters (auto-detected: no-op unless reachable and the subtable exists)")
	dssmCertDir := flag.String("dssm-cert-dir", envOr("TMMTOK_DSSM_CERT_DIR", "/tls/tmm/mds/clt"), "dir with tls.crt/tls.key/ca.crt for DSSM mTLS (empty or missing = token export disabled)")
	dssmServerName := flag.String("dssm-server-name", envOr("TMMTOK_DSSM_SERVER_NAME", "dssm-svc"), "TLS server name DSSM presents")
	dssmSubtable := flag.String("dssm-subtable", envOr("TMMTOK_DSSM_SUBTABLE", "TMMTOK"), "iRule `table` subtable marker holding the token counters")
	dssmDB := flag.Int("dssm-db", envInt("TMMTOK_DSSM_DB", 0), "DSSM Redis logical DB to scan")
	once := flag.Bool("once", false, "collect a single snapshot, print the /metrics text to stdout, and exit (debug)")
	flag.Parse()

	e := &exporter{
		segment:  *segment,
		tables:   splitNonEmpty(*tablesCSV),
		extra:    parseLabels(*extLabels),
		rwURL:    *remoteWrite,
		interval: *interval,
		dssm: dssmConfig{
			addr:       *dssmAddr,
			certDir:    *dssmCertDir,
			serverName: *dssmServerName,
			subtable:   *dssmSubtable,
			db:         *dssmDB,
		},
	}

	if *once {
		rec := httptest.NewRecorder()
		e.handleMetrics(rec, nil)
		fmt.Print(rec.Body.String())
		return
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
	dssm     dssmConfig

	mu          sync.Mutex
	dssmLastErr string // last DSSM dial/scan error, for transition-only logging
}

// dssmConfig points the token collector at the DSSM Redis holding the iRule
// `table` counters. It is entirely opt-in/auto-detecting: if certDir is unset
// or absent, or Redis is unreachable, or the subtable has no keys, the
// collector simply contributes no samples — it never fails the tmstat path.
type dssmConfig struct {
	addr       string
	certDir    string
	serverName string
	subtable   string
	db         int
}

// enabled reports whether the DSSM client cert is present. Absent cert dir =
// this deployment has no token iRule wired, so token export stays off.
func (d dssmConfig) enabled() bool {
	if d.addr == "" || d.certDir == "" || d.subtable == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(d.certDir, "tls.crt"))
	return err == nil
}

// sample is one metric data point: a fully-qualified metric name, its key
// labels, and the value. All metrics are gauges (see the package doc).
type sample struct {
	metric string
	labels []label
	value  float64
	// global marks a cluster-shared series (the DSSM token counters): its
	// per-instance external labels (pod, node) are dropped at remote_write so
	// every pod's exporter reports the one shared counter as a single series.
	global bool
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
	// Best-effort: append iRule token counters from DSSM/Redis when present.
	// Never fails the tmstat collect — token errors only suppress token samples.
	out = append(out, e.collectTokens()...)
	return out, nil
}

// tokenValueRe extracts the integer a `table incr` counter holds inside the
// DSSM record envelope. The value is stored as `V001…S<zero-padded-decimal>`
// (e.g. `V0010005000000b4…S00065` → 65); non-counter entries (e.g. the
// subtable's set markers) lack a trailing S<digits> and are skipped.
var tokenValueRe = regexp.MustCompile(`S([0-9]+)$`)

// collectTokens reads the iRule `table` token counters out of DSSM/Redis and
// returns them as f5tmm_token_<suffix> gauges. It is fully auto-detecting:
// returns nil (no error surfaced) when DSSM is disabled, unreachable, or the
// subtable is empty — so a cluster without the token-counting iRule emits
// nothing extra.
//
// The iRule stores each counter in a subtable named <subtable> (the marker),
// with member key `<suffix>|<lbl>=<val>|<lbl>=<val>…`. DSSM concatenates that
// into a Redis key `<prefix><hash><subtable><member>`; we strip up to and
// including the marker to recover the member, then split it into a metric
// suffix and labels.
func (e *exporter) collectTokens() []sample {
	if !e.dssm.enabled() {
		return nil
	}
	c, err := dialRedis(e.dssm.addr, e.dssm.certDir, e.dssm.serverName, e.dssm.db, 5*time.Second)
	if err != nil {
		e.dssmError("dial: " + err.Error())
		return nil
	}
	defer c.Close()

	keys, err := c.scanMatch("*"+e.dssm.subtable+"*", 200)
	if err != nil {
		e.dssmError("scan: " + err.Error())
		return nil
	}
	var out []sample
	for _, key := range keys {
		idx := strings.Index(key, e.dssm.subtable)
		if idx < 0 {
			continue
		}
		member := key[idx+len(e.dssm.subtable):]
		if member == "" {
			continue // the subtable index list key
		}
		raw, ok := c.get(key)
		if !ok {
			continue // missing or wrong-type (the index list)
		}
		m := tokenValueRe.FindStringSubmatch(raw)
		if m == nil {
			continue // not a numeric counter
		}
		val, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		suffix, lbls := parseTokenMember(member)
		if suffix == "" {
			continue
		}
		out = append(out, sample{metric: "f5tmm_token_" + sanitize(suffix), labels: lbls, value: val, global: true})
	}
	// Stable order so the text emitter groups each family under one TYPE line.
	sort.Slice(out, func(i, j int) bool {
		if out[i].metric != out[j].metric {
			return out[i].metric < out[j].metric
		}
		return labelKey(out[i].labels) < labelKey(out[j].labels)
	})
	if len(out) > 0 {
		e.clearDSSMError()
	}
	return out
}

// parseTokenMember splits an iRule member key `suffix|k=v|k=v` into the metric
// suffix and its sanitized labels. Pairs without '=' are ignored.
func parseTokenMember(member string) (string, []label) {
	parts := strings.Split(member, "|")
	suffix := parts[0]
	var lbls []label
	for _, p := range parts[1:] {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			continue
		}
		lbls = append(lbls, label{sanitize(k), v})
	}
	return suffix, lbls
}

func labelKey(lbls []label) string {
	var b strings.Builder
	for _, l := range lbls {
		b.WriteString(l.name)
		b.WriteByte('=')
		b.WriteString(l.value)
		b.WriteByte(',')
	}
	return b.String()
}

// dssmError logs a DSSM read failure only on transition (when the error first
// appears or its text changes), not every scrape — so a cluster where DSSM
// isn't deployed (or the token iRule isn't wired) logs at most one line, not a
// stream every push interval.
func (e *exporter) dssmError(msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if msg != e.dssmLastErr {
		log.Printf("dssm token export disabled: %s", msg)
		e.dssmLastErr = msg
	}
}

// clearDSSMError records recovery: if we were in an error state, log that token
// export resumed, then reset so the next outage logs again.
func (e *exporter) clearDSSMError() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dssmLastErr != "" {
		log.Printf("dssm token export: recovered, exporting token counters")
		e.dssmLastErr = ""
	}
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

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
