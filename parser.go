// SPDX-License-Identifier: MIT
// Copyright (c) 2020 Yorick Terweijden

package dsv

import (
	"bytes"
	"encoding/csv"
	"io"
	"strconv"
	"strings"

	"golang.org/x/exp/slices"

	"github.com/terwey/dsv-parser/characters"
	"github.com/terwey/dsv-parser/internal/debug"
	"github.com/terwey/dsv-parser/util"
)

// RowCount counts the records a Reader could not split the regular way.
type RowCount struct {
	// GoParser counts the records split by encoding/csv.
	GoParser uint64
	// Dropped counts the records no splitter could split; no Handler and no
	// Read receives them.
	Dropped uint64
	// GoParserDropped counts the records encoding/csv returned an error for
	// and no splitter could split afterwards.
	GoParserDropped uint64
}

type params struct {
	Characters *util.CharacterConfig

	Buffersize int

	// starting position of the CSV file
	// this is needed for Excel to CSV exports
	// that contain data above the header
	Start int

	Exclude []int

	// When set to true the CSV does not have newlines
	// (CR, LF or CRLF) inside records
	NewlineDelimited bool
}

func newParams() *params {
	p := &params{
		Buffersize: 4096,
	}

	p.Characters = util.NewCharacterConfig(characters.Character_NIL, characters.Character_NIL)

	return p
}

type buffers struct {
	gpos     []uint32
	sinput   [][]byte
	sbuf     []byte
	gbuf     []byte
	rbuf     []byte
	lbBuffer bytes.Buffer
}

// splitter splits one record into fields. Its results alias its buffers and
// are valid until the next split.
type splitter struct {
	p *params

	RowCounter RowCount

	colCount int

	*buffers

	inside             bool
	hasPrefixEnclosure bool
	hasSuffixEnclosure bool

	// keepLastColumn keeps an unnamed last header column, the rows have values in it
	keepLastColumn bool
}

func newSplitter(p *params) *splitter {
	return &splitter{
		p:       p,
		buffers: newBuffers(),
	}
}

func newBuffers() *buffers {
	buf := &buffers{
		gbuf: make([]byte, 0, 1*1024*1024),
		gpos: make([]uint32, 0, 1024),
		rbuf: make([]byte, 1*1024*1024),
	}

	return buf
}

// parseHeader splits the header line and names its unnamed columns: a line
// break in a name is a space. It sets the column count the rows are split
// against. The names alias the splitter's buffers.
func (r *splitter) parseHeader(data []byte) [][]byte {
	// replace CRLN/LN with space in header
	data = bytes.ReplaceAll(data, characters.Character_CARRIAGE_RETURN_LINE_FEED, []byte(" "))
	data = bytes.ReplaceAll(data, characters.Character_LINE_FEED, []byte(" "))
	cols, pos := r.SplitRow(0, data, 0, 0, false)
	if len(cols) == 0 {
		return nil
	}
	header := util.FlatToMulti(cols, pos)

	// we gotta check if the last column in the header is set, if not drop it
	if len(header[len(header)-1]) == 0 && !r.keepLastColumn {
		header = header[:len(header)-1]
		r.p.Characters.Excel = true
	}

	emptyCount := 1
	for i := range header {
		if len(header[i]) == 0 {
			if emptyCount > 1 {
				header[i] = []byte("empty_column__")
				header[i] = strconv.AppendInt(header[i], int64(emptyCount), 10)
			} else {
				header[i] = []byte("empty_column")
			}
			emptyCount++
		}
	}

	r.colCount = len(header)
	return header
}

// excludeExisting keeps the excluded columns the header has, each once and in order;
// SplitRow expects every index in Exclude to remove exactly one column.
func (r *splitter) excludeExisting(header []byte) {
	if len(r.p.Exclude) == 0 {
		return
	}

	delimiters := util.CountOutsideEnclosure(header, r.p.Characters.Delimiter, r.p.Characters.Enclosure)
	// unbalanced enclosures, count every delimiter instead
	if delimiters <= 0 {
		delimiters = util.Count(header, r.p.Characters.Delimiter)
	}
	columns := delimiters + 1

	slices.Sort(r.p.Exclude)
	r.p.Exclude = slices.Compact(r.p.Exclude)

	exclude := r.p.Exclude[:0]
	for _, i := range r.p.Exclude {
		if i < 0 || i >= columns {
			continue
		}
		exclude = append(exclude, i)
	}
	r.p.Exclude = exclude
}

