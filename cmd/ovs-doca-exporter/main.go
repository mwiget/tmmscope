// ovs-doca-exporter publishes the REAL dataplane byte/packet counters of an
// OVS-DOCA (NVIDIA BlueField) datapath to Prometheus remote_write.
//
// Why this exists: once DOCA hardware offload is engaged, established flows are
// handled by the eSwitch and never touch software accounting. Measured on a live
// BlueField DPU across a bench that moved ~20 GB:
//
//	ethtool -S p0 rx_bytes/tx_bytes ... delta 0
//	ovs-appctl dpctl/show -s port stats delta ~1 MB
//	ovs-appctl dpctl/dump-flows byte sum . delta ~27 GB   <- ground truth
//
// Only the per-flow counters see offloaded traffic, because OVS reads them back
// from hardware. So this exporter dumps flows and aggregates them.
//
// Flows are ephemeral (max-idle defaults to 20 s), so raw per-flow counters are
// neither stable series nor monotonic: a flow appears, counts, and vanishes.
// Publishing them directly would make rate() nonsense and explode cardinality.
// Instead each flow is tracked by ufid and only its DELTA is folded into a small
// set of long-lived aggregate counters, so the published series are monotonic
// and low-cardinality — exactly what rate() expects.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mwiget/tmmscope/internal/promwrite"
)

func main() {
	ctlPath := flag.String("ctl", envOr("OVS_CTL", ""), "ovs-vswitchd unixctl socket (default: newest /var/run/openvswitch/ovs-vswitchd.*.ctl)")
	rw := flag.String("remote-write", os.Getenv("TMSTAT_REMOTE_WRITE_URL"), "Prometheus remote_write URL")
	interval := flag.Duration("interval", envDur("PUSH_INTERVAL", 5*time.Second), "scrape/push interval")
	extLabels := flag.String("labels", os.Getenv("TMSTAT_EXTERNAL_LABELS"), "extra labels, comma-separated k=v")
	rules := flag.String("tenant-rules", envOr("TENANT_RULES",
		"vni=0x186a1=acme,tun_id=0x186a1=acme,192.168.100.=acme,10.0.121.=acme,"+
			"vni=0x186a2=bravo,tun_id=0x186a2=bravo,192.168.200.=bravo,10.0.122.=bravo"),
		"comma-separated <pattern>=<tenant> rules matched as substrings against each flow, first match wins "+
			"(patterns may themselves contain '=', the tenant is taken after the LAST '=')")
	once := flag.Bool("once", false, "print one scrape to stdout and exit")
	flag.Parse()

	e := &exporter{ctl: *ctlPath, rwURL: *rw, extra: parseLabels(*extLabels),
		rules: parseRules(*rules), seen: map[string]flowCounter{}, totals: map[key]*acc{}}

	if *once {
		if err := e.scrape(); err != nil {
			log.Fatalf("scrape: %v", err)
		}
		for _, s := range e.samples() {
			fmt.Printf("%s%s %g\n", s.Metric, fmtLabels(s.Labels), s.Value)
		}
		return
	}
	if e.rwURL == "" {
		log.Fatal("no --remote-write URL")
	}
	log.Printf("ovs-doca-exporter: remote_write to %s every %s (%d tenant rules)", e.rwURL, *interval, len(e.rules))
	t := time.NewTicker(*interval)
	defer t.Stop()
	client := &http.Client{Timeout: 20 * time.Second}
	for range t.C {
		if err := e.scrape(); err != nil {
			log.Printf("scrape: %v", err)
		}
		if err := e.push(context.Background(), client); err != nil {
			log.Printf("push: %v", err)
		}
	}
}

type key struct{ tenant, port string }

type acc struct{ bytes, packets float64 }

type flowCounter struct{ bytes, packets float64 }

type exporter struct {
	ctl    string
	rwURL  string
	extra  []promwrite.Label
	rules  []rule
	seen   map[string]flowCounter // ufid -> last counters
	totals map[key]*acc           // accumulated, monotonic
	flows  int
	offl   int
	up     float64
}

// unixctl speaks OVS's JSON-RPC over the unixctl socket — the same transport
// ovs-appctl uses. Talking it directly keeps the container image free of any
// OVS binaries (the sidecar is distroless).
func (e *exporter) unixctl(method string, params ...string) (string, error) {
	path := e.ctl
	if path == "" {
		m, _ := filepath.Glob("/var/run/openvswitch/ovs-vswitchd.*.ctl")
		if len(m) == 0 {
			return "", fmt.Errorf("no ovs-vswitchd unixctl socket found")
		}
		path = m[len(m)-1]
	}
	c, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	p := []any{}
	for _, s := range params {
		p = append(p, s)
	}
	req, _ := json.Marshal(map[string]any{"method": method, "params": p, "id": 0})
	if _, err := c.Write(req); err != nil {
		return "", err
	}
	// The reply is a single JSON object; decode incrementally so we stop as soon
	// as it is complete rather than waiting for EOF (the server keeps the socket).
	dec := json.NewDecoder(bufio.NewReader(c))
	var resp struct {
		Result any `json:"result"`
		Error  any `json:"error"`
	}
	if err := dec.Decode(&resp); err != nil {
		return "", err
	}
	if resp.Error != nil {
		return "", fmt.Errorf("ovs error: %v", resp.Error)
	}
	s, _ := resp.Result.(string)
	return s, nil
}

