// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Yorick Terweijden

package dsv_test

import (
	"fmt"
	"io"
	"log"
	"strings"
	"sync/atomic"
	"testing/iotest"

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

// printer is a Handler that prints every record under its row number.
type printer struct{}

func (printer) Record(row uint64, flat []byte, lengths []uint32) error {
	fmt.Printf("row %d: %s\n", row, flat[lengths[0]:])
	return nil
}

func (printer) Close() error {
	return nil
}

func ExampleReader_Checkpoint() {
	input := "id,name\n1,Ada\n2,Bob\n3,Cyd\n4,Dee\n5,Eve\n6,Fay\n7,Gus\n8,Hal\n"

	// the stream breaks inside the sixth record
	broken := io.MultiReader(strings.NewReader(input[:42]), iotest.ErrReader(io.ErrUnexpectedEOF))

	// a small buffer puts the checkpoints of this short input close together
	options := []dsv.Option{dsv.WithWorkers(1), dsv.WithBufferSize(16), dsv.WithCheckpoints()}

	r := dsv.NewReader(broken, options...)
	err := r.Run(func() (dsv.Handler, error) {
		return printer{}, nil
	})
	fmt.Println("stopped:", err)

	checkpoint := r.Checkpoint()
	fmt.Printf("checkpoint: %d bytes, row %d\n", checkpoint.Offset, checkpoint.Row)

	// the sender sends the input again from the checkpoint's offset
	rest := strings.NewReader(input[checkpoint.Offset:])

	r = dsv.NewReader(rest, append(options, dsv.WithCheckpoint(checkpoint))...)
	err = r.Run(func() (dsv.Handler, error) {
		return printer{}, nil
	})
	if err != nil {
		log.Fatal(err)
	}
	// Output:
	// row 1: Ada
	// row 2: Bob
	// row 3: Cyd
	// row 4: Dee
	// stopped: unexpected EOF
	// checkpoint: 32 bytes, row 4
	// row 5: Eve
	// row 6: Fay
	// row 7: Gus
	// row 8: Hal
}
