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
	"encoding/json"
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

	// firstBlockSize is the size of the blocks the input is read into until a
	// record does not fit in one.
	firstBlockSize = 1024 * 1024

	// A worker takes a batch at a time: up to batchRecords records, or fewer
	// that hold batchBytes together. A small batch costs more to hand over
	// than to split.
	batchRecords = 4096
	batchBytes   = 256 * 1024

	// blockTail is the free space at the end of a block the cutter does not read into.
	blockTail = 4096
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
	skip    int
	limit   int

	// cutter state, touched by the cutter goroutine only
	delimSet        bool
	enclosureSet    bool
	enclDetectCount int
	startCounter    int
	headerRead      bool
	skipped         int
	// limited is set when the cutter stopped at the limit
	limited bool
	// a header ending with a delimiter waits for the first row
	pendingHeader []byte
	// resumed is the header of the checkpoint the Reader started from
	resumed  *header
	blk      *block
	off, end int
	// blockSize is the size of the blocks the cutter takes: it doubles, up to
	// the buffer size, when one record fills a block
	blockSize int
	eof       bool
	cur       *batch
	row       uint64
	seq       uint64
	// base is the input offset of the first byte of blk
	base int64

	// checkpoints is set when the Reader keeps checkpoints: without it the
	// cutter takes no state and the workers report nothing to progress
	checkpoints bool
	progress    progress
	// resume is the checkpoint the Reader started from
	resume Checkpoint

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
	blk  *block
	recs []record
	// size is the length of the records together
	size  int
	cfg   util.CharacterConfig
	first uint64
	seq   uint64
}

// state is what the cutter carries from one record to the next: a Reader given
// it reads the input from offset on as the Reader it was taken from did.
type state struct {
	cfg             util.CharacterConfig
	delimSet        bool
	enclosureSet    bool
	enclDetectCount int
	// offset is the input offset of the next record, row the number of the last one
	offset int64
	row    uint64
}

// result is one batch split into fields, for Read.
type result struct {
	seq    uint64
	flat   []byte
	lens   []uint32
	fields []uint32
}

// progress is the state after the last batch delivered with every batch before it.
type progress struct {
	mu sync.Mutex
	// at is that state, set when reached is
	at      state
	reached bool
	// next is the sequence number of the first batch not delivered
	next uint64
	// cut holds, from head on and in input order, the state past every batch
	// from next on that a Reader can start after
	cut  []cutState
	head int
	// ahead holds the batches after next that were delivered, and whether whole
	ahead map[uint64]bool
	// stopped is set once a batch was not delivered whole
	stopped bool
}

// cutState is the state of the cutter past batch seq.
type cutState struct {
	seq   uint64
	after state
}

// add keeps the state past batch seq. The cutter calls it before it sends the
// batch, in input order, for a batch a Reader can start after.
func (p *progress) add(seq uint64, after state) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return
	}
	p.cut = append(p.cut, cutState{seq: seq, after: after})
}

// deliver records that the records of batch seq were delivered: all of them
// when whole is true.
func (p *progress) deliver(seq uint64, whole bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return
	}
	if seq != p.next {
		if p.ahead == nil {
			p.ahead = make(map[uint64]bool)
		}
		p.ahead[seq] = whole
		return
	}

	for {
		if !whole {
			p.stopped = true
			return
		}
		if p.head < len(p.cut) && p.cut[p.head].seq == p.next {
			p.at, p.reached = p.cut[p.head].after, true
			p.head++
			// the states passed make room once they are half of the slice
			if p.head == len(p.cut) {
				p.cut, p.head = p.cut[:0], 0
			} else if p.head >= 16 && 2*p.head >= len(p.cut) {
				p.cut, p.head = p.cut[:copy(p.cut, p.cut[p.head:])], 0
			}
		}
		p.next++
		var ok bool
		if whole, ok = p.ahead[p.next]; !ok {
			return
		}
		delete(p.ahead, p.next)
	}
}

