// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Yorick Terweijden

package dsv

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/exp/slices"

	"github.com/terwey/dsv-parser/util"
)

func readAll(t *testing.T, r *Reader) [][]string {
	t.Helper()
	defer r.Close()

	var records [][]string
	for {
		record, err := r.ReadStrings()
		if err == io.EOF {
			return records
		}
		if err != nil {
			t.Fatalf("ReadStrings(): %s", err)
		}
		records = append(records, record)
	}
}

func TestReader_Read(t *testing.T) {
	tests := []struct {
		name  string
		input string
		opts  []Option
		want  [][]string
	}{
		{
			name:  "detects pipe and quotation mark",
			input: "\"id\"|\"name\"\n\"1\"|\"Foo | Bar\"\n\"2\"|\"Baz\"\n",
			want:  [][]string{{"id", "name"}, {"1", "Foo | Bar"}, {"2", "Baz"}},
		},
		{
			name:  "line break inside an enclosure",
			input: "id,text\r\n1,\"two\r\nlines\"\r\n2,one\r\n",
			want:  [][]string{{"id", "text"}, {"1", "two\nlines"}, {"2", "one"}},
		},
		{
			name:  "doubled enclosure",
			input: "id,text\n1,\"say \"\"hi\"\"\"\n",
			want:  [][]string{{"id", "text"}, {"1", `say "hi"`}},
		},
		{
			name:  "byte order mark and unnamed column",
			input: "\xEF\xBB\xBFid;;name\n1;2;3\n",
			want:  [][]string{{"id", "empty_column", "name"}, {"1", "2", "3"}},
		},
		{
			name:  "excel export ends every line with a delimiter",
			input: "id,name,\n1,foo,\n2,bar,\n",
			want:  [][]string{{"id", "name"}, {"1", "foo"}, {"2", "bar"}},
		},
		{
			name:  "unnamed last column with values",
			input: "id,name,\n1,foo,x\n",
			want:  [][]string{{"id", "name", "empty_column"}, {"1", "foo", "x"}},
		},
		{
			name:  "header ending with a delimiter and no rows",
			input: "id,name,\n",
			want:  [][]string{{"id", "name"}},
		},
		{
			name:  "rows without a value are skipped",
			input: "id,name\n,\n1,foo\n",
			want:  [][]string{{"id", "name"}, {"1", "foo"}},
		},
		{
			name:  "literal multi-byte delimiter",
			input: "id<#>name\n1<#>foo, \"bar\"\n",
			opts:  []Option{WithDelimiter([]byte("<#>"))},
			want:  [][]string{{"id", "name"}, {"1", `foo, "bar"`}},
		},
		{
			name:  "newline delimited keeps a line break as the end of a record",
			input: "id,text\n1,\"two\nlines\"\n",
			opts:  []Option{WithNewlineDelimited(), WithEnclosure([]byte(`"`))},
			want:  [][]string{{"id", "text"}, {"1", `"two`}, {`lines"`}},
		},
		{
			name:  "header on the third line, empty lines not counted",
			input: "export 2020\n\nsheet 1\nid,name\n1,foo\n",
			opts:  []Option{WithStart(3)},
			want:  [][]string{{"id", "name"}, {"1", "foo"}},
		},
		{
			name:  "excluded column",
			input: "id,secret,name\n1,x,foo\n",
			opts:  []Option{WithExclude(1)},
			want:  [][]string{{"id", "name"}, {"1", "foo"}},
		},
		{
			name:  "empty input",
			input: "",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := readAll(t, NewReader(strings.NewReader(tt.input), append(tt.opts, WithOrder())...))
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Read() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestReader_Read_recordTooLong(t *testing.T) {
	r := NewReader(strings.NewReader("id,name\n1,"+strings.Repeat("x", 1024)+"\n"), WithBufferSize(64))
	defer r.Close()

	var err error
	for err == nil {
		_, err = r.Read()
	}
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Errorf("Read() error = %v, want %v", err, bufio.ErrTooLong)
	}
}

// endless is input that never ends.
type endless struct {
	header bool
}

func (e *endless) Read(p []byte) (int, error) {
	if !e.header {
		e.header = true
		return copy(p, "id,name\n"), nil
	}
	n := 0
	for n+6 <= len(p) {
		n += copy(p[n:], "1,foo\n")
	}
	return n, nil
}

// wholeRows is input that hands out whole rows only: a Read with room for
// less than a row returns no bytes.
type wholeRows struct {
	header bool
	rows   int
}

func (w *wholeRows) Read(p []byte) (int, error) {
	if !w.header {
		w.header = true
		return copy(p, "id,name\n"), nil
	}
	if w.rows == 0 {
		return 0, io.EOF
	}
	n := 0
	for n+6 <= len(p) && w.rows > 0 {
		n += copy(p[n:], "1,foo\n")
		w.rows--
	}
	return n, nil
}

func TestRegressionReader_Read_shortBufferTail(t *testing.T) {
	// the rows leave two bytes of the first buffer free, which wholeRows does not fill
	const rows = 30000
	r := NewReader(&wholeRows{rows: rows}, WithBufferSize(64*1024))
	defer r.Close()

	records := 0
	for {
		_, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read() after %d records: %s", records, err)
		}
		records++
	}
	if records != rows+1 {
		t.Errorf("Read() returned %d records, want %d", records, rows+1)
	}
}

func TestReader_Close_beforeEOF(t *testing.T) {
	r := NewReader(&endless{}, WithBufferSize(64*1024))
	for i := 0; i < 1000; i++ {
		if _, err := r.Read(); err != nil {
			t.Fatalf("Read(): %s", err)
		}
	}

	if err := r.Close(); err != nil {
		t.Errorf("Close() = %v", err)
	}
	if _, err := r.Read(); !errors.Is(err, ErrClosed) {
		t.Errorf("Read() after Close error = %v, want %v", err, ErrClosed)
	}
}

func TestReader_Read_afterError(t *testing.T) {
	tests := []struct {
		name  string
		input string
		opts  []Option
		want  error
	}{
		{
			name:  "empty input",
			input: "",
			want:  io.EOF,
		},
		{
			name:  "header longer than the buffer",
			input: strings.Repeat("x", 1024) + "\n",
			opts:  []Option{WithBufferSize(64)},
			want:  bufio.ErrTooLong,
		},
		{
			name:  "buffer size below 1",
			input: "id,name\n1,foo\n",
			opts:  []Option{WithBufferSize(-1)},
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewReader(strings.NewReader(tt.input), tt.opts...)
			defer r.Close()

			_, first := r.Read()
			if first == nil || (tt.want != nil && !errors.Is(first, tt.want)) {
				t.Fatalf("Read() error = %v, want %v", first, tt.want)
			}
			if _, second := r.Read(); second != first {
				t.Errorf("second Read() error = %v, want %v", second, first)
			}
		})
	}
}

