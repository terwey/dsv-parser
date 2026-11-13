// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Yorick Terweijden

package dsv

import (
	"encoding/csv"
	"io"
	"strconv"
	"testing"
)

// BenchmarkBuiltin_Read reads the rows of BenchmarkReader_Run with
// encoding/csv, on the one goroutine that package uses. The cases are named
// Rows/Options/columns. ReuseRecord makes a record valid until the next Read,
// as a record of Reader.Read is; Default allocates every record, as
// Reader.ReadStrings does.
func BenchmarkBuiltin_Read(b *testing.B) {
	benchmarks := []struct {
		name  string
		form  form
		reuse bool
	}{
		{name: "Enclosed/Default", form: enclosed},
		{name: "Enclosed/ReuseRecord", form: enclosed, reuse: true},

		{name: "EnclosedOneLine/Default", form: enclosedOneLine},
		{name: "EnclosedOneLine/ReuseRecord", form: enclosedOneLine, reuse: true},

		{name: "NoEnclosure/Default", form: plain},
		{name: "NoEnclosure/ReuseRecord", form: plain, reuse: true},
	}

	for _, bb := range benchmarks {
		for _, columns := range []int{20, 200} {
			b.Run(bb.name+"/"+strconv.Itoa(columns)+"_columns", func(b *testing.B) {
				header, rows, count := generateRows(columns, bb.form)

				b.SetBytes(int64(len(rows)))
				b.ReportAllocs()
				b.ResetTimer()

				r := csv.NewReader(&loop{header: header, rows: rows, times: b.N})
				r.ReuseRecord = bb.reuse

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
