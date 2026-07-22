// Package tmstat is a dependency-free reader for F5 tmm's tmstat shared-memory
// segments (the binary files under /var/tmstat/blade/, e.g. tmm0). It parses
// the on-disk "TMSS" segment format directly — no tmctl, no cgo, stdlib only —
// so a tiny exporter can read tmm's live virtual-server / pool-member / tmm /
// interface counters at seconds granularity and publish them (e.g. Prometheus).
//
// # Segment format (reverse-engineered, validated byte-for-byte vs tmctl)
//
// A segment is a flat array of fixed-size slabs (slab size advertised in the
// header, 4096 in practice). Every slab begins with a 64-byte header; row data
// starts at slab+64. Slab 0's 64-byte header doubles as the segment header.
//
//	slab header (little-endian):
//	  +0   [4]byte "TMSS" magic
//	  +4   uint16  tableid      — which table owns this slab's rows
//	  +6   uint16  linesPerRow  — row stride = linesPerRow * 64 ("line" = 64B)
//	  +16  uint32  slabIndex<<8 | 0xff
//	  +64  rows...
//
// Tables are self-describing via two bootstrap meta-tables whose own layout is
// stable tmm internals (hardcoded below to break the chicken-and-egg):
//
//	.table  (tableid 0): one row per table, in tableid order. Fields: name,
//	        rows (live row count), rowsz (logical row bytes), cols.
//	.column (tableid 1): one row per column of every table. Fields: name,
//	        tableid (owning table), offset (within the row), size, type.
//
// A table's rows live in every slab whose header tableid matches; each slab
// self-reports its row stride at +6, so the reader never has to recompute it.
//
// Column value types observed: 1=signed int, 2=unsigned int, 3=NUL-terminated
// string, 6=byte address (rendered colon-hex, e.g. MAC/IPv6+routedomain).
package tmstat

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const (
	magic       = "TMSS"
	slabHdrSize = 64
	lineSize    = 64 // a "line" is 64 bytes; row stride = linesPerRow * lineSize

	// Bootstrap meta-table ids (their tableid == .table storage index).
	tidTable  = 0 // ".table"
	tidColumn = 1 // ".column"

	// Column value types (the `type` field in .column).
	typeSignedInt   = 1
	typeUnsignedInt = 2
	typeString      = 3
	typeAddress     = 6

	// Column aggregation rules (the `rule` field in .column). tmm tables are
	// keyed: rows sharing the same key columns are merged on read. tmctl does
	// this for display; an exporter must too (no duplicate Prometheus label
	// sets). rule 0 marks a key column; rule 2 marks an additive counter.
	ruleKey     = 0
	ruleCounter = 2
)

// Column describes one field within a table's row.
type Column struct {
	Name    string
	Offset  int
	Size    int
	Type    int
	Rule    int
	Dropped bool
}

// IsKey reports whether the column is a key column (part of the row identity;
// rendered as a label, never a metric value).
func (c Column) IsKey() bool { return c.Rule == ruleKey && !c.Dropped }

// IsCounter reports whether the column is an additive counter (rule 2).
func (c Column) IsCounter() bool { return c.Rule == ruleCounter }

// IsNumeric reports whether the column holds an integer value (suitable as a
// metric sample), as opposed to a string or byte address.
func (c Column) IsNumeric() bool {
	return c.Type == typeSignedInt || c.Type == typeUnsignedInt
}

// IsAddress reports whether the column is a tmstat byte-address (type 6) —
// rendered as colon-hex by Value, but decodable to a human IP via DecodeAddr.
func (c Column) IsAddress() bool { return c.Type == typeAddress }

// DecodeAddr converts a tmstat 20-byte colon-hex address (the form Value returns
// for type-6 columns, e.g. pool_member.addr / virtual_server.destination) to a
// human-readable IP. The layout is a 16-byte IP — IPv4-mapped when bytes 10-11
// are ff:ff — followed by a 4-byte route domain. Returns (ip, true) on success,
// ("", false) if the input isn't 20 hex bytes.
func DecodeAddr(colonHex string) (string, bool) {
	parts := strings.Split(colonHex, ":")
	if len(parts) != 20 {
		return "", false
	}
	var b [20]byte
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return "", false
		}
		b[i] = byte(v)
	}
	if b[10] == 0xff && b[11] == 0xff {
		return net.IPv4(b[12], b[13], b[14], b[15]).String(), true
	}
	return net.IP(b[:16]).String(), true
}