// sortRows sorts the records after the header.
func sortRows(records [][]string) {
	if len(records) < 2 {
		return
	}
	slices.SortFunc(records[1:], func(a, b []string) bool {
		return slices.Compare(a, b) < 0
	})
}

// collected is what the collectors of one Run received.
type collected struct {
	mu      sync.Mutex
	records [][]string
	rows    []uint64
	closed  int
}

// collector is a Handler that keeps the records of one worker.
type collector struct {
	into *collected
	fail error
}

func (c *collector) Record(row uint64, flat []byte, lengths []uint32) error {
	if c.fail != nil {
		return c.fail
	}

	var record []string
	for _, field := range util.FlatToMulti(flat, lengths) {
		record = append(record, string(field))
	}

	c.into.mu.Lock()
	c.into.records = append(c.into.records, record)
	c.into.rows = append(c.into.rows, row)
	c.into.mu.Unlock()
	return nil
}

func (c *collector) Close() error {
	c.into.mu.Lock()
	c.into.closed++
	c.into.mu.Unlock()
	return nil
}

func TestReader_Run_handlerError(t *testing.T) {
	want := errors.New("disk full")
	var into collected

	r := NewReader(&endless{}, WithBufferSize(64*1024), WithWorkers(4))
	defer r.Close()

	err := r.Run(func() (Handler, error) {
		return &collector{into: &into, fail: want}, nil
	})
	if !errors.Is(err, want) {
		t.Errorf("Run() error = %v, want %v", err, want)
	}
	if into.closed != 4 {
		t.Errorf("Run() closed %d handlers, want 4", into.closed)
	}
}