// Checkpoint is a place in the input a Reader can start from again. The zero
// value is the start of the input. It can be stored as JSON.
type Checkpoint struct {
	// Offset is the number of bytes of input before the checkpoint. The input of
	// a Reader given the checkpoint starts with the byte after them.
	Offset int64
	// Row is the number of the last record before the checkpoint.
	Row uint64

	// state is the header and the detection state at Offset, encoded
	state []byte
}

// checkpointState is the encoded part of a Checkpoint.
type checkpointState struct {
	Names           [][]byte
	ColCount        int
	Exclude         []int
	KeepLastColumn  bool
	DelimSet        bool
	EnclosureSet    bool
	EnclDetectCount int
	Characters      []byte
}

// MarshalJSON encodes c with its header and detection state.
func (c Checkpoint) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Offset int64  `json:"offset"`
		Row    uint64 `json:"row"`
		State  []byte `json:"state,omitempty"`
	}{c.Offset, c.Row, c.state})
}

// UnmarshalJSON decodes what MarshalJSON encoded.
func (c *Checkpoint) UnmarshalJSON(data []byte) error {
	var v struct {
		Offset int64  `json:"offset"`
		Row    uint64 `json:"row"`
		State  []byte `json:"state"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	c.Offset, c.Row, c.state = v.Offset, v.Row, v.State
	return nil
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

// WithSkip skips the first records after the header. A skipped record gets no
// row number.
func WithSkip(records int) Option {
	return func(r *Reader) {
		r.skip = records
	}
}

// WithLimit stops the Reader after the given number of records, the header
// and the skipped records not counted. The rest of the input is not read.
func WithLimit(records int) Option {
	return func(r *Reader) {
		if records < 0 {
			return
		}
		r.limit = records
	}
}

// WithLineEnding sets the line ending the Reader starts with instead of
// detecting it: LF, CRLF or CR. The Reader still follows a file that changes
// its line ending, unless WithFixedLineEnding is set.
func WithLineEnding(eol []byte) Option {
	return func(r *Reader) {
		if len(eol) == 0 {
			return
		}
		r.p.Characters.EOL = append(characters.Character{}, eol...)
	}
}

// WithFixedLineEnding keeps the line ending the Reader detected or was given:
// a file that changes its line ending is read with the first one.
func WithFixedLineEnding() Option {
	return func(r *Reader) {
		r.p.Characters.FixedEOL = true
	}
}

// WithCheckpoints makes the Reader keep checkpoints, for Checkpoint to return.
// Without it a Reader keeps none and pays nothing for them.
func WithCheckpoints() Option {
	return func(r *Reader) {
		r.checkpoints = true
	}
}

// WithCheckpoint starts the Reader at a Checkpoint another Reader returned, and
// makes it keep checkpoints as WithCheckpoints does. Give it the options that
// Reader had. The input starts at the checkpoint's Offset and holds no header:
// Header, and the first Read, return the names the checkpoint carries. Rows are
// numbered on from the checkpoint's Row. The delimiter, enclosure, line ending
// and excluded columns are those of the checkpoint; nothing is detected again.
// WithStart and WithSkip do not apply to the input given, WithLimit counts the
// rows before the checkpoint too. The zero Checkpoint is the start of the
// input: the Reader reads it as one without a checkpoint does.
func WithCheckpoint(c Checkpoint) Option {
	return func(r *Reader) {
		r.resume = c
		r.checkpoints = true
	}
}

// WithExclude drops the columns at the given 0-based indices from every record.
func WithExclude(columns ...int) Option {
	return func(r *Reader) {
		r.p.Exclude = append([]int(nil), columns...)
	}
}

// WithBufferSize sets the largest record, in bytes, the Reader accepts. The
// default is 16 MiB. The input is read into blocks of 1 MiB, or of this size
// when it is smaller; a record that does not fit in a block doubles the size
// of the blocks from there on. With records near the size of the blocks a
// Reader holds up to five blocks per worker.
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
		limit:        -1,
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
	r.p.Characters.BufferSize = r.p.Buffersize
	r.blockSize = firstBlockSize
	if r.blockSize > r.p.Buffersize {
		r.blockSize = r.p.Buffersize
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
	if r.resume.state != nil {
		if err := r.restore(); err != nil {
			r.optErr = err
		}
	}
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

// restore sets the cutter to the state of the checkpoint the Reader was given.
func (r *Reader) restore() error {
	var st checkpointState
	if err := json.Unmarshal(r.resume.state, &st); err != nil {
		return fmt.Errorf("dsv: checkpoint: %w", err)
	}
	if err := r.p.Characters.UnmarshalBinary(st.Characters); err != nil {
		return fmt.Errorf("dsv: checkpoint: %w", err)
	}
	r.p.Characters.BufferSize = r.p.Buffersize

	r.delimSet, r.enclosureSet, r.enclDetectCount = st.DelimSet, st.EnclosureSet, st.EnclDetectCount
	r.headerRead = true
	r.startCounter = r.p.Start
	r.skipped = r.skip
	r.row = r.resume.Row
	r.base = r.resume.Offset
	r.resumed = &header{
		names:          st.Names,
		colCount:       st.ColCount,
		exclude:        st.Exclude,
		keepLastColumn: st.KeepLastColumn,
	}
	return nil
}

// Checkpoint returns the last place a Reader can start from again with
// WithCheckpoint: the end of a record that was delivered with every record
// before it. The Reader keeps checkpoints only when it was given
// WithCheckpoints or WithCheckpoint; without them Checkpoint returns the zero
// Checkpoint. A record is delivered when Read returned it and Read was called
// again, or under Run when Record returned nil for it. Checkpoints lie up to
// 4096 records or 256 KiB of input apart, so records after the checkpoint can
// have been delivered as well: a Reader started from it delivers them again,
// under the same row numbers.
//
// Before the first checkpoint is reached it returns the Checkpoint the Reader
// was given, the zero Checkpoint without one. After an error, an error of the
// input included, it still returns the last checkpoint reached. Checkpoint is
// safe to call during Run, not concurrently with Read.
func (r *Reader) Checkpoint() Checkpoint {
	r.progress.mu.Lock()
	defer r.progress.mu.Unlock()

	if !r.progress.reached {
		return r.resume
	}
	at := &r.progress.at
	cfg, err := at.cfg.MarshalBinary()
	if err != nil {
		return r.resume
	}
	encoded, err := json.Marshal(checkpointState{
		Names:           r.hdr.names,
		ColCount:        r.hdr.colCount,
		Exclude:         r.hdr.exclude,
		KeepLastColumn:  r.hdr.keepLastColumn,
		DelimSet:        at.delimSet,
		EnclosureSet:    at.enclosureSet,
		EnclDetectCount: at.enclDetectCount,
		Characters:      cfg,
	})
	if err != nil {
		return r.resume
	}
	return Checkpoint{Offset: at.offset, Row: at.row, state: encoded}
}

// Characters returns the delimiter, enclosure and line ending the Reader was
// given or detected, and the modes it switched on. Detection goes on while the
// input is read: the result is complete once Run has returned or Read has
// returned io.EOF, and is not to be called before that.
func (r *Reader) Characters() util.CharacterConfig {
	return *r.p.Characters
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
			if r.checkpoints {
				r.progress.deliver(r.res.seq, true)
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

// fail stops the Reader with err: records not yet delivered are lost.
func (r *Reader) fail(err error) {
	r.setErr(err)
	r.quitOnce.Do(func() {
		close(r.quit)
	})
}

// setErr keeps err as the error Run and Read return, unless one is kept already.
func (r *Reader) setErr(err error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
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
					if r.checkpoints {
						r.progress.deliver(b.seq, false)
					}
					r.putBatch(b)
					return
				}
			}
			if r.checkpoints {
				r.progress.deliver(b.seq, true)
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
	// a block from before the blocks grew is dropped
	if blk == nil || len(blk.buf) < r.blockSize {
		blk = &block{buf: make([]byte, r.blockSize)}
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
	b.size = 0
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

	if r.resumed != nil {
		r.headerCh <- r.resumed
	}

	r.blk = r.getBlock()
	for {
		token, err := r.next()
		if err == ErrClosed {
			return
		}
		if err != nil {
			if err != io.EOF {
				// the records cut before the error are whole: the workers get them
				// before Run or Read returns the error
				r.setErr(err)
				r.flush(true)
				return
			}
			break
		}
		if !r.record(token) {
			return
		}
		if r.limited {
			break
		}
	}

	// a file without rows keeps the Excel reading of a header ending with a delimiter
	if r.pendingHeader != nil {
		r.sendHeader(r.split.parseHeader(r.pendingHeader))
		r.pendingHeader = nil
	}
	r.flush(true)
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

		free := len(r.blk.buf) - r.end
		if free == 0 && r.off == 0 {
			if len(r.blk.buf) >= r.p.Buffersize {
				return nil, fmt.Errorf("record exceeds the %d byte buffer, or the delimiter (%s) or enclosure (%s) is wrong: %w",
					r.p.Buffersize, r.p.Characters.Delimiter, r.p.Characters.Enclosure, bufio.ErrTooLong)
			}
			// one record fills the block: it moves to a block of twice the size
			r.blockSize = 2 * len(r.blk.buf)
			if r.blockSize > r.p.Buffersize {
				r.blockSize = r.p.Buffersize
			}
		}
		// a Read into the last bytes of a block gets little, and from some inputs nothing
		if free == 0 || (r.off > 0 && free < blockTail && free < len(r.blk.buf)/2) {
			// the records cut so far stay in the old block, the rest of it moves
			if !r.flush(true) {
				return nil, ErrClosed
			}
			blk := r.getBlock()
			n := copy(blk.buf, r.blk.buf[r.off:r.end])
			r.releaseBlock(r.blk)
			r.base += int64(r.off)
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
	// the state now is the one past data, not past the batch: no Reader starts here
	if r.cur != nil && (r.cur.cfg.Excel != r.p.Characters.Excel ||
		!bytes.Equal(r.cur.cfg.Enclosure, r.p.Characters.Enclosure) ||
		!bytes.Equal(r.cur.cfg.Delimiter, r.p.Characters.Delimiter)) {
		if !r.flush(false) {
			return false
		}
	}

	r.row++
	if r.cur == nil {
		b, _ := r.batchPool.Get().(*batch)
		if b == nil {
			b = &batch{recs: make([]record, 0, 256)}
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
	r.cur.size += len(data)
	if len(r.cur.recs) == batchRecords || r.cur.size >= batchBytes {
		return r.flush(true)
	}
	return true
}

// flush sends the current batch to the workers. It returns false when the
// Reader was closed. resumable is true when the cutter has not looked past the
// last record of the batch for anything but that record's own cut: its state
// is then the one a Reader starts with after the batch, kept when the Reader
// keeps checkpoints.
func (r *Reader) flush(resumable bool) bool {
	if r.cur == nil {
		return true
	}
	if resumable && r.checkpoints {
		r.progress.add(r.cur.seq, state{
			cfg:             *r.p.Characters,
			delimSet:        r.delimSet,
			enclosureSet:    r.enclosureSet,
			enclDetectCount: r.enclDetectCount,
			offset:          r.base + int64(r.off),
			row:             r.row,
		})
	}

	select {
	case r.batches <- r.cur:
		r.cur = nil
		return true
	case <-r.quit:
		return false
	}
}
