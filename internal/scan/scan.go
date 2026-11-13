// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Yorick Terweijden

// Package scan finds single bytes in records eight bytes at a time.
package scan

import (
	"encoding/binary"
	"math/bits"
)

const (
	ones = 0x0101010101010101
	low7 = 0x7f7f7f7f7f7f7f7f

	// gather moves the lowest bit of every byte of a word into its top byte
	gather = 0x0102040810204080
)

// before[b] has bit i set when b has an odd number of bits set below bit i.
var before = func() (table [256]uint8) {
	for b := range table {
		odd := false
		for i := 0; i < 8; i++ {
			if odd {
				table[b] |= 1 << i
			}
			if b&(1<<i) != 0 {
				odd = !odd
			}
		}
	}
	return table
}()

// Repeat returns a word that holds c in every byte.
func Repeat(c byte) uint64 {
	return ones * uint64(c)
}

// Equal returns a word with the top bit set of every byte of word that equals
// the byte repeated holds.
func Equal(word, repeated uint64) uint64 {
	x := word ^ repeated
	return ^((x&low7 + low7) | x | low7)
}

// Word returns the first eight bytes of data as a word: byte i of data is at
// bits 8i to 8i+7. Data shorter than eight bytes is filled up with zero bytes.
func Word(data []byte) uint64 {
	if len(data) >= 8 {
		return binary.LittleEndian.Uint64(data)
	}

	var word uint64
	for i := len(data) - 1; i >= 0; i-- {
		word = word<<8 | uint64(data[i])
	}
	return word
}

// Counts are the delimiters and enclosures of a record, counted up to some
// position in it.
type Counts struct {
	// Delimiters counts every delimiter.
	Delimiters int
	// Enclosures counts every enclosure.
	Enclosures int
	// Outside counts the delimiters with an even number of enclosures before them.
	Outside int
}

// Add counts data, which follows what c has counted so far, into c.
func (c *Counts) Add(data []byte, delimiter, enclosure byte) {
	repeatedDelimiter, repeatedEnclosure := Repeat(delimiter), Repeat(enclosure)
	delimiterCount, enclosureCount, outsideCount := c.Delimiters, c.Enclosures, c.Outside
	inside := enclosureCount&1 != 0

	for len(data) >= 8 {
		word := binary.LittleEndian.Uint64(data)
		data = data[8:]

		delimiters := Equal(word, repeatedDelimiter)
		enclosures := Equal(word, repeatedEnclosure)

		if enclosures == 0 {
			n := bits.OnesCount64(delimiters)
			delimiterCount += n
			if !inside {
				outsideCount += n
			}
			continue
		}

		enclosed := before[uint8((enclosures>>7)*gather>>56)]
		if inside {
			enclosed = ^enclosed
		}
		outsideCount += bits.OnesCount8(uint8((delimiters>>7)*gather>>56) &^ enclosed)
		delimiterCount += bits.OnesCount64(delimiters)

		n := bits.OnesCount64(enclosures)
		enclosureCount += n
		if n&1 != 0 {
			inside = !inside
		}
	}

	for _, b := range data {
		switch b {
		case enclosure:
			enclosureCount++
			inside = !inside
		case delimiter:
			delimiterCount++
			if !inside {
				outsideCount++
			}
		}
	}

	c.Delimiters, c.Enclosures, c.Outside = delimiterCount, enclosureCount, outsideCount
}

// notPlainStart is 0 for a letter, a digit and white space, and 1 for every other byte.
var notPlainStart = func() (table [256]byte) {
	for c := 0; c < len(table); c++ {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c == ' ', c >= '\t' && c <= '\r':
		default:
			table[c] = 1
		}
	}
	return table
}()

// PlainStart reports whether c is a letter, a digit or white space.
func PlainStart(c byte) bool {
	return notPlainStart[c] == 0
}

// PlainStarts reports whether every column of data, split at delimiter, is
// empty or starts with a letter, a digit or white space.
func PlainStarts(data []byte, delimiter byte) bool {
	n := len(data)
	if n == 0 {
		return true
	}
	if data[0] != delimiter && notPlainStart[data[0]] != 0 {
		return false
	}

	repeated := Repeat(delimiter)
	for base := 0; base < n; base += 8 {
		found := Equal(Word(data[base:]), repeated)
		if n-base < 8 {
			// the bytes Word filled up with are not data
			found &= 1<<(8*uint(n-base)) - 1
		}

		for found != 0 {
			start := base + bits.TrailingZeros64(found)>>3 + 1
			found &= found - 1
			if start < n && data[start] != delimiter && notPlainStart[data[start]] != 0 {
				return false
			}
		}
	}
	return true
}