var (
	reUfid    = regexp.MustCompile(`ufid:([0-9a-f-]+)`)
	rePackets = regexp.MustCompile(`packets:(\d+)`)
	reBytes   = regexp.MustCompile(`bytes:(\d+)`)
	rePort    = regexp.MustCompile(`in_port\(([^)]+)\)`)
	reOffl    = regexp.MustCompile(`offloaded:yes`)
)

// scrape dumps the datapath flows and folds each flow's delta into the totals.
func (e *exporter) scrape() error {
	out, err := e.unixctl("dpctl/dump-flows", "-m")
	if err != nil {
		e.up = 0
		return err
	}
	e.up, e.flows, e.offl = 1, 0, 0
	live := make(map[string]flowCounter, len(e.seen))
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		mu := reUfid.FindStringSubmatch(line)
		if mu == nil {
			continue
		}
		ufid := mu[1]
		pk := num(rePackets, line)
		by := num(reBytes, line)
		e.flows++
		offloaded := reOffl.MatchString(line)
		if offloaded {
			e.offl++
		}
		tenant := e.tenantFor(line)
		// Label by the datapath port the flow arrives on rather than a synthetic
		// direction: p0 is the fabric uplink, en3f0pf0sfNNN the service functions.
		// Small, stable cardinality and it maps straight onto the wiring.
		port := "unknown"
		if m := rePort.FindStringSubmatch(line); m != nil {
			port = m[1]
		}
		live[ufid] = flowCounter{bytes: by, packets: pk}
		prev := e.seen[ufid]
		// A flow's counters only ever climb while it lives; if it went backwards
		// the ufid was reused, so count the new value as fresh traffic.
		db, dp := by-prev.bytes, pk-prev.packets
		if db < 0 || dp < 0 {
			db, dp = by, pk
		}
		k := key{tenant: tenant, port: port}
		a := e.totals[k]
		if a == nil {
			a = &acc{}
			e.totals[k] = a
		}
		a.bytes += db
		a.packets += dp
	}
	e.seen = live // flows that vanished stop being tracked; their bytes are already banked
	return sc.Err()
}

// rule attributes a flow to a tenant when its pattern appears in the flow text.
type rule struct{ pattern, tenant string }

// tenantFor returns the first matching rule's tenant. Matching on the flow text
// covers every shape the same tenant takes on this datapath — VXLAN vni on the
// fabric side, and the tenant's origin subnet / VIP on the SF side, where the
// internal flows carry no VNI at all (bravo is 802.1Q-tagged instead).
func (e *exporter) tenantFor(line string) string {
	for _, r := range e.rules {
		if strings.Contains(line, r.pattern) {
			return r.tenant
		}
	}
	return "other"
}

func parseRules(s string) []rule {
	var out []rule
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		i := strings.LastIndex(kv, "=")
		if i <= 0 {
			continue
		}
		out = append(out, rule{pattern: kv[:i], tenant: kv[i+1:]})
	}
	return out
}

func (e *exporter) samples() []promwrite.Sample {
	out := []promwrite.Sample{
		{Metric: "ovs_doca_up", Value: e.up},
		{Metric: "ovs_doca_flows", Value: float64(e.flows)},
		{Metric: "ovs_doca_flows_offloaded", Value: float64(e.offl)},
	}
	for k, a := range e.totals {
		l := []promwrite.Label{{Name: "tenant", Value: k.tenant}, {Name: "port", Value: k.port}}
		out = append(out,
			promwrite.Sample{Metric: "ovs_doca_datapath_bytes", Labels: l, Value: a.bytes},
			promwrite.Sample{Metric: "ovs_doca_datapath_packets", Labels: l, Value: a.packets})
	}
	return out
}

func (e *exporter) push(ctx context.Context, c *http.Client) error {
	body := promwrite.BuildWriteRequest(e.samples(), e.extra, time.Now().UnixMilli())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.rwURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	req.Header.Set("User-Agent", "ovs-doca-exporter")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("remote_write %s: %s", e.rwURL, resp.Status)
	}
	return nil
}

func num(re *regexp.Regexp, s string) float64 {
	if m := re.FindStringSubmatch(s); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		return v
	}
	return 0
}

func parseLabels(s string) []promwrite.Label {
	var out []promwrite.Label
	for _, kv := range strings.Split(s, ",") {
		if a, b, ok := strings.Cut(strings.TrimSpace(kv), "="); ok && a != "" {
			out = append(out, promwrite.Label{Name: a, Value: b})
		}
	}
	return out
}

func fmtLabels(l []promwrite.Label) string {
	if len(l) == 0 {
		return ""
	}
	var p []string
	for _, x := range l {
		p = append(p, fmt.Sprintf("%s=%q", x.Name, x.Value))
	}
	return "{" + strings.Join(p, ",") + "}"
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envDur(k string, d time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if p, err := time.ParseDuration(v); err == nil {
			return p
		}
	}
	return d
}