// Table is the schema + location metadata for one tmstat table.
type Table struct {
	Name    string
	ID      int // tableid == storage index in .table
	Rows    int // live row count
	RowSize int // logical row size in bytes (rowsz)
	Cols    int
	Columns []Column
}

// Segment is a parsed tmstat segment.
type Segment struct {
	data      []byte
	slabSize  int
	slabCount int
	byName    map[string]*Table
	byID      map[int]*Table
	order     []*Table // tables in tableid order
}

// Row is one decoded row: column values rendered to strings exactly as tmctl
// would, plus typed accessors for the exporter.
type Row struct {
	tbl *Table
	raw []byte
}

// Parse decodes a tmstat segment from its raw bytes (e.g. the contents of
// /var/tmstat/blade/tmm0).
func Parse(data []byte) (*Segment, error) {
	if len(data) < slabHdrSize || string(data[0:4]) != magic {
		return nil, fmt.Errorf("tmstat: not a TMSS segment")
	}
	s := &Segment{data: data, byName: map[string]*Table{}, byID: map[int]*Table{}}

	// Slab size = distance to the second TMSS magic (fall back to 4096).
	s.slabSize = 4096
	for off := 1024; off+4 <= len(data); off += 1024 {
		if string(data[off:off+4]) == magic {
			s.slabSize = off
			break
		}
	}
	if s.slabSize < slabHdrSize {
		return nil, fmt.Errorf("tmstat: bad slab size %d", s.slabSize)
	}
	s.slabCount = len(data) / s.slabSize

	if err := s.loadSchema(); err != nil {
		return nil, err
	}
	return s, nil
}

// bootstrapTable returns the hardcoded layout for one of the two meta-tables,
// used only to read .table/.column themselves. These layouts are stable tmm
// internals; everything else is discovered from the segment.
func bootstrapTable(id int) *Table {
	switch id {
	case tidTable:
		return &Table{Name: ".table", ID: tidTable, Columns: []Column{
			{Name: "name", Offset: 0, Size: 49, Type: typeString},
			{Name: "rows", Offset: 53, Size: 4, Type: typeUnsignedInt},
			{Name: "rowsz", Offset: 57, Size: 2, Type: typeUnsignedInt},
			{Name: "cols", Offset: 59, Size: 2, Type: typeUnsignedInt},
		}}
	case tidColumn:
		return &Table{Name: ".column", ID: tidColumn, Columns: []Column{
			{Name: "name", Offset: 0, Size: 49, Type: typeString},
			{Name: "tableid", Offset: 49, Size: 2, Type: typeUnsignedInt},
			{Name: "offset", Offset: 51, Size: 2, Type: typeUnsignedInt},
			{Name: "size", Offset: 53, Size: 2, Type: typeUnsignedInt},
			{Name: "type", Offset: 55, Size: 1, Type: typeUnsignedInt},
			{Name: "rule", Offset: 56, Size: 2, Type: typeUnsignedInt},
			{Name: "dropped", Offset: 58, Size: 1, Type: typeUnsignedInt},
		}}
	}
	return nil
}

// loadSchema reads .table then .column to populate every table's metadata.
func (s *Segment) loadSchema() error {
	// .table: one row per table, in tableid order. We don't yet know its live
	// row count, so walk every .table slab to the end.
	//
	// A table's id is its index among the OCCUPIED .table rows. tmm allocates
	// .table in slabs and does not necessarily fill one before starting the
	// next, so the raw slot index drifts from the true tableid wherever a slab
	// is partially used — measured on a live segment: pool_member_stat sat at
	// slot 1067 but is tableid 171, virtual_server_stat at 2084 vs 292 (3577
	// slots for 421 real tables). Since .column joins on the true tableid,
	// counting empty slots silently attaches every later table's columns to the
	// wrong table (or to none), which is why the per-object tables looked
	// column-less and empty. Skipping unoccupied slots restores the identity
	// exactly (verified: tmm_stat 7, interface_stat 43, pool_member_stat 171,
	// virtual_server_stat 292).
	tmeta := bootstrapTable(tidTable)
	var tables []*Table
	s.walkSlabRows(tidTable, -1, func(raw []byte) {
		r := Row{tbl: tmeta, raw: raw}
		name := r.Str("name")
		if name == "" {
			return // unused slot in a partially-filled slab, not a table
		}
		t := &Table{
			Name:    name,
			ID:      len(tables),
			Rows:    int(r.Uint("rows")),
			RowSize: int(r.Uint("rowsz")),
			Cols:    int(r.Uint("cols")),
		}
		tables = append(tables, t)
	})
	if len(tables) == 0 {
		return fmt.Errorf("tmstat: no tables found")
	}
	for _, t := range tables {
		s.order = append(s.order, t)
		s.byID[t.ID] = t
		if t.Name != "" {
			s.byName[t.Name] = t
		}
	}

	// .column: attach each column to its owning table (by tableid).
	// Walk every .column slab for the same reason: .column's own live row count
	// lags on a running tmm (694 reported vs ~9k real columns), and stopping
	// there truncates the tail of each table's column list — interface_stat kept
	// only its first 4 of 81 columns, so counters.bytes_in/pkts_in never made it
	// out. Unoccupied slots are skipped by name, as above; without that they
	// would all read tableid 0 and pollute .table's own schema.
	cmeta := bootstrapTable(tidColumn)
	s.walkSlabRows(tidColumn, -1, func(raw []byte) {
		r := Row{tbl: cmeta, raw: raw}
		if r.Str("name") == "" {
			return
		}
		tid := int(r.Uint("tableid"))
		t := s.byID[tid]
		if t == nil {
			return
		}
		t.Columns = append(t.Columns, Column{
			Name:    r.Str("name"),
			Offset:  int(r.Uint("offset")),
			Size:    int(r.Uint("size")),
			Type:    int(r.Uint("type")),
			Rule:    int(r.Uint("rule")),
			Dropped: r.Uint("dropped") != 0,
		})
	})
	return nil
}