func (r *splitter) genSplit(s, sep []byte, n int, fn util.CleanFunc, exclude []int) (data []byte, pos []uint32) {
	r.gbuf = r.gbuf[:0]
	r.gpos = r.gpos[:0]
	return util.GenSplitCore(s, sep, n, fn, exclude, r.gbuf, r.gpos)
}

func (r *splitter) colSplitFlat(s []byte, sep characters.Character, enclosure characters.Character, n int, fn util.CleanFunc) (data []byte, pos []uint32) {
	r.gbuf = r.gbuf[:0]
	r.gpos = r.gpos[:0]
	return util.ColSplitFlatCore(s, sep, enclosure, n, fn, r.gbuf, r.gpos)
}

func (r *splitter) Clean(data []byte, prefix, suffix bool) []byte {
	if r.p.Characters.Config.EnclosureClean && bytes.Contains(data, r.p.Characters.Config.Enclosure) {
		// without prefix or suffix set, only a value wrapped on both ends is enclosed;
		// a lone quote at either end, such as the inch mark in 55", is data
		enclosed := len(data) >= 2*len(r.p.Characters.Config.Enclosure) &&
			bytes.HasPrefix(data, r.p.Characters.Config.Enclosure) &&
			bytes.HasSuffix(data, r.p.Characters.Config.Enclosure)
		if prefix {
			data = util.TrimPrefixSkipCheck(data, r.p.Characters.Config.Enclosure)
		} else if enclosed {
			data = bytes.TrimPrefix(data, r.p.Characters.Config.Enclosure)
		}
		if suffix {
			data = util.TrimSuffixSkipCheck(data, r.p.Characters.Config.Enclosure)
		} else if enclosed {
			data = bytes.TrimSuffix(data, r.p.Characters.Config.Enclosure)
		}

		// we need to clean double quotes
		if bytes.Contains(data, r.p.Characters.Config.DoubleEnclosure) {
			data = util.Replace(data, r.p.Characters.Config.DoubleEnclosure, r.p.Characters.Config.Enclosure, r.rbuf, -1)
		}
	}

	if bytes.IndexByte(data, characters.Character_CARRIAGE_RETURN[0]) != -1 {
		data = util.Replace(data, characters.Character_CARRIAGE_RETURN_LINE_FEED, characters.Character_LINE_FEED, r.rbuf, -1)

		// check again because it might have been cleaned out
		data = util.Replace(data, characters.Character_CARRIAGE_RETURN, characters.Character_LINE_FEED, r.rbuf, -1)
	}

	return data
}

// SplitRow splits data into a flat record and the length of each field. count
// and outsideCount are the delimiters, and the delimiters outside an enclosure,
// the scanner counted in data; zero for both means not counted. colCount zero
// means the columns of the header.
func (r *splitter) SplitRow(colCount int, data []byte, count, outsideCount int, self bool) (out []byte, pos []uint32) {
	if !self {
		if out, pos, ok := r.splitWholeRow(colCount, data, count, outsideCount); ok {
			return out, pos
		}
	}
	return r.splitRow(colCount, data, count, outsideCount, self)
}