func runAll(t *testing.T, r *Reader, workers int) [][]string {
	t.Helper()
	defer r.Close()

	header, err := r.Header()
	if err != nil {
		t.Fatalf("Header(): %s", err)
	}
	names := make([]string, len(header))
	for i := range header {
		names[i] = string(header[i])
	}

	var into collected
	err = r.Run(func() (Handler, error) {
		return &collector{into: &into}, nil
	})
	if err != nil {
		t.Fatalf("Run(): %s", err)
	}
	if into.closed != workers {
		t.Errorf("Run() closed %d handlers, want %d", into.closed, workers)
	}

	slices.Sort(into.rows)
	if reused := len(into.records) - len(slices.Compact(into.rows)); reused != 0 {
		t.Errorf("Run() gave %d records a row number already used", reused)
	}

	return append([][]string{names}, into.records...)
}

// failAt is a Handler that fails on one row.
type failAt struct {
	row uint64
}

func (f failAt) Record(row uint64, flat []byte, lengths []uint32) error {
	if row == f.row {
		return errors.New("disk full")
	}
	return nil
}

func (f failAt) Close() error { return nil }

func TestReader_Offset(t *testing.T) {
	// many returns n records, each format filled in with its number
	many := func(format string, n int) []string {
		records := make([]string, n)
		for i := range records {
			records[i] = fmt.Sprintf(format, i)
		}
		return records
	}

	tests := []struct {
		name string
		// head is the header and the lines before it
		head string
		// records are whole records, each with its line ending
		records []string
		// tail is an unfinished record; the input ends in io.ErrUnexpectedEOF after it
		tail string
		opts []Option
		// run reads with Run instead of Read
		run bool
		// failRow is the row the Handler of Run fails on
		failRow uint64
		// exact is set when Offset is past the last record delivered, not
		// only at the end of a record
		exact bool
	}{
		{
			name:    "records across buffer refills",
			head:    "id,name\n",
			records: many("%d,foo\n", 200),
			opts:    []Option{WithBufferSize(64), WithWorkers(4), WithOrder()},
			exact:   true,
		},
		{
			name:    "line break inside an enclosure",
			head:    "id,text\n",
			records: many("%d,\"two\nlines\"\n", 100),
			opts:    []Option{WithBufferSize(64), WithWorkers(4), WithOrder()},
			exact:   true,
		},
		{
			// a CR at the end of a buffer is read as the line ending: terwey/dsv-parser#4
			name:    "CRLF inside an enclosure",
			head:    "id,text\r\n",
			records: many("%d,\"two\r\nlines\"\r\n", 100),
			opts:    []Option{WithWorkers(4), WithOrder()},
			exact:   true,
		},
		{
			name:    "last record without a line ending",
			head:    "id,name\n",
			records: []string{"1,foo\n", "2,bar"},
			opts:    []Option{WithOrder()},
			exact:   true,
		},
		{
			name:    "header on the third line",
			head:    "export 2020\n\nsheet 1\nid,name\n",
			records: many("%d,foo\n", 50),
			opts:    []Option{WithStart(3), WithBufferSize(64), WithOrder()},
			exact:   true,
		},
		{
			name:    "excluded column",
			head:    "id,secret,name\n",
			records: many("%d,x,foo\n", 50),
			opts:    []Option{WithExclude(1), WithBufferSize(64), WithOrder()},
			exact:   true,
		},
		{
			name:    "stream cut inside an enclosure",
			head:    "id,text\n",
			records: many("%d,\"two\nlines\"\n", 100),
			tail:    "100,\"two\nli",
			opts:    []Option{WithBufferSize(64), WithWorkers(4), WithOrder()},
			exact:   true,
		},
		{
			name:    "unordered",
			head:    "id,name\n",
			records: many("%d,foo\n", 200),
			opts:    []Option{WithBufferSize(64), WithWorkers(4)},
		},
		{
			name:    "unordered stream cut",
			head:    "id,name\n",
			records: many("%d,foo\n", 200),
			tail:    "200,fo",
			opts:    []Option{WithBufferSize(64), WithWorkers(4)},
		},
		{
			name:    "run",
			head:    "id,name\n",
			records: many("%d,foo\n", 200),
			opts:    []Option{WithBufferSize(64), WithWorkers(4)},
			run:     true,
		},
		{
			name:    "run, stream cut",
			head:    "id,name\n",
			records: many("%d,foo\n", 200),
			tail:    "200,fo",
			opts:    []Option{WithBufferSize(64), WithWorkers(4)},
			run:     true,
		},
		{
			name:    "run, handler fails on row 30",
			head:    "id,name\n",
			records: many("%d,foo\n", 200),
			opts:    []Option{WithBufferSize(64), WithWorkers(1)},
			run:     true,
			failRow: 30,
			exact:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// ends[i] is the offset past the first i records
			ends := []int64{int64(len(tt.head))}
			for _, record := range tt.records {
				ends = append(ends, ends[len(ends)-1]+int64(len(record)))
			}
			boundary := func(offset int64) bool {
				_, found := slices.BinarySearch(ends, offset)
				return found
			}

			var input io.Reader = strings.NewReader(tt.head + strings.Join(tt.records, "") + tt.tail)
			if tt.tail != "" {
				input = io.MultiReader(input, iotest.ErrReader(io.ErrUnexpectedEOF))
			}
			r := NewReader(input, tt.opts...)
			defer r.Close()

			// delivered counts the records delivered in input order
			delivered := len(tt.records)
			var err error
			if tt.run {
				err = r.Run(func() (Handler, error) {
					return failAt{row: tt.failRow}, nil
				})
				if tt.failRow != 0 {
					delivered = int(tt.failRow) - 1
				}
			} else {
				previous := int64(0)
				for i := -1; ; i++ {
					if _, err = r.Read(); err != nil {
						delivered = i
						break
					}
					got := r.Offset()
					if tt.exact && got != ends[i+1] {
						t.Fatalf("Offset() after record %d = %d, want %d", i+1, got, ends[i+1])
					}
					if !boundary(got) || got < previous {
						t.Fatalf("Offset() after record %d = %d, after %d: not the end of a later record", i+1, got, previous)
					}
					previous = got
				}
				if err == io.EOF {
					err = nil
				}
			}

			wantErr := tt.tail != "" || tt.failRow != 0
			if (err != nil) != wantErr || (tt.tail != "" && !errors.Is(err, io.ErrUnexpectedEOF)) {
				t.Fatalf("reading ended in %v, want an error: %t", err, wantErr)
			}
			got := r.Offset()
			if !wantErr || tt.exact {
				if got != ends[delivered] {
					t.Errorf("Offset() at the end = %d, want %d", got, ends[delivered])
				}
			} else if !boundary(got) {
				t.Errorf("Offset() at the end = %d, not the end of a record", got)
			}
		})
	}
}

