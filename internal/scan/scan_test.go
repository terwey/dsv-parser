// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Yorick Terweijden

package scan

import (
	"math/rand"
	"testing"
)

// countBytes counts data one byte at a time.
func countBytes(data []byte, delimiter, enclosure byte) Counts {
	var c Counts
	for _, b := range data {
		switch b {
		case enclosure:
			c.Enclosures++
		case delimiter:
			c.Delimiters++
			if c.Enclosures%2 == 0 {
				c.Outside++
			}
		}
	}
	return c
}

func TestCounts_Add(t *testing.T) {
	tests := []struct {
		name string
		data string
		want Counts
	}{
		{name: "delimiter inside an enclosure", data: `"a,b",c`, want: Counts{Delimiters: 2, Enclosures: 2, Outside: 1}},
		{name: "enclosure left open", data: `a,"b,c,d`, want: Counts{Delimiters: 3, Enclosures: 1, Outside: 1}},
		{name: "enclosure open across eight bytes", data: `aaaaaaa"bbbbbbbb,cccccccc,"d,e`, want: Counts{Delimiters: 3, Enclosures: 2, Outside: 1}},
		{name: "doubled enclosure", data: `"a ""b"", c",d`, want: Counts{Delimiters: 2, Enclosures: 6, Outside: 1}},
		{name: "no enclosure", data: `a,b,c,d,e,f,g,h,i,j`, want: Counts{Delimiters: 9, Outside: 9}},
		{name: "empty", data: ``, want: Counts{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Counts
			got.Add([]byte(tt.data), ',', '"')
			if got != tt.want {
				t.Errorf("Add() = %+v, want %+v", got, tt.want)
			}
		})
	}

	// counting in parts, eight bytes at a time, equals counting byte by byte
	t.Run("generated", func(t *testing.T) {
		alphabet := []byte(`ab,,""` + "\n\x00\xff'")
		rng := rand.New(rand.NewSource(1))
		for i := 0; i < 200000; i++ {
			data := make([]byte, rng.Intn(70))
			for j := range data {
				data[j] = alphabet[rng.Intn(len(alphabet))]
			}
			delimiter, enclosure := byte(','), byte('"')
			if i%5 == 0 {
				delimiter, enclosure = 0x00, 0xff
			}

			var got Counts
			cut := 0
			if len(data) != 0 {
				cut = rng.Intn(len(data) + 1)
			}
			got.Add(data[:cut], delimiter, enclosure)
			got.Add(data[cut:], delimiter, enclosure)

			if want := countBytes(data, delimiter, enclosure); got != want {
				t.Fatalf("Add(%q, cut at %d) = %+v, want %+v", data, cut, got, want)
			}
		}
	})
}

func TestWord(t *testing.T) {
	tests := []struct {
		name string
		data string
		want uint64
	}{
		{name: "short", data: "\x01\x02\x03", want: 0x030201},
		{name: "eight bytes", data: "\x01\x02\x03\x04\x05\x06\x07\x08", want: 0x0807060504030201},
		{name: "longer", data: "\x01\x02\x03\x04\x05\x06\x07\x08\x09", want: 0x0807060504030201},
		{name: "empty", data: "", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Word([]byte(tt.data)); got != tt.want {
				t.Errorf("Word() = %#x, want %#x", got, tt.want)
			}
		})
	}
}

func TestPlainStarts(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{name: "enclosed column", data: `1,"a",2`, want: false},
		{name: "enclosed first column", data: `"a",b`, want: false},
		{name: "enclosed last column", data: `aaaaaaaa,bbbbbbbb,'c'`, want: false},
		{name: "question mark", data: `a,?b?`, want: false},
		{name: "byte above ASCII", data: "a,\xc3\xa9", want: false},
		{name: "letters, digits and white space", data: "a,1, b,\tc\n2,d", want: true},
		{name: "empty columns", data: ",,a,,", want: true},
		{name: "enclosure inside a column", data: `a"b",c'd'`, want: true},
		{name: "empty", data: "", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlainStarts([]byte(tt.data), ','); got != tt.want {
				t.Errorf("PlainStarts() = %v, want %v", got, tt.want)
			}
		})
	}
}

func BenchmarkCounts_Add(b *testing.B) {
	row := []byte(`"31","south","7.11","2021-08-13","north, ""quoted"" and whole upper","36","inner","42.16","2021-08-18","north, ""quoted"" and whole south"` + "\n")
	var data []byte
	for len(data) < 64*1024 {
		data = append(data, row...)
	}

	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		var c Counts
		c.Add(data, ',', '"')
		if c.Delimiters == 0 {
			b.Fatal("no delimiter counted")
		}
	}
}
