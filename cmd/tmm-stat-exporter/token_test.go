package main

import (
	"strconv"
	"testing"
)

func TestTokenValueRe(t *testing.T) {
	// want = -1 means "no numeric counter" (regex should not match).
	cases := map[string]float64{
		"V0010005000000b40000000000000000S00065":      65, // table incr counter (zero-padded)
		"V0010005ffffffffffffffff000000000000000S07":  7,
		"V0010005ffffffffffffffff0000000000000002000": -1, // no trailing S<digits>
		"":   -1,
		"S5": 5,
	}
	for in, want := range cases {
		m := tokenValueRe.FindStringSubmatch(in)
		if want < 0 {
			if m != nil {
				t.Errorf("value %q: expected no match, got %q", in, m[1])
			}
			continue
		}
		if m == nil {
			t.Errorf("value %q: expected match %v, got none", in, want)
			continue
		}
		got, _ := strconv.ParseFloat(m[1], 64)
		if got != want {
			t.Errorf("value %q: got %v want %v", in, got, want)
		}
	}
}

func TestParseTokenMember(t *testing.T) {
	suffix, lbls := parseTokenMember("total|vs=203.0.113.105|model=gpt-stub|day=20260624")
	if suffix != "total" {
		t.Fatalf("suffix=%q", suffix)
	}
	want := map[string]string{"vs": "203.0.113.105", "model": "gpt-stub", "day": "20260624"}
	if len(lbls) != len(want) {
		t.Fatalf("labels=%v", lbls)
	}
	for _, l := range lbls {
		if want[l.name] != l.value {
			t.Errorf("label %s=%s, want %s", l.name, l.value, want[l.name])
		}
	}

	// No labels, just a bare suffix.
	s2, l2 := parseTokenMember("prompt")
	if s2 != "prompt" || l2 != nil {
		t.Errorf("bare: suffix=%q labels=%v", s2, l2)
	}

	// A label name gets sanitized; a malformed pair (no '=') is dropped.
	s3, l3 := parseTokenMember("completion|pool name=p1|garbage")
	if s3 != "completion" || len(l3) != 1 || l3[0].name != "pool_name" || l3[0].value != "p1" {
		t.Errorf("sanitize/drop: suffix=%q labels=%v", s3, l3)
	}
}

func TestDropPerInstance(t *testing.T) {
	extra := []label{{"cluster", "calico"}, {"pod", "f5-tmm-x"}, {"node", "agent-0"}, {"region", "us"}}
	got := dropPerInstance(extra)
	want := map[string]bool{"cluster": true, "region": true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, l := range got {
		if !want[l.name] {
			t.Errorf("unexpected label %s kept", l.name)
		}
		if l.name == "pod" || l.name == "node" {
			t.Errorf("per-instance label %s should be dropped", l.name)
		}
	}
}

func TestDSSMDisabledWhenNoCert(t *testing.T) {
	if (dssmConfig{addr: "x:6379", certDir: "/nonexistent", subtable: "TMMTOK"}).enabled() {
		t.Error("should be disabled when cert dir is absent")
	}
	if (dssmConfig{addr: "", certDir: "/etc", subtable: "TMMTOK"}).enabled() {
		t.Error("should be disabled when addr empty")
	}
}