func TestProgress_deliver(t *testing.T) {
	type delivery struct {
		seq   uint64
		end   int64
		whole bool
	}
	tests := []struct {
		name       string
		deliveries []delivery
		want       int64
	}{
		{
			name:       "a batch ahead waits for the one before it",
			deliveries: []delivery{{1, 20, true}},
			want:       5,
		},
		{
			name:       "batches out of order",
			deliveries: []delivery{{2, 30, true}, {1, 20, true}, {0, 10, true}},
			want:       30,
		},
		{
			name:       "a batch delivered in part stops the batches after it",
			deliveries: []delivery{{0, 8, false}, {1, 20, true}},
			want:       8,
		},
		{
			name:       "a batch ahead delivered in part",
			deliveries: []delivery{{2, 30, true}, {1, 15, false}, {0, 10, true}},
			want:       15,
		},
		{
			name:       "no record of the batch delivered",
			deliveries: []delivery{{0, 0, false}, {1, 20, true}},
			want:       5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := progress{offset: 5}
			for _, d := range tt.deliveries {
				p.deliver(d.seq, d.end, d.whole)
			}
			if p.offset != tt.want {
				t.Errorf("offset = %d, want %d", p.offset, tt.want)
			}
		})
	}
}

// wantRecords reads the records a fixture holds: one JSON array of strings per
// line, the header first, in input order.
func wantRecords(t *testing.T, path string) [][]string {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var records [][]string
	dec := json.NewDecoder(f)
	for dec.More() {
		var record []string
		if err := dec.Decode(&record); err != nil {
			t.Fatalf("%s: %s", path, err)
		}
		records = append(records, record)
	}
	return records
}

