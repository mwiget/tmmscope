package promwrite

import (
	"encoding/binary"
	"math"
	"sort"

	"github.com/golang/snappy"
)

// BuildWriteRequest encodes samples into a snappy-compressed Prometheus
// remote_write request body (prometheus.WriteRequest, proto3). It hand-rolls the
// wire format — the schema is tiny and stable, so this avoids a protoc/codegen
// dependency:
//
//	WriteRequest { repeated TimeSeries timeseries = 1; }
//	TimeSeries   { repeated Label labels = 1; repeated Sample samples = 2; }
//	Label        { string name = 1; string value = 2; }
//	Sample       { double value = 1; int64 timestamp = 2; }
//
// Each sample becomes one TimeSeries: its metric name as the __name__ label,
// plus the external labels and the sample's key labels. Label sets are sorted by
// name (remote_write requires sorted, unique label names).
// Sample is one metric sample to publish.
type Sample struct {
	Metric string
	Labels []Label
	Value  float64
	// Global marks a cluster-shared series: the per-instance labels (pod, node)
	// are dropped so every producer writes the same series instead of N copies.
	Global bool
}

// Label is a single metric label.
type Label struct{ Name, Value string }

func BuildWriteRequest(samples []Sample, extra []Label, tsMillis int64) []byte {
	// Per-instance external labels (pod, node) are dropped from global samples:
	// the DSSM token counters are a single cluster-shared value every tmm pod's
	// exporter reads identically, so tagging them per-pod would split one series
	// into N duplicates. Global samples keep only cluster-scoped labels.
	//
	// CAVEAT (multi-tmm): because the label set is identical across pods, on a
	// cluster with N>1 tmm pods all N exporters push the SAME global series every
	// interval at offset times, so the remote_write receiver logs "out of order
	// sample" and drops the later-arriving duplicate. This is benign — every pod
	// reads the same DSSM counter and pushes the same value, so the accepted
	// samples still form a correct series — just noisy. A clean fix would be
	// single-writer token export (lease/leader) or a per-pod label with the
	// dashboard using max by(...) instead of sum.
	globalExtra := dropPerInstance(extra)

	var req []byte
	for _, s := range samples {
		ex := extra
		if s.Global {
			ex = globalExtra
		}
		labels := make([]Label, 0, len(s.Labels)+len(ex)+1)
		labels = append(labels, Label{"__name__", s.Metric})
		labels = append(labels, ex...)
		labels = append(labels, s.Labels...)
		sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })

		var ts []byte
		for _, l := range labels {
			ts = appendBytesField(ts, 1, encodeLabel(l)) // TimeSeries.Labels = 1
		}
		ts = appendBytesField(ts, 2, encodeSample(s.Value, tsMillis)) // TimeSeries.samples = 2

		req = appendBytesField(req, 1, ts) // WriteRequest.timeseries = 1
	}
	return snappy.Encode(nil, req)
}

// dropPerInstance returns the external labels minus the per-instance ones
// (pod, node), for cluster-shared global series.
func dropPerInstance(extra []Label) []Label {
	out := make([]Label, 0, len(extra))
	for _, l := range extra {
		if l.Name == "pod" || l.Name == "node" {
			continue
		}
		out = append(out, l)
	}
	return out
}

func encodeLabel(l Label) []byte {
	var b []byte
	b = appendStringField(b, 1, l.Name)  // Label.Name = 1
	b = appendStringField(b, 2, l.Value) // Label.Value = 2
	return b
}

func encodeSample(value float64, tsMillis int64) []byte {
	var b []byte
	b = appendDoubleField(b, 1, value)    // Sample.Value = 1 (double, wire type 1)
	b = appendVarintField(b, 2, tsMillis) // Sample.timestamp = 2 (int64, wire type 0)
	return b
}

// --- minimal protobuf wire helpers ---

func appendTag(b []byte, field, wire int) []byte {
	return appendUvarint(b, uint64(field)<<3|uint64(wire))
}

func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendBytesField(b []byte, field int, data []byte) []byte {
	b = appendTag(b, field, 2) // wire type 2 = length-delimited
	b = appendUvarint(b, uint64(len(data)))
	return append(b, data...)
}

func appendStringField(b []byte, field int, s string) []byte {
	b = appendTag(b, field, 2)
	b = appendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

func appendVarintField(b []byte, field int, v int64) []byte {
	b = appendTag(b, field, 0) // wire type 0 = varint
	return appendUvarint(b, uint64(v))
}

func appendDoubleField(b []byte, field int, f float64) []byte {
	b = appendTag(b, field, 1) // wire type 1 = 64-bit
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], math.Float64bits(f))
	return append(b, buf[:]...)
}
