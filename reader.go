// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Yorick Terweijden

// Package dsv reads records from delimiter separated input, CSV included, that
// does not follow RFC 4180: any delimiter or enclosure, detected when not
// given, unescaped delimiters and line breaks inside fields, Excel and PHP
// exports.
//
// One goroutine cuts the input into records and workers split the records into
// fields. Run hands the fields to a Handler per worker; Read returns them to
// the caller.
package dsv

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/terwey/dsv-parser/characters"
	"github.com/terwey/dsv-parser/util"
)

const (
	defaultBufferSize = 16 * 1024 * 1024

	// batchRecords is the most records one worker takes at a time.
	batchRecords = 128
)

// ErrClosed is returned by a Reader that was closed before its input ended.
var ErrClosed = errors.New("dsv: reader closed")

// Handler receives the records one worker splits. Run gives every worker a
// Handler of its own and calls it from that worker only.
type Handler interface {
	// Record receives one record. row is its number in the input: the first
	// record after the header is 1, and the number of a skipped record is not
	// reused. flat holds the fields back to back and lengths the length of each
	// field. Both are valid until Record returns. An error stops the Reader.
	Record(row uint64, flat []byte, lengths []uint32) error
	// Close is called once, after the last Record.
	Close() error
}

// Reader reads records from delimiter separated input. Use either Run or Read
// on one Reader. Header, Run and Read are not safe for concurrent use; Close is.
type Reader struct {
	// RowCounter is complete once Run has returned or Read has returned io.EOF.
	RowCounter RowCount

	input     io.Reader
	p         *params
	split     *splitter
	splitFunc bufio.SplitFunc

	workers int
	ordered bool

	// cutter state, touched by the cutter goroutine only
	delimSet        bool
	enclosureSet    bool
	enclDetectCount int
	startCounter    int
	headerRead      bool
	// a header ending with a delimiter waits for the first row
	pendingHeader []byte
	blk           *block
	off, end      int
	eof           bool
	cur           *batch
	row           uint64
	seq           uint64

	blockPool  sync.Pool
	batchPool  sync.Pool
	resultPool sync.Pool

	startOnce  sync.Once
	headerOnce sync.Once
	headerCh   chan *header
	hdr        *header
	batches    chan *batch
	results    chan *result
	quit       chan struct{}
	quitOnce   sync.Once
	cutting    sync.WaitGroup
	working    sync.WaitGroup

	mu  sync.Mutex
	err error

	// optErr is the option NewReader could not accept
	optErr error

	// Read state
	reading bool
	readErr error
	res     *result
	resRec  int
	resFlat uint32
	resLen  int
	pending map[uint64]*result
	nextSeq uint64
	fields  [][]byte
}

// header is what the cutter learned from the header line; workers split with it.
type header struct {
	names          [][]byte
	colCount       int
	exclude        []int
	keepLastColumn bool
}

// block is one buffer of input. Records are slices of it, so it returns to the
// pool only when the cutter and every batch cut from it have released it.
type block struct {
	buf  []byte
	refs int32
}

type record struct {
	data           []byte
	count, outside int32
}

// batch is a run of records cut under one detection state.
type batch struct {
	blk   *block
	recs  []record
	cfg   util.CharacterConfig
	first uint64
	seq   uint64
}

// result is one batch split into fields, for Read.
type result struct {
	seq    uint64
	flat   []byte
	lens   []uint32
	fields []uint32
}

// Option configures a Reader.
type Option func(*Reader)

// WithDelimiter sets the delimiter instead of detecting it. An empty delimiter
// leaves the detection on.
func WithDelimiter(delimiter []byte) Option {
	return func(r *Reader) {
		if len(delimiter) == 0 {
			return
		}
		r.p.Characters.SetDelimiter(append(characters.Character{}, delimiter...))
	}
}

// WithEnclosure sets the enclosure instead of detecting it. An empty enclosure
// reads the input as newline delimited: no field holds a line break.
func WithEnclosure(enclosure []byte) Option {
	return func(r *Reader) {
		r.p.Characters.SetEnclosure(enclosure)
		if len(enclosure) == 0 {
			r.p.Characters.EnclosureIsEmpty = true
			r.p.NewlineDelimited = true
		}
	}
}