// smallBuffer holds the longest record of the fixtures, and little more.
const smallBuffer = 16 * 1024

func TestReader_fixtures(t *testing.T) {
	paths, err := filepath.Glob("tests/*.csv")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no fixtures in tests")
	}

	open := func(t *testing.T, path string) *os.File {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			want := wantRecords(t, path+".jsonl")
			if len(want) < 2 {
				t.Fatalf("%s.jsonl holds %d records", path, len(want))
			}

			ordered := readAll(t, NewReader(open(t, path), WithOrder(), WithWorkers(4)))
			if diff := cmp.Diff(want, ordered); diff != "" {
				t.Errorf("Read() with WithOrder mismatch (-want +got):\n%.4000s", diff)
			}

			// a small buffer moves the records across many buffers
			small := readAll(t, NewReader(open(t, path), WithOrder(), WithWorkers(4), WithBufferSize(smallBuffer)))
			if diff := cmp.Diff(want, small); diff != "" {
				t.Errorf("Read() with a %d byte buffer mismatch (-want +got):\n%.4000s", smallBuffer, diff)
			}

			sortRows(want)

			unordered := readAll(t, NewReader(open(t, path), WithWorkers(4)))
			sortRows(unordered)
			if diff := cmp.Diff(want, unordered); diff != "" {
				t.Errorf("Read() mismatch (-want +got):\n%.4000s", diff)
			}

			handled := runAll(t, NewReader(open(t, path), WithWorkers(4)), 4)
			sortRows(handled)
			if diff := cmp.Diff(want, handled); diff != "" {
				t.Errorf("Run() mismatch (-want +got):\n%.4000s", diff)
			}
		})
	}
}

// loop is an input of a header followed by the same rows, times over.
type loop struct {
	header []byte
	rows   []byte
	times  int
	rest   []byte
}

func (l *loop) Read(p []byte) (int, error) {
	if len(l.rest) == 0 {
		switch {
		case l.header != nil:
			l.rest, l.header = l.header, nil
		case l.times > 0:
			l.rest = l.rows
			l.times--
		default:
			return 0, io.EOF
		}
	}

	n := copy(p, l.rest)
	l.rest = l.rest[n:]
	return n, nil
}

// tally is a Handler that counts the records and fields of one worker and
// adds them to the totals when it is closed.
type tally struct {
	records, fields           uint64
	totalRecords, totalFields *uint64
}

