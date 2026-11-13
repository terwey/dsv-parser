// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Yorick Terweijden

package dsv

import (
	"encoding/binary"
	"math/bits"

	"github.com/terwey/dsv-parser/internal/scan"
)

// splitWholeRow splits a record whose fields are all whole in one pass. A
// field is whole when it holds no enclosure, or starts with one, ends with
// one, and holds only doubled ones in between. ok is false for any other
// record, and for a record splitRow would split another way; out and pos are
// then not to be used. The arguments are those of SplitRow.
func (r *splitter) splitWholeRow(colCount int, data []byte, count, outsideCount int) (out []byte, pos []uint32, ok bool) {
	cfg := r.p.Characters
	if r.colCount == 0 || len(r.p.Exclude) != 0 || len(data) == 0 || len(cfg.Delimiter) != 1 {
		return nil, nil, false
	}

	delimiter := cfg.Delimiter[0]
	var enclosure byte
	hasEnclosure := false
	switch len(cfg.Enclosure) {
	case 0:
	case 1:
		enclosure = cfg.Enclosure[0]
		hasEnclosure = true
	default:
		return nil, nil, false
	}
	if cfg.EnclosureClean != hasEnclosure || isLineBreak(delimiter) || (hasEnclosure && (isLineBreak(enclosure) || enclosure == delimiter)) {
		return nil, nil, false
	}

	inner, ok := r.splitWhole(data, delimiter, enclosure, hasEnclosure)
	if !ok {
		return nil, nil, false
	}
	fields := len(r.gpos)

	// from here on the decisions are those of splitRow
	if colCount == 0 {
		colCount = r.colCount
	}
	provided := count != 0 || outsideCount != 0
	if !provided {
		count = fields - 1 + inner
	}
	if cfg.Excel {
		count--
	}

	if colCount == count+1 {
		if inner == 0 {
			return r.gbuf, r.gpos, true
		}
		// with no delimiter outside an enclosure splitRow splits at every delimiter
		if outside := fields - 1; outside == 0 || outside >= count {
			return nil, nil, false
		}
	}

	if !hasEnclosure {
		return nil, nil, false
	}
	if !provided {
		outsideCount = fields - 1
	}
	if cfg.Excel {
		outsideCount--
	}
	if colCount != outsideCount+1 || fields != colCount {
		return nil, nil, false
	}
	// ColSplitFlat stops after count-1 delimiters
	if count > 0 && count-1 < fields-1 {
		return nil, nil, false
	}
	return r.gbuf, r.gpos, true
}

func isLineBreak(c byte) bool {
	return c == '\n' || c == '\r'
}

// splitWhole splits data at the delimiters outside an enclosure into r.gbuf
// and r.gpos. It removes the enclosure around a field, makes a doubled
// enclosure a single one, and makes CRLF and CR a LF. inner counts the
// delimiters inside an enclosure. ok is false when a field is not whole.
func (r *splitter) splitWhole(data []byte, delimiter, enclosure byte, hasEnclosure bool) (inner int, ok bool) {
	n := len(data)
	// eight bytes are written at a time, up to seven of them past the field
	if cap(r.gbuf) < n+8 {
		r.gbuf = make([]byte, 0, 2*n+8)
	}
	out := r.gbuf[:cap(r.gbuf)]
	lens := r.gpos[:0]

	// written counts the bytes of out in use, fieldOut those before the field
	written, fieldOut := 0, 0
	// the field starts at fieldStart; data[pending:] is not written yet
	fieldStart, pending := 0, 0
	// inside is true between the enclosure that opens a field and the one that closes it
	inside := false
	// closed is the index of the enclosure that closed the field, skip that
	// of the second enclosure of a doubled one
	closed, skip := -1, -1

	repeatedDelimiter, repeatedEnclosure, repeatedCR := scan.Repeat(delimiter), scan.Repeat(enclosure), scan.Repeat('\r')

	for base := 0; base < n; base += 8 {
		word := scan.Word(data[base:])
		found := scan.Equal(word, repeatedDelimiter) | scan.Equal(word, repeatedCR)
		if hasEnclosure {
			found |= scan.Equal(word, repeatedEnclosure)
		}
		if n-base < 8 {
			// the bytes Word filled up with are not data
			found &= 1<<(8*uint(n-base)) - 1
		}

		for found != 0 {
			i := base + bits.TrailingZeros64(found)>>3
			found &= found - 1

			switch c := data[i]; {
			case c == delimiter:
				if inside {
					inner++
					continue
				}
				if closed >= 0 {
					if i != closed+1 {
						return 0, false
					}
				} else {
					written = appendShort(out, written, data, pending, i)
				}
				lens = append(lens, uint32(written-fieldOut))
				fieldOut = written
				fieldStart, pending, closed = i+1, i+1, -1

			case c == '\r':
				if closed >= 0 {
					return 0, false
				}
				written = appendShort(out, written, data, pending, i)
				if i+1 == n || data[i+1] != '\n' {
					out[written] = '\n'
					written++
				}
				pending = i + 1

			default:
				if i == skip {
					continue
				}
				if !inside {
					if i != fieldStart || closed >= 0 {
						return 0, false
					}
					inside = true
					pending = i + 1
					continue
				}
				if i+1 < n && data[i+1] == enclosure {
					written = appendShort(out, written, data, pending, i+1)
					pending, skip = i+2, i+1
					continue
				}
				written = appendShort(out, written, data, pending, i)
				inside = false
				closed = i
			}
		}
	}

	if inside {
		return 0, false
	}
	if closed >= 0 {
		if closed != n-1 {
			return 0, false
		}
	} else {
		written = appendShort(out, written, data, pending, n)
	}
	lens = append(lens, uint32(written-fieldOut))

	r.gbuf, r.gpos = out[:written], lens
	return inner, true
}

// appendShort copies src[from:to] to dst[written:] and returns written plus
// the bytes copied. Up to eight bytes are copied as one word, which can write
// up to seven bytes more: dst holds at least eight bytes after written.
func appendShort(dst []byte, written int, src []byte, from, to int) int {
	n := to - from
	switch {
	case n <= 0:
		return written
	case n <= 8 && from+8 <= len(src):
		binary.LittleEndian.PutUint64(dst[written:], binary.LittleEndian.Uint64(src[from:]))
	case n >= 8 && n <= 16:
		binary.LittleEndian.PutUint64(dst[written:], binary.LittleEndian.Uint64(src[from:]))
		binary.LittleEndian.PutUint64(dst[written+n-8:], binary.LittleEndian.Uint64(src[to-8:]))
	default:
		copy(dst[written:], src[from:to])
	}
	return written + n
}