// WithNewlineDelimited reads every line as one record: no field holds a line break.
func WithNewlineDelimited() Option {
	return func(r *Reader) {
		r.p.NewlineDelimited = true
	}
}

// WithPHP reads input written by PHP, which escapes an enclosure with a backslash.
func WithPHP() Option {
	return func(r *Reader) {
		r.p.Characters.PHP = true
	}
}

// WithStart sets the 1-based line the header is on; the lines before it are
// skipped. Empty lines are not counted.
func WithStart(line int) Option {
	return func(r *Reader) {
		r.p.Start = line
	}
}

// WithExclude drops the columns at the given 0-based indices from every record.
func WithExclude(columns ...int) Option {
	return func(r *Reader) {
		r.p.Exclude = append([]int(nil), columns...)
	}
}

// WithBufferSize sets the largest record, in bytes, the Reader accepts. The
// default is 16 MiB. The input is read into buffers of this size; with records
// near it a Reader holds up to five buffers per worker.
func WithBufferSize(size int) Option {
	return func(r *Reader) {
		r.p.Buffersize = size
	}
}

// WithWorkers sets the number of workers that split records. The default is
// one per CPU.
func WithWorkers(workers int) Option {
	return func(r *Reader) {
		r.workers = workers
	}
}

// WithOrder makes Read return the records in input order. Without it Read
// returns them in the order the workers finish. It does not change Run: a
// Handler orders by the row number it is given.
func WithOrder() Option {
	return func(r *Reader) {
		r.ordered = true
	}
}

// NewReader returns a Reader that reads from input. It starts no goroutine
// until Header, Run or Read is called.
func NewReader(input io.Reader, opts ...Option) *Reader {
	r := &Reader{
		input:        input,
		p:            newParams(),
		workers:      runtime.GOMAXPROCS(0),
		startCounter: 1,
		headerCh:     make(chan *header, 1),
		quit:         make(chan struct{}),
	}
	r.p.Buffersize = defaultBufferSize

	for _, opt := range opts {
		opt(r)
	}

	if r.workers < 1 {
		r.workers = 1
	}
	if r.p.Buffersize < 1 {
		r.optErr = fmt.Errorf("dsv: buffer size %d is below 1", r.p.Buffersize)
	}
	if r.p.Characters.Delimiter.Length() > 1 && len(r.p.Characters.Enclosure) == 0 {
		r.p.Characters.EnclosureIsEmpty = true
		r.p.NewlineDelimited = true
	}

	if r.p.Characters.Delimiter != nil {
		r.delimSet = true
	}
	if r.p.Characters.Enclosure != nil {
		r.enclosureSet = true
	}
	if r.p.Characters.EnclosureIsEmpty {
		r.enclosureSet = true
	}
	if r.p.Start > 1 {
		r.p.Characters.ReturnEarly = true
	}

	r.split = newSplitter(r.p)
	r.splitFunc = r.p.Characters.SplitCsvInput
	if r.p.NewlineDelimited {
		r.splitFunc = bufio.ScanLines
	}
	r.batches = make(chan *batch, 4*r.workers)

	return r
}