func (c *tally) Record(row uint64, flat []byte, lengths []uint32) error {
	c.records++
	c.fields += uint64(len(lengths))
	return nil
}

func (c *tally) Close() error {
	atomic.AddUint64(c.totalRecords, c.records)
	atomic.AddUint64(c.totalFields, c.fields)
	return nil
}

// form is the shape of generated rows.
type form int

const (
	// plain rows have no enclosure and no delimiter or line break in a field
	plain form = iota
	// enclosedOneLine rows enclose every field; every fifth field holds a
	// delimiter and a doubled enclosure
	enclosedOneLine
	// enclosed rows are enclosedOneLine rows with a line break in every fifth field
	enclosed
)

// generateRows returns a header and about a mebibyte of comma separated rows
// with the given number of columns: numbers, words, decimals, dates and text.
func generateRows(columns int, f form) (header, rows []byte, count int) {
	words := []string{"north", "south", "east", "west", "upper", "lower", "inner", "outer"}

	field := func(b []byte, row, column int) []byte {
		if f != plain {
			b = append(b, '"')
		}
		switch column % 5 {
		case 0:
			b = strconv.AppendInt(b, int64(row*31+column), 10)
		case 1:
			b = append(b, words[(row+column)%len(words)]...)
		case 2:
			b = strconv.AppendInt(b, int64((row*7+column)%1000), 10)
			b = append(b, '.')
			b = strconv.AppendInt(b, int64(10+(row+column)%90), 10)
		case 3:
			b = append(b, "2021-08-"...)
			b = strconv.AppendInt(b, int64(10+(row+column)%19), 10)
		case 4:
			b = append(b, words[row%len(words)]...)
			switch f {
			case plain:
				b = append(b, " plain and whole "...)
			case enclosedOneLine:
				b = append(b, ", \"\"quoted\"\" and whole "...)
			case enclosed:
				b = append(b, ", \"\"quoted\"\" and\nbroken "...)
			}
			b = append(b, words[column%len(words)]...)
		}
		if f != plain {
			b = append(b, '"')
		}
		return b
	}

	for column := 0; column < columns; column++ {
		if column != 0 {
			header = append(header, ',')
		}
		header = append(header, 'c')
		header = strconv.AppendInt(header, int64(column), 10)
	}
	header = append(header, '\n')

	for len(rows) < 1024*1024 {
		for column := 0; column < columns; column++ {
			if column != 0 {
				rows = append(rows, ',')
			}
			rows = field(rows, count, column)
		}
		rows = append(rows, '\n')
		count++
	}

	return header, rows, count
}

// readerCase is a case of the Reader benchmarks: the shape of the rows, and
// what the caller told the Reader about them.
type readerCase struct {
	name string
	form form
	opts []Option
}

func readerCases() []readerCase {
	comma, quote := []byte(","), []byte(`"`)

	return []readerCase{
		// every field enclosed, line breaks inside fields
		{name: "Enclosed/AutoDetect", form: enclosed},
		{name: "Enclosed/DelimiterAndEnclosureSet", form: enclosed, opts: []Option{WithDelimiter(comma), WithEnclosure(quote)}},

		// every field enclosed, no line break inside a field
		{name: "EnclosedOneLine/AutoDetect", form: enclosedOneLine},
		{name: "EnclosedOneLine/DelimiterAndEnclosureSet", form: enclosedOneLine, opts: []Option{WithDelimiter(comma), WithEnclosure(quote)}},
		{name: "EnclosedOneLine/EnclosureSetNewlineDelimited", form: enclosedOneLine, opts: []Option{WithDelimiter(comma), WithEnclosure(quote), WithNewlineDelimited()}},

		// no enclosure in the rows
		// AutoDetect never finds an enclosure, so every record is searched for one
		{name: "NoEnclosure/AutoDetect", form: plain},
		{name: "NoEnclosure/EnclosureSet", form: plain, opts: []Option{WithDelimiter(comma), WithEnclosure(quote)}},
		{name: "NoEnclosure/NoEnclosureSet", form: plain, opts: []Option{WithDelimiter(comma), WithEnclosure(nil)}},
		{name: "NoEnclosure/NewlineDelimited", form: plain, opts: []Option{WithNewlineDelimited()}},
	}
}