// splitRow is SplitRow for every record: it tries one way to split after the other.
func (r *splitter) splitRow(colCount int, data []byte, count, outsideCount int, self bool) (out []byte, pos []uint32) {
	// we need to check first if inside the data the delimiter occurs
	// we can count the amount of occurences of the delimiter and compare
	// if that matches the amount we expect
	// amount = columns + 1

	if debug.DebugLog {
		debug.LogData("[SplitRow] data", data, 512)
		debug.Debug("[SplitRow] encl: %s delim: %s", r.p.Characters.Enclosure, r.p.Characters.Delimiter)
		debug.BreakMe()
	}

	if colCount == 0 {
		colCount = r.colCount
	}

	lowestCount := colCount

	provided := false

	// the incoming count is a delimiterCount and _not_ a columnCount
	if count != 0 || outsideCount != 0 {
		provided = true
	}

	if !provided {
		count = util.Count(data, r.p.Characters.Delimiter)
	}

	if r.p.Characters.Excel {
		count--
	}

	plain := colCount == count+1-len(r.p.Exclude)
	// a short row can reach the expected count with delimiters inside an enclosure
	if plain && len(r.p.Characters.Enclosure) != 0 && delimiterInsideEnclosure(data, r.p.Characters.Delimiter, r.p.Characters.Enclosure) {
		if outside := util.CountOutsideEnclosure(data, r.p.Characters.Delimiter, r.p.Characters.Enclosure); outside > 0 && outside < count {
			plain = false
			lowestCount = outside
		}
	}

	if plain || (colCount == 0 && len(r.p.Characters.Enclosure) == 0 && len(r.p.Characters.Delimiter) > 1) {
		out, pos = r.genSplit(data, r.p.Characters.Delimiter, -1, r.Clean, r.p.Exclude)
		if debug.DebugLog {
			debug.Debug("[SplitRow] GenSplit found %d columns (r.colCount: %d, colCount: %d)", len(pos), r.colCount, colCount)
		}
		return
	}

	if debug.DebugLog {
		debug.BreakMe()
	}

	// no point counting outside enclosure when it doesn't exist
	if len(r.p.Characters.Enclosure) != 0 {
		if !provided {
			outsideCount = util.CountOutsideEnclosure(data, r.p.Characters.Delimiter, r.p.Characters.Enclosure)
			if outsideCount != 0 && outsideCount < lowestCount {
				lowestCount = outsideCount
			}
		}

		if r.p.Characters.Excel {
			outsideCount--
		}

		// CountOutsideEnclosure returns the amount of delimiters, not columns so add 1
		if colCount == outsideCount+1-len(r.p.Exclude) {
			// we should pass the result from `util.Count` to this func for a speedup
			n := -1
			if count > 0 {
				n = count
			}
			out, pos = r.colSplitFlat(data, r.p.Characters.Delimiter, r.p.Characters.Enclosure, n, r.Clean)
			if debug.DebugLog {
				debug.Debug("[SplitRow] ColSplitFlat found %d columns (r.colCount: %d, colCount: %d)", len(pos), r.colCount, colCount)
			}
			if len(pos) == colCount {
				return out, pos
			}
			if len(pos) != 0 && len(pos) < lowestCount {
				lowestCount = len(pos)
			}
		}
	} else {
		if count != 0 && count < lowestCount {
			lowestCount = count
		}
	}

	// r.colCount is set to non-zero on header creation
	// TODO: fix that the header gets parsed by our code too
	if r.colCount != 0 {
		// count := Count(data, r.p.Delimiter)
		// if the count is correct we can just simply do a normal split and skip this logic
		if count > colCount {
			out, pos = r.ParseUnescapedDelimiter(util.ColSplit(data, r.p.Characters.Delimiter, r.p.Characters.Enclosure, -1))
			if debug.DebugLog {
				debug.Debug("[SplitRow] ParseUnescapedDelimiter found %d columns (r.colCount: %d, colCount: %d)", len(pos), r.colCount, colCount)
			}
			if len(pos) == colCount {
				return out, pos
			}
			if len(pos) != 0 && len(pos) < lowestCount {
				lowestCount = len(pos)
			}
		}
	}

	out = make([]byte, 0)
	pos = make([]uint32, 0)

	// the SplitUnescapedDelimiter didn't work properly, use slow method
	// NOTE: it does not support other enclosures that QUOTATION_MARK
	if count > colCount {
		// for simplicity sake we use the Go's build in CSV parser when the
		// delimiter occurs within the quotation
		// the reason for not always using it, is simply speed.
		csvReader := csv.NewReader(bytes.NewReader(data))
		csvReader.Comma = r.p.Characters.Delimiter.Rune()

		for {
			record, err := csvReader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				// last hail mary for broken CSVs, we use a gensplit as we do not care anymore about consistency for these
				o, p := util.GenSplit(data, r.p.Characters.Delimiter, -1, r.Clean, r.p.Exclude)
				if len(o) != 0 {
					if debug.EnableLogDropped {
						debug.LogDropped("GoParser could have dropped (delimCounted: %d): \n%s\nfound: \n%s", bytes.Count(data, r.p.Characters.Delimiter), data, util.FlatToMulti(o, p))
					}
					return o, p
				}
				if debug.EnableLogDropped {
					debug.LogDropped("GoParser dropped (delimCounted: %d): \n%s", bytes.Count(data, r.p.Characters.Delimiter), data)
				}
				r.RowCounter.GoParserDropped++
				return nil, nil
			}

			for i, col := range record {
				if r.p.Exclude != nil && slices.Contains(r.p.Exclude, i) {
					continue
				}

				// we have to clean the enclosure if it's not a double quote
				if !bytes.Equal(r.p.Characters.Enclosure, characters.Character_QUOTATION_MARK) {
					buf := r.Clean([]byte(col), false, false)
					col = string(buf)
				} else {
					// we do not have to call a cleanFunc here as the CSV parser of Go already did so
					// we do have to clean the \r\n
					col = strings.ReplaceAll(col, characters.Character_CARRIAGE_RETURN_LINE_FEED.String(), characters.Character_LINE_FEED.String())
					col = strings.ReplaceAll(col, characters.Character_CARRIAGE_RETURN.String(), characters.Character_LINE_FEED.String())
				}
				out = append(out, col...)
				pos = append(pos, uint32(len(col)))
			}
			r.RowCounter.GoParser++
		}

		if r.colCount != 0 {
			if debug.DebugLog {
				debug.LogReader(data, count, colCount)
			}
		}

		if len(pos) != 0 && len(pos) < lowestCount {
			lowestCount = len(pos)
		}
		if len(pos) == colCount {
			return out, pos
		}

		// first pass for the header
		if r.colCount == 0 {
			return out, pos
		}
	}

	if !self {
		if debug.DebugLog {
			debug.Debug("looping (sep: %s delim: %s) (r.colCount: %d lowestCount: %d): \n%s\n", r.p.Characters.Enclosure,
				r.p.Characters.Delimiter, r.colCount, lowestCount, data)
		}
		return r.SplitRow(lowestCount+1, data, 0, 0, true)
	}

	if debug.DebugLog {
		if len(out) == 0 {
			debug.Debug("dropping (sep: %s delim: %s) (r.colCount: %d lowestCount: %d): \n%s\n", r.p.Characters.Enclosure,
				r.p.Characters.Delimiter, r.colCount, lowestCount, data)
		} else {
			debug.Debug("returning: \n%v\n%v\n", out, pos)
		}
	}

	if len(out) == 0 {
		// last hail mary for broken CSVs
		o, p := util.GenSplit(data, r.p.Characters.Delimiter, -1, r.Clean, r.p.Exclude)
		if len(o) != 0 {
			if debug.EnableLogDropped {
				debug.LogDropped("SplitRow could have dropped (delimCounted: %d): \n%s\nfound: \n%s", bytes.Count(data, r.p.Characters.Delimiter), data, util.FlatToMulti(o, p))
			}
			return o, p
		}
		if debug.EnableLogDropped {
			debug.LogDropped("SplitRow dropped (delimCounted: %d): \n%s", bytes.Count(data, r.p.Characters.Delimiter), data)
		}
		r.RowCounter.Dropped++
	}

	return out, pos
}