// Header returns the names of the columns: a line break in a name is a space
// and an unnamed column is named empty_column. It returns io.EOF for input
// without a header. The names stay valid. A caller that does not go on to Run,
// or to Read up to io.EOF, calls Close.
func (r *Reader) Header() ([][]byte, error) {
	if r.optErr != nil {
		return nil, r.optErr
	}

	r.startOnce.Do(func() {
		r.cutting.Add(1)
		go r.cut()
	})
	r.headerOnce.Do(func() {
		r.hdr = <-r.headerCh
	})

	if r.hdr == nil {
		if err := r.failure(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	return r.hdr.names, nil
}

// Run splits the records after the header and hands each to a Handler. It
// calls newHandler once per worker, from the calling goroutine, and returns
// when the input is read and every Handler is closed. Records without a value
// in any field are skipped. A record can hold fewer or more fields than the
// header.
func (r *Reader) Run(newHandler func() (Handler, error)) error {
	if _, err := r.Header(); err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}

	for i := 0; i < r.workers; i++ {
		h, err := newHandler()
		if err != nil {
			r.fail(err)
			break
		}
		r.working.Add(1)
		go r.handle(h)
	}

	r.working.Wait()
	r.cutting.Wait()
	return r.failure()
}

// Read returns the next record, one slice per field. The first record is the
// header. Records without a value in any field are skipped. A record can hold
// fewer or more fields than the header.
//
// The record and its fields are valid until the next call to Read. Read returns
// io.EOF after the last record. A caller that stops before io.EOF calls Close.
func (r *Reader) Read() ([][]byte, error) {
	if r.readErr != nil {
		return nil, r.readErr
	}
	select {
	case <-r.quit:
		r.readErr = r.failure()
		return nil, r.readErr
	default:
	}

	if !r.reading {
		r.reading = true
		names, err := r.Header()
		if err != nil {
			r.readErr = err
			return nil, err
		}

		r.results = make(chan *result, 4*r.workers)
		r.pending = make(map[uint64]*result)
		for i := 0; i < r.workers; i++ {
			r.working.Add(1)
			go r.work()
		}
		go func() {
			r.working.Wait()
			close(r.results)
		}()

		return names, nil
	}

	for {
		if r.res != nil {
			if r.resRec < len(r.res.fields) {
				return r.nextFields(), nil
			}
			r.resultPool.Put(r.res)
			r.res = nil
		}

		res, err := r.nextResult()
		if err != nil {
			r.readErr = err
			return nil, err
		}
		r.res, r.resRec, r.resFlat, r.resLen = res, 0, 0, 0
	}
}

// ReadStrings is Read with the fields copied into strings, which stay valid.
func (r *Reader) ReadStrings() ([]string, error) {
	fields, err := r.Read()
	if err != nil {
		return nil, err
	}

	record := make([]string, len(fields))
	for i := range fields {
		record[i] = string(fields[i])
	}
	return record, nil
}

// Close stops the Reader and waits for its goroutines. It waits for a Read of
// the input that is in flight. Records not yet returned are lost: Run, and
// Read unless it already returned io.EOF, return ErrClosed.
func (r *Reader) Close() error {
	r.fail(ErrClosed)
	r.cutting.Wait()
	r.working.Wait()
	return nil
}

func (r *Reader) fail(err error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
	r.quitOnce.Do(func() {
		close(r.quit)
	})
}

func (r *Reader) failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *Reader) addRowCount(c RowCount) {
	r.mu.Lock()
	r.RowCounter.GoParser += c.GoParser
	r.RowCounter.Dropped += c.Dropped
	r.RowCounter.GoParserDropped += c.GoParserDropped
	r.mu.Unlock()
}