// BenchmarkReader_Run reads generated rows b.N times as one input and counts
// the records. MB/s is the speed of cutting and splitting. The cases are
// named Rows/Options/columns.
func BenchmarkReader_Run(b *testing.B) {
	for _, bb := range readerCases() {
		for _, columns := range []int{20, 200} {
			b.Run(bb.name+"/"+strconv.Itoa(columns)+"_columns", func(b *testing.B) {
				header, rows, count := generateRows(columns, bb.form)

				b.SetBytes(int64(len(rows)))
				b.ReportAllocs()
				b.ResetTimer()

				r := NewReader(&loop{header: header, rows: rows, times: b.N}, bb.opts...)
				defer r.Close()

				var records, fields uint64
				err := r.Run(func() (Handler, error) {
					return &tally{totalRecords: &records, totalFields: &fields}, nil
				})
				if err != nil {
					b.Fatal(err)
				}

				b.StopTimer()
				wantRecords := uint64(count) * uint64(b.N)
				if records != wantRecords || fields != wantRecords*uint64(columns) {
					b.Fatalf("Run() handed out %d records and %d fields, want %d and %d",
						records, fields, wantRecords, wantRecords*uint64(columns))
				}
			})
		}
	}
}

// BenchmarkReader_ReadOneCPU runs the cases of BenchmarkReader_Run with one
// worker on one CPU, and takes the records from Read.
func BenchmarkReader_ReadOneCPU(b *testing.B) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))

	for _, bb := range readerCases() {
		for _, columns := range []int{20, 200} {
			b.Run(bb.name+"/"+strconv.Itoa(columns)+"_columns", func(b *testing.B) {
				header, rows, count := generateRows(columns, bb.form)
				opts := append([]Option{WithWorkers(1)}, bb.opts...)

				b.SetBytes(int64(len(rows)))
				b.ReportAllocs()
				b.ResetTimer()

				r := NewReader(&loop{header: header, rows: rows, times: b.N}, opts...)
				defer r.Close()

				var records, fields uint64
				for {
					record, err := r.Read()
					if err == io.EOF {
						break
					}
					if err != nil {
						b.Fatal(err)
					}
					records++
					fields += uint64(len(record))
				}

				b.StopTimer()
				// the header is a record here
				wantRecords := uint64(count)*uint64(b.N) + 1
				if records != wantRecords || fields != wantRecords*uint64(columns) {
					b.Fatalf("Read() returned %d records and %d fields, want %d and %d",
						records, fields, wantRecords, wantRecords*uint64(columns))
				}
			})
		}
	}
}

// BenchmarkReader_ReadOffset is the NoEnclosure/NewlineDelimited/20_columns
// case of BenchmarkReader_ReadOneCPU, the one with the most records per byte,
// with Offset called after every Read.
func BenchmarkReader_ReadOffset(b *testing.B) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))

	header, rows, count := generateRows(20, plain)

	b.SetBytes(int64(len(rows)))
	b.ReportAllocs()
	b.ResetTimer()

	r := NewReader(&loop{header: header, rows: rows, times: b.N}, WithWorkers(1), WithNewlineDelimited())
	defer r.Close()

	records := 0
	var offset int64
	for {
		_, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			b.Fatal(err)
		}
		records++
		offset = r.Offset()
	}

	b.StopTimer()
	if want := count*b.N + 1; records != want {
		b.Fatalf("Read() returned %d records, want %d", records, want)
	}
	if want := int64(len(header) + len(rows)*b.N); offset != want {
		b.Fatalf("Offset() = %d, want %d", offset, want)
	}
}