// walkSlabRows invokes fn for each row of the given tableid, in slab/file order.
// If maxRows >= 0 it stops after that many rows; -1 reads every slab fully.
func (s *Segment) walkSlabRows(tableid, maxRows int, fn func(raw []byte)) {
	got := 0
	for sl := 0; sl < s.slabCount; sl++ {
		base := sl * s.slabSize
		if base+slabHdrSize > len(s.data) || string(s.data[base:base+4]) != magic {
			continue
		}
		if int(binary.LittleEndian.Uint16(s.data[base+4:])) != tableid {
			continue
		}
		stride := int(binary.LittleEndian.Uint16(s.data[base+6:])) * lineSize
		if stride <= 0 {
			continue
		}
		perSlab := (s.slabSize - slabHdrSize) / stride
		for r := 0; r < perSlab; r++ {
			if maxRows >= 0 && got >= maxRows {
				return
			}
			off := base + slabHdrSize + r*stride
			if off+stride > len(s.data) {
				return
			}
			fn(s.data[off : off+stride])
			got++
		}
	}
}

// Tables returns every table's metadata, in tableid order.
func (s *Segment) Tables() []*Table { return s.order }

// Table returns the named table's metadata, or nil.
func (s *Segment) Table(name string) *Table { return s.byName[name] }

// Rows decodes the live rows of the named table, key-aggregated the way tmm's
// stats subsystem (and tmctl) present them: rows sharing all key columns
// (rule 0) are merged into one, with counter columns (rule 2) summed. This is
// what a Prometheus exporter wants — one row per distinct label set. Use RawRows
// for the unaggregated rows.
func (s *Segment) Rows(name string) ([]Row, error) {
	raw, err := s.RawRows(name)
	if err != nil {
		return nil, err
	}
	return s.aggregate(s.byName[name], raw), nil
}

// RawRows decodes every live row of the named table without key aggregation.
func (s *Segment) RawRows(name string) ([]Row, error) {
	t := s.byName[name]
	if t == nil {
		return nil, fmt.Errorf("tmstat: no such table %q", name)
	}
	// Walk every slab and skip unoccupied slots rather than taking the first
	// t.Rows in file order: a table's slabs need not be filled front-to-back, so
	// leading empty slots would otherwise consume the whole row budget and the
	// real rows would never be reached. Measured on a live segment,
	// pool_member_stat returned 43 all-zero rows that key-aggregated into a
	// single {pool_name="",addr="::"} row while its 43 real members sat further
	// in. t.Rows is still honoured as the live row count once empties are gone.
	rows := make([]Row, 0, t.Rows)
	s.walkSlabRows(t.ID, -1, func(raw []byte) {
		if t.Rows > 0 && len(rows) >= t.Rows {
			return
		}
		if allZero(raw) {
			return // unoccupied slot
		}
		// Copy the row so callers may retain it past the next read.
		b := make([]byte, len(raw))
		copy(b, raw)
		rows = append(rows, Row{tbl: t, raw: b})
	})
	return rows, nil
}

