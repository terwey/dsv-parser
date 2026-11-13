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
)

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

// Counts are the enclosures of a record and its delimiters outside an
// enclosure, counted up to some position in it.
type Counts struct {
	// Enclosures counts every enclosure.
	Enclosures int
	// Outside counts the delimiters with an even number of enclosures before them.
	Outside int
}

// Add counts data, which follows what c has counted so far, into c.
func (c *Counts) Add(data []byte, delimiter, enclosure byte) {
	repeatedDelimiter, repeatedEnclosure := Repeat(delimiter), Repeat(enclosure)
	enclosureCount, outsideCount := c.Enclosures, c.Outside
	// open holds 1 in every byte while an enclosure is open
	var open uint64
	if enclosureCount&1 != 0 {
		open = ones
	}

	for len(data) >= 8 {
		word := binary.LittleEndian.Uint64(data)
		data = data[8:]

		delimiters := Equal(word, repeatedDelimiter) >> 7
		// byte i of before holds the number of enclosures in bytes 0 to i of word
		before := Equal(word, repeatedEnclosure) >> 7 * ones

		outsideCount += bits.OnesCount64(delimiters &^ (before ^ open))

		n := before >> 56
		enclosureCount += int(n)
		open ^= ones * (n & 1)
	}

	inside := open != 0
	for _, b := range data {
		switch b {
		case enclosure:
			enclosureCount++
			inside = !inside
		case delimiter:
			if !inside {
				outsideCount++
			}
		}
	}

	c.Enclosures, c.Outside = enclosureCount, outsideCount
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
