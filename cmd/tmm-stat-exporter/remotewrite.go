package main

import (
	"encoding/binary"
	"math"
	"sort"

	"github.com/golang/snappy"
)

// buildWriteRequest encodes samples into a snappy-compressed Prometheus
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
func buildWriteRequest(samples []sample, extra []label, tsMillis int64) []byte {
	var req []byte
	for _, s := range samples {
		labels := make([]label, 0, len(s.labels)+len(extra)+1)
		labels = append(labels, label{"__name__", s.metric})
		labels = append(labels, extra...)
		labels = append(labels, s.labels...)
		sort.Slice(labels, func(i, j int) bool { return labels[i].name < labels[j].name })

		var ts []byte
		for _, l := range labels {
			ts = appendBytesField(ts, 1, encodeLabel(l)) // TimeSeries.labels = 1
		}
		ts = appendBytesField(ts, 2, encodeSample(s.value, tsMillis)) // TimeSeries.samples = 2

		req = appendBytesField(req, 1, ts) // WriteRequest.timeseries = 1
	}
	return snappy.Encode(nil, req)
}

func encodeLabel(l label) []byte {
	var b []byte
	b = appendStringField(b, 1, l.name)  // Label.name = 1
	b = appendStringField(b, 2, l.value) // Label.value = 2
	return b
}

func encodeSample(value float64, tsMillis int64) []byte {
	var b []byte
	b = appendDoubleField(b, 1, value)    // Sample.value = 1 (double, wire type 1)
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
