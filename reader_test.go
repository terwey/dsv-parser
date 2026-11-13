// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Yorick Terweijden

package dsv

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
