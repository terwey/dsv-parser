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
			name:  "skips the first records after the header",
			input: "a,b\n1,2\n3,4\n5,6\n7,8\n",
			opts:  []Option{WithSkip(2)},
			want:  [][]string{{"a", "b"}, {"5", "6"}, {"7", "8"}},
		},
		{
			name:  "stops after the limit",
			input: "a,b\n1,2\n3,4\n5,6\n7,8\n",
			opts:  []Option{WithLimit(2)},
			want:  [][]string{{"a", "b"}, {"1", "2"}, {"3", "4"}},
		},
		{
			name:  "limit counts from the first record that is not skipped",
			input: "a,b\n1,2\n3,4\n5,6\n7,8\n",
			opts:  []Option{WithSkip(1), WithLimit(2)},
			want:  [][]string{{"a", "b"}, {"3", "4"}, {"5", "6"}},
		},
		{
			name:  "limit of zero returns the header alone",
			input: "a,b\n1,2\n",
			opts:  []Option{WithLimit(0)},
			want:  [][]string{{"a", "b"}},
		},
		{
			name:  "header ending with a delimiter is judged by the first record that is not skipped",
			input: "a,b,\n1,2,\n3,4,\n5,6,x\n",
			opts:  []Option{WithSkip(2)},
			want:  [][]string{{"a", "b", "empty_column"}, {"5", "6", "x"}},
		},
		{
			name:  "line ending set to CRLF reads a LF as data",
			input: "a,b\n1,2\r\n3,4\r\n5,6\n",
			opts:  []Option{WithLineEnding([]byte("\r\n"))},
			want:  [][]string{{"a", "b 1", "2"}, {"3", "4"}, {"5", "6\n"}},
		},
		{
			name:  "line ending follows the file",
			input: "\"a\",\"b\"\r\n\"1\",\"x\r\ny\"\n\"3\",\"4\"\n\"5\",\"6\"\n",
			want:  [][]string{{"a", "b"}, {"1", "x\ny"}, {"3", "4"}, {"5", "6"}},
		},
		{
			name:  "fixed line ending does not follow the file",
			input: "\"a\",\"b\"\r\n\"1\",\"x\r\ny\"\n\"3\",\"4\"\n\"5\",\"6\"\n",
			opts:  []Option{WithFixedLineEnding()},
			want:  [][]string{{"a", "b"}, {"1", "x\ny\"\n\"3", "4\"\n\"5", "\"6\"\n"}},
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

func TestReader_Characters(t *testing.T) {
	r := NewReader(strings.NewReader("\"id\"|\"name\"\r\n\"1\"|\"Foo | Bar\"\r\n"))
	readAll(t, r)

	got := r.Characters()
	want := []string{"|", `"`, "\r\n"}
	if diff := cmp.Diff(want, []string{string(got.Delimiter), string(got.Enclosure), string(got.EOL)}); diff != "" {
		t.Errorf("Characters() delimiter, enclosure and line ending mismatch (-want +got):\n%s", diff)
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

func TestRegressionReader_Read_shortReads(t *testing.T) {
	set := []Option{WithDelimiter([]byte(",")), WithEnclosure([]byte(`"`))}

	tests := []struct {
		name  string
		input string
		opts  []Option
		// buffer is the smallest buffer read with: it holds the longest record
		// with its line break, and the bytes after a record its cut looks at
		buffer int
		want   [][]string
	}{
		{
			name:   "line break in an enclosure, LF, detected",
			input:  "id,text\n0,\"two\nlines\"\n1,\"two\nlines\"\n",
			buffer: 14,
			want:   [][]string{{"id", "text"}, {"0", "two\nlines"}, {"1", "two\nlines"}},
		},
		{
			name:   "line break in an enclosure, CRLF, detected",
			input:  "id,text\r\n0,\"two\r\nlines\"\r\n1,\"two\r\nlines\"\r\n",
			buffer: 16,
			want:   [][]string{{"id", "text"}, {"0", "two\nlines"}, {"1", "two\nlines"}},
		},
		{
			name:   "line break in an enclosure, LF, set",
			input:  "id,text\n0,\"two\nlines\"\n1,\"two\nlines\"\n",
			opts:   set,
			buffer: 14,
			want:   [][]string{{"id", "text"}, {"0", "two\nlines"}, {"1", "two\nlines"}},
		},
		{
			name:   "line break in an enclosure, CRLF, set",
			input:  "id,text\r\n0,\"two\r\nlines\"\r\n1,\"two\r\nlines\"\r\n",
			opts:   set,
			buffer: 16,
			want:   [][]string{{"id", "text"}, {"0", "two\nlines"}, {"1", "two\nlines"}},
		},
		{
			name:   "line break in an enclosure, CR, detected",
			input:  "id,text\r0,\"two\rlines\"\r1,one\r",
			buffer: 14,
			want:   [][]string{{"id", "text"}, {"0", "two\nlines"}, {"1", "one"}},
		},
		{
			name:   "apostrophes before the enclosure on a line, LF, detected",
			input:  "id,a,b,c,d\n1,'a',\"b\",\"c\",\"d\"\n",
			buffer: 19,
			want:   [][]string{{"id", "a", "b", "c", "d"}, {"1", "'a'", "b", "c", "d"}},
		},
		{
			name:   "delimiter and line break in an enclosure, CRLF, detected",
			input:  "id;text\r\n1;\"a;b\r\nc\"\r\n2;plain\r\n",
			buffer: 12,
			want:   [][]string{{"id", "text"}, {"1", "a;b\nc"}, {"2", "plain"}},
		},
		// the stray enclosure is told from an open one by the field the next record opens
		{
			name:   "stray enclosure, CRLF, set",
			input:  "id,size\r\n1,55\"\r\n2,\"a,b\r\nc\"\r\n",
			opts:   set,
			buffer: 18,
			want:   [][]string{{"id", "size"}, {"1", `55"`}, {"2", "a,b\nc"}},
		},
		{
			name:   "line break in an enclosed header, CRLF, set",
			input:  "\"id\",\"te\r\nxt\"\r\n1,2\r\n",
			opts:   set,
			buffer: 15,
			want:   [][]string{{"id", "te xt"}, {"1", "2"}},
		},
	}

	readers := []struct {
		name string
		wrap func(io.Reader) io.Reader
	}{
		{name: "whole", wrap: func(r io.Reader) io.Reader { return r }},
		{name: "one byte", wrap: iotest.OneByteReader},
		{name: "half", wrap: iotest.HalfReader},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := append([]Option{WithOrder()}, tt.opts...)
			for _, rd := range readers {
				got := readAll(t, NewReader(rd.wrap(strings.NewReader(tt.input)), opts...))
				if diff := cmp.Diff(tt.want, got); diff != "" {
					t.Errorf("Read() from a %s reader mismatch (-want +got):\n%s", rd.name, diff)
				}
			}

			// the buffer sizes from tt.buffer up end a buffer on every byte of the input
			for size := tt.buffer; size <= len(tt.input); size++ {
				got := readAll(t, NewReader(strings.NewReader(tt.input), append(opts, WithBufferSize(size))...))
				if diff := cmp.Diff(tt.want, got); diff != "" {
					t.Errorf("Read() with a %d byte buffer mismatch (-want +got):\n%s", size, diff)
				}
			}
		})
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

// rowKeeper is a Handler that keeps every record under its row number.
type rowKeeper struct {
	mu   *sync.Mutex
	rows map[uint64][]string
}

func (k rowKeeper) Record(row uint64, flat []byte, lengths []uint32) error {
	record := make([]string, len(lengths))
	for i, n := range lengths {
		record[i] = string(flat[:n])
		flat = flat[n:]
	}
	k.mu.Lock()
	k.rows[row] = record
	k.mu.Unlock()
	return nil
}

func (k rowKeeper) Close() error { return nil }

// cutAt returns input from offset from on. With to inside the input it ends
// there in io.ErrUnexpectedEOF, a stream that broke.
func cutAt(input string, from int64, to int) io.Reader {
	if to >= len(input) {
		return strings.NewReader(input[from:])
	}
	return io.MultiReader(strings.NewReader(input[from:to]), iotest.ErrReader(io.ErrUnexpectedEOF))
}

// stored returns c as a caller gets it back from storage.
func stored(t *testing.T, c Checkpoint) Checkpoint {
	t.Helper()
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("json.Marshal(Checkpoint): %s", err)
	}
	var decoded Checkpoint
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(Checkpoint): %s", err)
	}
	return decoded
}

func TestReader_Checkpoint(t *testing.T) {
	// many returns n records, each format filled in with its number
	many := func(format string, n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, format, i)
		}
		return b.String()
	}

	tests := []struct {
		name  string
		input string
		opts  []Option
		// limited is true when the Reader stops before the end of the input
		limited bool
	}{
		{
			name:  "plain",
			input: "id,name\n" + many("%d,foo\n", 300),
			opts:  []Option{WithBufferSize(64)},
		},
		{
			name:  "line break inside an enclosure",
			input: "id,text\n" + many("%d,\"two\nlines\"\n", 200),
			opts:  []Option{WithBufferSize(128)},
		},
		{
			name:  "CRLF inside an enclosure",
			input: "id,text\r\n" + many("%d,\"two\r\nlines\"\r\n", 200),
			opts:  []Option{WithBufferSize(128)},
		},
		{
			name:  "enclosure first used after plain records",
			input: "id,text\n" + many("%d,plain\n", 150) + many("%d,\"a,b\"\n", 150),
			opts:  []Option{WithBufferSize(128)},
		},
		{
			name:  "another quote character after the enclosure",
			input: "id,text\n" + many("%d,\"a,b\"\n", 150) + many("%d,'c,d'\n", 150),
			opts:  []Option{WithBufferSize(128)},
		},
		{
			name:  "semicolon, commas in later records",
			input: "id;text\n" + many("%d;x\n", 150) + many("%d;a,b,c,d\n", 150),
			opts:  []Option{WithBufferSize(128)},
		},
		{
			name:  "header and records end with a delimiter",
			input: "id,name,\n" + many("%d,foo,\n", 300),
			opts:  []Option{WithBufferSize(64)},
		},
		{
			name:  "header ends with a delimiter, records hold a last value",
			input: "id,name,\n" + many("%d,foo,bar\n", 300),
			opts:  []Option{WithBufferSize(64)},
		},
		{
			name:  "multi-byte delimiter",
			input: "id||name\n" + many("%d||foo\n", 300),
			opts:  []Option{WithBufferSize(64)},
		},
		{
			name:  "newline delimited",
			input: "id,name\n" + many("%d,\"foo\"\n", 300),
			opts:  []Option{WithBufferSize(64), WithNewlineDelimited()},
		},
		{
			name:  "excluded column",
			input: "id,secret,name\n" + many("%d,x,foo\n", 300),
			opts:  []Option{WithBufferSize(64), WithExclude(1)},
		},
		{
			name:  "excluded column the header does not have",
			input: "id,secret,name\n" + many("%d,x,foo\n", 300),
			opts:  []Option{WithBufferSize(64), WithExclude(1, 7)},
		},
		{
			name:  "header on the third line",
			input: "export 2020\nsheet 1\nid,name\n" + many("%d,foo\n", 300),
			opts:  []Option{WithBufferSize(64), WithStart(3)},
		},
		{
			name:    "skip and limit",
			input:   "id,name\n" + many("%d,foo\n", 300),
			opts:    []Option{WithBufferSize(64), WithSkip(5), WithLimit(250)},
			limited: true,
		},
		{
			name:  "records without a value",
			input: "id,name\n" + many("%d,foo\n,\n\n", 200),
			opts:  []Option{WithBufferSize(64)},
		},
		{
			name:  "full batches in the default buffer",
			input: "id,name\n" + many("%d,foo\n", 20000),
		},
		{
			name:  "less than one batch in the default buffer",
			input: "id,name\n" + many("%d,foo\n", 300),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// the stream breaks at each of these offsets in turn, then reads to its end
			var cuts []int
			for i := 1; i <= 7; i++ {
				cuts = append(cuts, len(tt.input)*i/8+i)
			}
			cuts = append(cuts, len(tt.input))

			t.Run("Run", func(t *testing.T) {
				opts := append([]Option{WithWorkers(3)}, tt.opts...)

				var mu sync.Mutex
				want := make(map[uint64][]string)
				whole := NewReader(strings.NewReader(tt.input), opts...)
				wantNames, err := whole.Header()
				if err != nil {
					t.Fatalf("Header(): %s", err)
				}
				if err := whole.Run(func() (Handler, error) { return rowKeeper{&mu, want}, nil }); err != nil {
					t.Fatalf("Run(): %s", err)
				}

				got := make(map[uint64][]string)
				var checkpoint Checkpoint
				past := false
				for _, cut := range cuts {
					if int64(cut) < checkpoint.Offset {
						continue
					}
					r := NewReader(cutAt(tt.input, checkpoint.Offset, cut), append(opts, WithCheckpoint(checkpoint))...)
					names, err := r.Header()
					if err == nil {
						if diff := cmp.Diff(wantNames, names); diff != "" {
							t.Fatalf("Header() from offset %d mismatch (-want +got):\n%s", checkpoint.Offset, diff)
						}
						rows := make(map[uint64][]string)
						err = r.Run(func() (Handler, error) { return rowKeeper{&mu, rows}, nil })
						for row, record := range rows {
							if had, ok := got[row]; ok && !slices.Equal(had, record) {
								t.Fatalf("row %d from offset %d = %q, was %q", row, checkpoint.Offset, record, had)
							}
							got[row] = record
						}
					}
					// a Reader at its limit does not read up to the break
					if tt.limited && err == nil {
						cut = len(tt.input)
					}
					if (cut < len(tt.input)) != errors.Is(err, io.ErrUnexpectedEOF) || (cut == len(tt.input) && err != nil) {
						t.Fatalf("reading up to %d of %d bytes ended in %v", cut, len(tt.input), err)
					}

					next := stored(t, r.Checkpoint())
					if next.Offset < checkpoint.Offset || next.Row < checkpoint.Row {
						t.Fatalf("Checkpoint() = offset %d row %d, before offset %d row %d",
							next.Offset, next.Row, checkpoint.Offset, checkpoint.Row)
					}
					// a caller keeps the rows up to the checkpoint; the rest comes again
					for row := range got {
						if cut < len(tt.input) && row > next.Row {
							delete(got, row)
						}
					}
					checkpoint = next
					if cut == len(tt.input) {
						break
					}
					if checkpoint.Offset > 0 {
						past = true
					}
				}

				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("rows of the runs together mismatch (-want +got):\n%s", diff)
				}
				if !past {
					t.Errorf("no run started past the start of the input")
				}
			})

			t.Run("Read", func(t *testing.T) {
				opts := append([]Option{WithWorkers(3), WithOrder()}, tt.opts...)
				want := readAll(t, NewReader(strings.NewReader(tt.input), opts...))

				var got [][]string
				var checkpoint Checkpoint
				for _, cut := range cuts {
					if int64(cut) < checkpoint.Offset {
						continue
					}
					r := NewReader(cutAt(tt.input, checkpoint.Offset, cut), append(opts, WithCheckpoint(checkpoint))...)

					// a caller keeps what it read before the checkpoint moved
					var kept, read [][]string
					at := checkpoint
					var err error
					for first := true; ; first = false {
						var record []string
						record, err = r.ReadStrings()
						if now := r.Checkpoint(); now.Offset != at.Offset {
							kept, read, at = append(kept, read...), nil, now
						}
						if err != nil {
							break
						}
						// every run returns the header first
						if first {
							if diff := cmp.Diff(want[0], record); diff != "" {
								t.Fatalf("header from offset %d mismatch (-want +got):\n%s", checkpoint.Offset, diff)
							}
							continue
						}
						read = append(read, record)
					}
					r.Close()
					if tt.limited && err == io.EOF {
						cut = len(tt.input)
					}
					if (cut < len(tt.input)) != errors.Is(err, io.ErrUnexpectedEOF) || (cut == len(tt.input) && err != io.EOF) {
						t.Fatalf("reading up to %d of %d bytes ended in %v", cut, len(tt.input), err)
					}
					if cut == len(tt.input) && len(read) != 0 {
						t.Fatalf("%d records read after the last checkpoint of input read to its end", len(read))
					}

					got = append(got, kept...)
					checkpoint = stored(t, at)
					if cut == len(tt.input) {
						break
					}
				}

				if diff := cmp.Diff(want[1:], got); diff != "" {
					t.Errorf("records of the runs together mismatch (-want +got):\n%s", diff)
				}
			})
		})
	}
}