func (r *Reader) nextResult() (*result, error) {
	for {
		if r.ordered {
			if res, ok := r.pending[r.nextSeq]; ok {
				delete(r.pending, r.nextSeq)
				r.nextSeq++
				return res, nil
			}
		}

		res, ok := <-r.results
		if !ok {
			r.cutting.Wait()
			if err := r.failure(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		if !r.ordered {
			return res, nil
		}
		r.pending[res.seq] = res
	}
}

func (r *Reader) nextFields() [][]byte {
	res := r.res
	r.fields = r.fields[:0]
	for i := uint32(0); i < res.fields[r.resRec]; i++ {
		n := res.lens[r.resLen]
		r.fields = append(r.fields, res.flat[r.resFlat:r.resFlat+n:r.resFlat+n])
		r.resFlat += n
		r.resLen++
	}
	r.resRec++
	return r.fields
}

// workerSplitter returns the splitter of one worker. Its detection state is
// set from every batch it splits.
func (r *Reader) workerSplitter() *splitter {
	sp := newSplitter(&params{
		Characters: &util.CharacterConfig{},
		Exclude:    r.hdr.exclude,
	})
	sp.colCount = r.hdr.colCount
	sp.keepLastColumn = r.hdr.keepLastColumn
	return sp
}

// handle is one worker of Run.
func (r *Reader) handle(h Handler) {
	defer r.working.Done()

	sp := r.workerSplitter()
	defer func() {
		if err := h.Close(); err != nil {
			r.fail(err)
		}
		r.addRowCount(sp.RowCounter)
	}()

	for {
		select {
		case <-r.quit:
			return
		case b, ok := <-r.batches:
			if !ok {
				return
			}
			*sp.p.Characters = b.cfg
			for i := range b.recs {
				rec := &b.recs[i]
				flat, lens := sp.SplitRow(0, rec.data, int(rec.count), int(rec.outside), false)
				if len(flat) == 0 {
					continue
				}
				if err := h.Record(b.first+uint64(i), flat, lens); err != nil {
					r.fail(err)
					r.putBatch(b)
					return
				}
			}
			r.putBatch(b)
		}
	}
}

// work is one worker of Read. It splits a batch straight into the result the
// caller reads from: the splitter appends at the tail of the result's buffers.
func (r *Reader) work() {
	defer r.working.Done()

	sp := r.workerSplitter()
	defer func() {
		r.addRowCount(sp.RowCounter)
	}()

	for {
		select {
		case <-r.quit:
			return
		case b, ok := <-r.batches:
			if !ok {
				return
			}

			res, _ := r.resultPool.Get().(*result)
			if res == nil {
				res = &result{}
			}
			res.seq = b.seq
			res.flat, res.lens, res.fields = res.flat[:0], res.lens[:0], res.fields[:0]

			size := 0
			for i := range b.recs {
				size += len(b.recs[i].data)
			}
			// the splitter writes eight bytes at a time, up to seven past a field
			if cap(res.flat) < size+8 {
				res.flat = make([]byte, 0, size+8)
			}

			*sp.p.Characters = b.cfg
			for i := range b.recs {
				rec := &b.recs[i]
				n, m := len(res.flat), len(res.lens)
				if cap(res.lens)-m < sp.colCount+64 {
					grown := make([]uint32, m, 2*cap(res.lens)+sp.colCount+64)
					copy(grown, res.lens)
					res.lens = grown
				}
				sp.gbuf, sp.gpos = res.flat[n:n], res.lens[m:m]

				flat, lens := sp.SplitRow(0, rec.data, int(rec.count), int(rec.outside), false)
				if len(flat) == 0 {
					continue
				}

				// the regular splitters wrote in place; the fallbacks return their own slices
				if cap(res.flat) > n && &flat[0] == &res.flat[:n+1][n] {
					res.flat = res.flat[:n+len(flat)]
				} else {
					res.flat = append(res.flat, flat...)
				}
				if len(lens) != 0 && cap(res.lens) > m && &lens[0] == &res.lens[:m+1][m] {
					res.lens = res.lens[:m+len(lens)]
				} else {
					res.lens = append(res.lens, lens...)
				}
				res.fields = append(res.fields, uint32(len(lens)))
			}
			r.putBatch(b)

			select {
			case r.results <- res:
			case <-r.quit:
				return
			}
		}
	}
}

func (r *Reader) getBlock() *block {
	blk, _ := r.blockPool.Get().(*block)
	if blk == nil {
		blk = &block{buf: make([]byte, r.p.Buffersize)}
	}
	atomic.StoreInt32(&blk.refs, 1)
	return blk
}

func (r *Reader) releaseBlock(blk *block) {
	if atomic.AddInt32(&blk.refs, -1) == 0 {
		r.blockPool.Put(blk)
	}
}

func (r *Reader) putBatch(b *batch) {
	r.releaseBlock(b.blk)
	b.blk = nil
	for i := range b.recs {
		b.recs[i] = record{}
	}
	b.recs = b.recs[:0]
	r.batchPool.Put(b)
}

// cut is the cutter goroutine: it reads the input, cuts it into records and
// sends them to the workers in batches.
func (r *Reader) cut() {
	defer r.cutting.Done()
	defer close(r.batches)
	defer close(r.headerCh)
	defer func() {
		r.addRowCount(r.split.RowCounter)
	}()

	r.blk = r.getBlock()
	for {
		token, err := r.next()
		if err == ErrClosed {
			return
		}
		if err != nil {
			if err != io.EOF {
				r.fail(err)
				return
			}
			break
		}
		if !r.record(token) {
			return
		}
	}

	// a file without rows keeps the Excel reading of a header ending with a delimiter
	if r.pendingHeader != nil {
		r.sendHeader(r.split.parseHeader(r.pendingHeader))
		r.pendingHeader = nil
	}
	r.flush()
}

// next returns the next record of the input, a slice of the current block.
func (r *Reader) next() ([]byte, error) {
	stalled := 0
	for {
		if r.end > r.off || r.eof {
			if r.end == r.off {
				return nil, io.EOF
			}
			advance, token, err := r.splitFunc(r.blk.buf[r.off:r.end], r.eof)
			if err != nil {
				return nil, err
			}
			if advance < 0 || advance > r.end-r.off {
				return nil, bufio.ErrAdvanceTooFar
			}
			r.off += advance
			if token != nil {
				return token, nil
			}
			if r.eof {
				return nil, io.EOF
			}
		}

		if r.end == len(r.blk.buf) {
			if r.off == 0 {
				return nil, fmt.Errorf("record exceeds the %d byte buffer, or the delimiter (%s) or enclosure (%s) is wrong: %w",
					r.p.Buffersize, r.p.Characters.Delimiter, r.p.Characters.Enclosure, bufio.ErrTooLong)
			}
			// the records cut so far stay in the old block, the rest of it moves
			if !r.flush() {
				return nil, ErrClosed
			}
			blk := r.getBlock()
			n := copy(blk.buf, r.blk.buf[r.off:r.end])
			r.releaseBlock(r.blk)
			r.blk, r.off, r.end = blk, 0, n
		}

		n, err := r.input.Read(r.blk.buf[r.end:])
		r.end += n
		if err == io.EOF {
			r.eof = true
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			stalled++
			if stalled > 100 {
				return nil, io.ErrNoProgress
			}
			continue
		}
		stalled = 0
	}
}

func (r *Reader) sendHeader(names [][]byte) {
	if names == nil {
		return
	}

	owned := make([][]byte, len(names))
	for i := range names {
		owned[i] = append([]byte(nil), names[i]...)
	}
	r.headerCh <- &header{
		names:          owned,
		colCount:       r.split.colCount,
		exclude:        append([]int(nil), r.p.Exclude...),
		keepLastColumn: r.split.keepLastColumn,
	}
}

// add appends a row to the current batch. A batch ends when it is full or the
// detection state changed, so every row is split under the state it was cut in.
func (r *Reader) add(data []byte, count, outside int) bool {
	if r.cur != nil && (len(r.cur.recs) == batchRecords ||
		r.cur.cfg.Excel != r.p.Characters.Excel ||
		!bytes.Equal(r.cur.cfg.Enclosure, r.p.Characters.Enclosure) ||
		!bytes.Equal(r.cur.cfg.Delimiter, r.p.Characters.Delimiter)) {
		if !r.flush() {
			return false
		}
	}

	r.row++
	if r.cur == nil {
		b, _ := r.batchPool.Get().(*batch)
		if b == nil {
			b = &batch{recs: make([]record, 0, batchRecords)}
		}
		b.cfg = *r.p.Characters
		b.blk = r.blk
		atomic.AddInt32(&r.blk.refs, 1)
		b.first = r.row
		b.seq = r.seq
		r.seq++
		r.cur = b
	}
	r.cur.recs = append(r.cur.recs, record{data: data, count: int32(count), outside: int32(outside)})
	return true
}

// flush sends the current batch to the workers. It returns false when the
// Reader was closed.
func (r *Reader) flush() bool {
	if r.cur == nil {
		return true
	}

	select {
	case r.batches <- r.cur:
		r.cur = nil
		return true
	case <-r.quit:
		return false
	}
}
