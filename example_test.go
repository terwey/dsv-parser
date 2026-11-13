// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Yorick Terweijden

package dsv_test

import (
	"fmt"
	"io"
	"log"
	"strings"
	"sync/atomic"

	dsv "github.com/terwey/dsv-parser"
)

func ExampleReader_ReadStrings() {
	input := strings.NewReader("id|name\n1|\"Foo | Bar\"\n2|Baz\n")

	r := dsv.NewReader(input, dsv.WithOrder())
	defer r.Close()

	for {
		record, err := r.ReadStrings()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%q\n", record)
	}
	// Output:
	// ["id" "name"]
	// ["1" "Foo | Bar"]
	// ["2" "Baz"]
}

// counter is a Handler that counts the records and fields of one worker.
type counter struct {
	records, fields *uint64
}

func (c counter) Record(row uint64, flat []byte, lengths []uint32) error {
	atomic.AddUint64(c.records, 1)

	// lengths holds the length of each field, flat the fields back to back
	var start uint32
	for _, length := range lengths {
		field := flat[start : start+length]
		if len(field) != 0 {
			atomic.AddUint64(c.fields, 1)
		}
		start += length
	}
	return nil
}

func (c counter) Close() error {
	return nil
}

func ExampleReader_Run() {
	input := strings.NewReader("id;name;city\n1;Foo;Berlin\n2;Bar;\n3;Baz;Wien\n")

	r := dsv.NewReader(input)
	defer r.Close()

	header, err := r.Header()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%q\n", header)

	var records, fields uint64
	err = r.Run(func() (dsv.Handler, error) {
		return counter{records: &records, fields: &fields}, nil
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(records, "records,", fields, "fields with a value")
	// Output:
	// ["id" "name" "city"]
	// 3 records, 8 fields with a value
}