func TestProgress_deliver(t *testing.T) {
	type delivery struct {
		seq uint64
		// offset is the offset of the state past the batch, 0 for a batch no Reader starts after
		offset int64
		whole  bool
	}
	tests := []struct {
		name       string
		deliveries []delivery
		// want is the offset of the state reached, 0 for none
		want int64
	}{
		{
			name:       "a batch ahead waits for the one before it",
			deliveries: []delivery{{1, 20, true}},
			want:       0,
		},
		{
			name:       "batches out of order",
			deliveries: []delivery{{2, 30, true}, {1, 20, true}, {0, 10, true}},
			want:       30,
		},
		{
			name:       "a batch not delivered whole stops the batches after it",
			deliveries: []delivery{{0, 10, true}, {1, 20, false}, {2, 30, true}},
			want:       10,
		},
		{
			name:       "a batch ahead not delivered whole",
			deliveries: []delivery{{2, 30, true}, {1, 20, false}, {0, 10, true}},
			want:       10,
		},
		{
			name:       "a batch no Reader starts after is passed over",
			deliveries: []delivery{{0, 10, true}, {1, 0, true}, {2, 30, true}},
			want:       30,
		},
		{
			name:       "the last batch delivered is one no Reader starts after",
			deliveries: []delivery{{0, 10, true}, {1, 0, true}},
			want:       10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p progress
			// the cutter adds the states in input order, before any batch is delivered
			cut := append([]delivery(nil), tt.deliveries...)
			slices.SortFunc(cut, func(a, b delivery) bool { return a.seq < b.seq })
			for _, d := range cut {
				if d.offset != 0 {
					p.add(d.seq, state{offset: d.offset})
				}
			}
			for _, d := range tt.deliveries {
				p.deliver(d.seq, d.whole)
			}
			if p.at.offset != tt.want || p.reached != (tt.want != 0) {
				t.Errorf("state reached = offset %d (reached %t), want offset %d", p.at.offset, p.reached, tt.want)
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