// aggregate merges rows that share all key columns (rule 0), summing counter
// columns (rule 2) and keeping the first value for the rest, preserving
// first-occurrence order. Tables with no key columns are returned unmerged.
func (s *Segment) aggregate(t *Table, rows []Row) []Row {
	var keyCols []Column
	for _, c := range t.Columns {
		if c.Rule == ruleKey && !c.Dropped {
			keyCols = append(keyCols, c)
		}
	}
	if len(keyCols) == 0 {
		return rows
	}
	index := map[string]int{} // key -> position in out
	var out []Row
	for _, r := range rows {
		var kb strings.Builder
		for _, c := range keyCols {
			kb.WriteString(r.Value(c))
			kb.WriteByte(0)
		}
		key := kb.String()
		if pos, ok := index[key]; ok {
			// Merge into the existing row: sum counter columns in place.
			dst := out[pos].raw
			for _, c := range t.Columns {
				if c.Rule == ruleCounter && c.Type != typeString && c.Type != typeAddress {
					setUint(dst, c.Offset, c.Size, leUint(dst, c.Offset, c.Size)+r.Uint(c.Name))
				}
			}
			continue
		}
		index[key] = len(out)
		out = append(out, r)
	}
	return out
}

// col looks up a column by name in this row's table.
func (r Row) col(name string) (Column, bool) {
	for _, c := range r.tbl.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

// Uint reads an unsigned integer column (0 if absent / out of range).
func (r Row) Uint(name string) uint64 {
	c, ok := r.col(name)
	if !ok {
		return 0
	}
	return leUint(r.raw, c.Offset, c.Size)
}

// Str reads a string column (NUL-terminated).
func (r Row) Str(name string) string {
	c, ok := r.col(name)
	if !ok {
		return ""
	}
	return cstr(r.raw, c.Offset, c.Size)
}

// Float returns a numeric column's value as a float64 (for a metric sample).
// Non-numeric columns return 0.
func (r Row) Float(c Column) float64 {
	switch c.Type {
	case typeSignedInt:
		return float64(leInt(r.raw, c.Offset, c.Size))
	case typeUnsignedInt:
		return float64(leUint(r.raw, c.Offset, c.Size))
	default:
		return 0
	}
}

// Value renders any column exactly as tmctl's CSV would.
func (r Row) Value(c Column) string {
	if c.Offset+c.Size > len(r.raw) {
		return ""
	}
	switch c.Type {
	case typeString:
		return cstr(r.raw, c.Offset, c.Size)
	case typeAddress:
		return addrHex(r.raw[c.Offset : c.Offset+c.Size])
	case typeSignedInt:
		return strconv.FormatInt(leInt(r.raw, c.Offset, c.Size), 10)
	default: // typeUnsignedInt and anything integer-like
		return strconv.FormatUint(leUint(r.raw, c.Offset, c.Size), 10)
	}
}

// CSV renders the whole table as tmctl -c would: a header line of column names
// followed by one comma-separated line per row.
func (s *Segment) CSV(name string) (string, error) {
	rows, err := s.Rows(name)
	if err != nil {
		return "", err
	}
	t := s.byName[name]
	var b strings.Builder
	for i, c := range t.Columns {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(c.Name)
	}
	b.WriteByte('\n')
	for _, row := range rows {
		for i, c := range t.Columns {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(row.Value(c))
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func setUint(b []byte, off, size int, v uint64) {
	if off+size > len(b) {
		return
	}
	for i := 0; i < size; i++ {
		b[off+i] = byte(v >> (8 * i))
	}
}

func leUint(b []byte, off, size int) uint64 {
	if off+size > len(b) {
		return 0
	}
	var v uint64
	for i := 0; i < size; i++ {
		v |= uint64(b[off+i]) << (8 * i)
	}
	return v
}

func leInt(b []byte, off, size int) int64 {
	u := leUint(b, off, size)
	if size < 8 && size > 0 {
		shift := uint(64 - 8*size)
		return int64(u<<shift) >> shift // sign-extend
	}
	return int64(u)
}

func cstr(b []byte, off, size int) string {
	if off+size > len(b) {
		size = len(b) - off
	}
	s := b[off : off+size]
	if i := indexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return string(s)
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// addrHex renders a byte address as uppercase colon-separated hex, matching
// tmctl (e.g. "FE:80:00:...:00").
func addrHex(b []byte) string {
	const hex = "0123456789ABCDEF"
	if len(b) == 0 {
		return ""
	}
	out := make([]byte, 0, len(b)*3-1)
	for i, x := range b {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hex[x>>4], hex[x&0xf])
	}
	return string(out)
}

// allZero reports whether a raw row is entirely zero bytes — tmstat's marker for
// an unoccupied slot inside an allocated slab.
func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
