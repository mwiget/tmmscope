package tmstat

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadSegment parses the committed snapshot fixture (captured live from a tmm
// pod; see testdata/README.md for provenance + how to regenerate).
func loadSegment(t *testing.T) *Segment {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "snap.tmm0.gz"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzip fixture: %v", err)
	}
	data, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	seg, err := Parse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return seg
}

func TestDecodeAddr(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"00:00:00:00:00:00:00:00:00:00:FF:FF:A9:FE:00:01:00:00:00:00", "169.254.0.1", true},
		{"00:00:00:00:00:00:00:00:00:00:FF:FF:CB:00:71:64:00:00:00:00", "203.0.113.100", true},
		{"00:00:00:00:00:00:00:00:00:00:FF:FF:7F:14:01:FE:00:00:00:00", "127.20.1.254", true},
		{"FE:80:00:00:00:00:00:00:02:01:23:FF:FE:45:67:00:00:00:00:00", "fe80::201:23ff:fe45:6700", true},
		{"not-an-address", "", false},
	}
	for _, c := range cases {
		got, ok := DecodeAddr(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("DecodeAddr(%q) = %q,%v; want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestSegmentBasics(t *testing.T) {
	seg := loadSegment(t)
	if seg.slabSize != 4096 {
		t.Errorf("slabSize = %d, want 4096", seg.slabSize)
	}
	if seg.slabCount != 1024 {
		t.Errorf("slabCount = %d, want 1024", seg.slabCount)
	}
	// verify.txt: Table count is 412.
	if got := len(seg.Tables()); got != 412 {
		t.Errorf("table count = %d, want 412", got)
	}
	for _, name := range []string{"tmm_stat", "virtual_server_stat", "pool_member_stat", "interface_stat"} {
		tbl := seg.Table(name)
		if tbl == nil {
			t.Fatalf("table %q not found", name)
		}
		if len(tbl.Columns) != tbl.Cols {
			t.Errorf("%s: %d columns parsed, .table says cols=%d", name, len(tbl.Columns), tbl.Cols)
		}
	}
}

// TestCSVGoldens is the gate that lets us drop the tmctl dependency: our pure-Go
// CSV render of each table must match `tmctl -f snap -c <table>` byte for byte.
func TestCSVGoldens(t *testing.T) {
	seg := loadSegment(t)
	for _, name := range []string{"tmm_stat", "virtual_server_stat", "pool_member_stat", "interface_stat"} {
		t.Run(name, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join("testdata", "csv-"+name+".csv"))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			got, err := seg.CSV(name)
			if err != nil {
				t.Fatalf("CSV: %v", err)
			}
			gotLines := strings.Split(strings.TrimRight(got, "\n"), "\n")
			wantLines := strings.Split(strings.TrimRight(string(want), "\n"), "\n")
			if len(gotLines) != len(wantLines) {
				t.Fatalf("line count: got %d, want %d\nfirst got:  %s\nfirst want: %s",
					len(gotLines), len(wantLines), head(gotLines), head(wantLines))
			}
			for i := range wantLines {
				if gotLines[i] != wantLines[i] {
					t.Fatalf("line %d differs (got || want):\n%s", i,
						firstDiffContext(gotLines[i], wantLines[i]))
				}
			}
		})
	}
}

func head(ss []string) string {
	if len(ss) == 0 {
		return "<none>"
	}
	if len(ss[0]) > 200 {
		return ss[0][:200] + "…"
	}
	return ss[0]
}

// firstDiffContext returns the two strings trimmed around their first differing
// field so failures are readable on 90-column rows.
func firstDiffContext(got, want string) string {
	g := strings.Split(got, ",")
	w := strings.Split(want, ",")
	n := len(g)
	if len(w) < n {
		n = len(w)
	}
	for i := 0; i < n; i++ {
		if g[i] != w[i] {
			lo := i - 3
			if lo < 0 {
				lo = 0
			}
			hi := i + 4
			if hi > n {
				hi = n
			}
			return strings.Join(g[lo:hi], ",") + "  ||  " + strings.Join(w[lo:hi], ",") +
				"  (field " + itoa(i) + ")"
		}
	}
	if len(g) != len(w) {
		return got + "  ||  " + want + "  (field count " + itoa(len(g)) + " vs " + itoa(len(w)) + ")"
	}
	return got + "  ||  " + want
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