// delimiterInsideEnclosure reports whether a delimiter sits between an enclosure and the next one.
func delimiterInsideEnclosure(data []byte, delimiter, enclosure characters.Character) bool {
	if len(delimiter) == 1 && len(enclosure) == 1 {
		inside := false
		for _, c := range data {
			if c == enclosure[0] {
				inside = !inside
			} else if inside && c == delimiter[0] {
				return true
			}
		}
		return false
	}

	for {
		open := bytes.Index(data, enclosure)
		if open == -1 {
			return false
		}
		data = data[open+len(enclosure):]

		end := bytes.Index(data, enclosure)
		if end == -1 {
			return false
		}
		if bytes.Contains(data[:end], delimiter) {
			return true
		}
		data = data[end+len(enclosure):]
	}
}

func (r *splitter) ParseUnescapedDelimiter(input [][]byte) (data []byte, pos []uint32) {
	cleanFunc := r.p.Characters.Clean
	// NOTE: this method  doesn't work on optional enclosures.

	// we need to seek through the data and find the first Enclosure and escape the delimiter

	// we first split on the delimiter and then check if two Enclosures are available
	r.sbuf = make([]byte, 0)
	enclosurecheck := false

	// debug.DebugColumns(input)

	for i := range input {
		// debug.Debug("input[i]: >>>%s<<<", input[i])
		// debug.Debug("buf: %s", r.sbuf)
		// debug.DebugColumns(util.FlatToMulti(data, pos))

		// if debug.DebugLog {
		// 	debug.BreakMe()
		// }

		count := 0
		counted := false

		if !r.inside {
			count = util.Count(input[i], r.p.Characters.Enclosure)
			counted = true
			if count != 0 {
				r.inside = true
			}
		}

		// debug.Debug("inside: %v, enclosurecheck: %v", r.inside, enclosurecheck)

		// there is a case where the last buf can end with """ or similar
		// we need to check if the NEXT starts with a " if so, move buf to columns and continue happily with life
		if enclosurecheck {
			if len(input[i]) == 1 {
				// data was something like `|"foo|"|`
				r.sbuf = append(r.sbuf, r.p.Characters.Delimiter...)
				// add the "empty" data, anti-slipmat func ;)
				// check if input is r.p.Enclosure
				if !bytes.Equal(input[i], r.p.Characters.Enclosure) {
					r.sbuf = append(r.sbuf, input[i]...)
				}
			}
			if bytes.HasPrefix(input[i], r.p.Characters.Enclosure) {
				r.sbuf = cleanFunc(r.sbuf, false, false)
				if r.p.Exclude != nil && slices.Contains(r.p.Exclude, i) {
				} else {
					data = append(data, r.sbuf...)
					pos = append(pos, uint32(len(r.sbuf)))
				}
				r.sbuf = make([]byte, 0)
				r.inside = false
				enclosurecheck = false
			}
			if len(input[i]) == 1 {
				continue
			}
		}

		// unEnclosured column
		if len(input[i]) == 0 {
			// delimiter inside an enclosure
			if r.inside {
				r.sbuf = append(r.sbuf, r.p.Characters.Delimiter...)
				continue
			}
			if r.p.Exclude != nil && slices.Contains(r.p.Exclude, i) {
				continue
			} else {
				input[i] = util.ReplaceAll(input[i], characters.Character_CARRIAGE_RETURN_LINE_FEED, characters.Character_LINE_FEED)
				input[i] = util.ReplaceAll(input[i], characters.Character_CARRIAGE_RETURN, characters.Character_LINE_FEED)
				data = append(data, input[i]...)
				pos = append(pos, uint32(len(input[i])))
			}
			continue
		}

		if len(input[i]) == 1 {
			if len(r.sbuf) != 0 && bytes.Equal(input[i], r.p.Characters.Enclosure) {
				if r.p.Exclude != nil && slices.Contains(r.p.Exclude, i) {
				} else {
					// the delimiter appears at the end, finalize the column
					r.sbuf = append(r.sbuf, r.p.Characters.Delimiter...)

					// TODO: it's unsure if here the suffix should be removed too
					// the tests all work so should be fine, however originally
					// here there was no trimsuffix (which is part of cleanFunc)
					r.sbuf = cleanFunc(r.sbuf, false, false)

					data = append(data, r.sbuf...)
					pos = append(pos, uint32(len(r.sbuf)))
				}
				r.sbuf = make([]byte, 0)
				continue
			}

			// in the case it is NOT an Enclosure, place a delimiter and append the buffer
			if !bytes.Equal(input[i], r.p.Characters.Enclosure) {
				r.sbuf = append(r.sbuf, r.p.Characters.Delimiter...)
			}
			r.sbuf = append(r.sbuf, input[i]...)

			continue
		}

		// if we counted the enclosures before we don't have to check if there is a prefix or suffix
		if counted && count == 0 {
			r.hasPrefixEnclosure = false
			r.hasSuffixEnclosure = false
		} else {
			r.hasPrefixEnclosure = bytes.HasPrefix(input[i], r.p.Characters.Enclosure)
			r.hasSuffixEnclosure = bytes.HasSuffix(input[i], r.p.Characters.Enclosure)
		}

		// debug.Debug("r.hasPrefixEnclosure: %v, r.hasSuffixEnclosure: %v, count: %d", r.hasPrefixEnclosure, r.hasSuffixEnclosure, count)

		if !r.hasPrefixEnclosure || !r.hasSuffixEnclosure {
			if len(r.sbuf) != 0 {
				r.sbuf = append(r.sbuf, r.p.Characters.Delimiter...)
				r.sbuf = append(r.sbuf, input[i]...)

				if r.hasSuffixEnclosure && !bytes.HasSuffix(input[i], r.p.Characters.DoubleEnclosure) {
					if r.p.Exclude != nil && slices.Contains(r.p.Exclude, i) {
					} else {
						// we know for sure that the suffix exists
						r.sbuf = cleanFunc(r.sbuf, false, true)
						data = append(data, r.sbuf...)
						pos = append(pos, uint32(len(r.sbuf)))
					}
					r.sbuf = make([]byte, 0)
					r.inside = false
					enclosurecheck = false
				} else {
					enclosurecheck = true
				}
			} else {
				// we are missing the second Enclosure, add to the r.sbuf
				r.sbuf = append(r.sbuf, input[i]...)
			}
			continue
		}

		if count%2 == 0 && r.hasPrefixEnclosure && r.hasSuffixEnclosure {
			if r.p.Exclude != nil && slices.Contains(r.p.Exclude, i) {
			} else {
				// we know for sure it has a prefix and suffix
				input[i] = cleanFunc(input[i], true, true)
				data = append(data, input[i]...)
				pos = append(pos, uint32(len(input[i])))
			}
			r.inside = false
			continue
		}

		// edge-case where ""| appears
		// or where count%2 != 0
		if count != 2 {
			r.sbuf = append(r.sbuf, input[i]...)
			continue
		}

		if r.p.Exclude != nil && slices.Contains(r.p.Exclude, i) {
		} else {
			input[i] = cleanFunc(input[i], false, false)
			data = append(data, input[i]...)
			pos = append(pos, uint32(len(input[i])))
		}
	}

	return
}

// record turns one cut record into the header or a row of the current batch.
// It returns false when the Reader was closed. token is a slice of the current
// block; nothing but a batch of that block may keep it.
func (r *Reader) record(token []byte) bool {
	if len(token) == 0 {
		return true
	}

	if r.p.Start > 1 && r.startCounter < r.p.Start {
		if r.startCounter+1 == r.p.Start {
			r.p.Characters.ReturnEarly = false
		}
		r.startCounter++
		return true
	}

	if r.headerRead {
		if r.skipped < r.skip {
			r.skipped++
			return true
		}
		if r.limit >= 0 && r.row == uint64(r.limit) {
			r.limited = true
			return true
		}
	}

	chunk := token
	if r.p.Characters.Excel {
		chunk = bytes.TrimSuffix(chunk, r.p.Characters.Delimiter)
	}
	// only clean the PHP characters in the scanner when using Nd
	if r.p.NewlineDelimited && r.p.Characters.PHP {
		chunk = r.p.Characters.ReplaceBackslashEscapedEnclosure(chunk)
	}
	// if no delimiter was set we detect it once
	if !r.delimSet && len(r.p.Characters.Delimiter) == 0 {
		r.p.Characters.SetDelimiter(characters.DetectDelimiter(chunk))
	}

	if !r.enclosureSet && len(r.p.Characters.Enclosure) == 0 {
		prevCount := r.enclDetectCount
		var encl characters.Character
		encl, r.enclDetectCount, _ = characters.DetectEnclosureWithDelimiter(chunk, r.p.Characters.Delimiter)
		if prevCount < r.enclDetectCount || r.enclDetectCount != -1 {
			if debug.DebugLog {
				debug.Debug("prevCount (%d) < enclDetectCount (%d): %s < %s", prevCount, r.enclDetectCount, r.p.Characters.Enclosure, encl)
			}
			r.p.Characters.SetEnclosure(encl)
		}
		if r.p.Characters.DelimiterCount != 0 {
			r.enclosureSet = true
		}
	}

	// header has to be set once
	if r.p.Characters.DelimiterCount == 0 {
		// we have to count the delimiters in the header to know for row scanning
		// how many fields we need
		r.p.Characters.DelimiterCount = bytes.Count(chunk, r.p.Characters.Delimiter)
		r.delimSet = true
	}

	// we have to process a header once
	if !r.headerRead {
		// a trim would have been sufficient but turns out some files have multiple
		chunk = util.ReplaceAll(chunk, characters.BOM_UTF8, []byte{})

		// some files have empty cols before the header, we should drop them, slight speed impact but worth the fix
		if len(util.ReplaceAll(chunk, r.p.Characters.Delimiter, characters.Character_EMPTY)) == 0 {
			return true
		}

		r.split.excludeExisting(chunk)

		// an unnamed last column is an Excel export only when the first row ends with a delimiter as well
		if bytes.HasSuffix(chunk, r.p.Characters.Delimiter) {
			r.pendingHeader = append([]byte(nil), chunk...)
			r.headerRead = true
			return true
		}

		if names := r.split.parseHeader(chunk); names != nil {
			r.headerRead = true
			r.sendHeader(names)
		}
		return true
	}

	count, outside := r.p.Characters.DelimCounted, r.p.Characters.DelimOutsideCounted
	// reset the count
	r.p.Characters.DelimCounted = 0
	r.p.Characters.DelimOutsideCounted = 0

	if r.pendingHeader != nil {
		r.split.keepLastColumn = !bytes.HasSuffix(chunk, r.p.Characters.Delimiter)
		names := r.split.parseHeader(r.pendingHeader)
		r.pendingHeader = nil
		if r.p.Characters.Excel {
			chunk = bytes.TrimSuffix(chunk, r.p.Characters.Delimiter)
		}
		r.sendHeader(names)
	}

	return r.add(chunk, count, outside)
}
