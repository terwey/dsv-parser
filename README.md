# dsv-parser

A Go library that reads delimiter separated values: CSV, TSV, pipe separated files and the many files that call
themselves CSV without following [RFC 4180](https://www.rfc-editor.org/rfc/rfc4180).

It takes an `io.Reader` and hands out records. It does not write anything and has no dependency on a database.

```go
import dsv "github.com/terwey/dsv-parser"
```

## What it reads

- Any delimiter and any enclosure, single or multi-byte: `,` `;` `|` a tab, `<#>`, an emoji, a Unicode separator.
- Delimiter, enclosure and line ending are detected when you do not give them.
  - Delimiters: tab, `|`, `~`, `,`, `;`, `$`, `^`, `#`, the ASCII record separator and the Unicode record, unit and
    invisible separators.
  - Enclosures: `"`, `""`, `"""`, `'` and `*`.
  - Line endings: LF, CRLF and CR.
- A delimiter or a line break inside an enclosed field.
- A delimiter inside an enclosure that was never escaped.
- Enclosures that are optional, doubled (`""`) or, in files written by PHP, escaped with a backslash (`\"`).
- Excel exports that end every line with a delimiter.
- A byte order mark, empty lines and lines of only delimiters before the header.
- Rows with fewer or more fields than the header.

## Install

```sh
go get github.com/terwey/dsv-parser
```

It needs Go 1.18 or later.

## Reading records

`Read` returns one record per call, one slice per field. The first record is the header.

```go
r := dsv.NewReader(file)
defer r.Close()

for {
	record, err := r.Read() // [][]byte
	if err == io.EOF {
		break
	}
	if err != nil {
		return err
	}
	// record and its fields are valid until the next Read
}
```

`ReadStrings` does the same and copies the fields into strings, which stay valid:

```go
r := dsv.NewReader(strings.NewReader("id|name\n1|\"Foo | Bar\"\n2|Baz\n"), dsv.WithOrder())
defer r.Close()

for {
	record, err := r.ReadStrings()
	if err == io.EOF {
		break
	}
	if err != nil {
		return err
	}
	fmt.Printf("%q\n", record)
}
// ["id" "name"]
// ["1" "Foo | Bar"]
// ["2" "Baz"]
```

**Records come back in the order the workers finish them, not in input order.** Pass `WithOrder()` when the order
matters. The header is always first.

## Handling records on the workers

`Read` funnels every record through the one goroutine that calls it. When the consumer can work in parallel, such as
one database connection per worker, use `Run`. It gives every worker a `Handler` of its own and calls it from that
worker only, so a `Handler` needs no lock for its own state.

```go
type Handler interface {
	Record(row uint64, flat []byte, lengths []uint32) error
	Close() error
}
```

- `row` is the number of the record in the input. The first record after the header is 1.
- `flat` holds the fields back to back and `lengths` the length of each field. This is the form the parser produces;
  nothing is copied to build it. Both are valid until `Record` returns.
- An error from `Record` stops the `Reader`, and `Run` returns it.
- `Close` is called once per `Handler`, after its last `Record`.

```go
type counter struct {
	records, fields *uint64
}

func (c counter) Record(row uint64, flat []byte, lengths []uint32) error {
	atomic.AddUint64(c.records, 1)

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

func (c counter) Close() error { return nil }

r := dsv.NewReader(file)
defer r.Close()

header, err := r.Header() // [][]byte, stays valid
if err != nil {
	return err
}

var records, fields uint64
err = r.Run(func() (dsv.Handler, error) {
	return counter{records: &records, fields: &fields}, nil
})
```

`Run` returns when the input is read and every `Handler` is closed. Use either `Run` or `Read` on one `Reader`.

## Options

| Option | What it does | Default |
|---|---|---|
| `WithDelimiter(b)` | Sets the delimiter. | Detected |
| `WithEnclosure(b)` | Sets the enclosure. An empty one means no enclosure, and reads the input as newline delimited. | Detected |
| `WithNewlineDelimited()` | Reads every line as one record: no field holds a line break. This is the fast path. | Off |
| `WithPHP()` | Reads a backslash before an enclosure as an escape. | Switched on when the input calls for it |
| `WithStart(line)` | The 1-based line the header is on. Lines before it are skipped; empty lines are not counted. | 1 |
| `WithExclude(cols...)` | Drops the columns at these 0-based indices from every record. | None |
| `WithBufferSize(n)` | The largest record, in bytes. | 16 MiB |
| `WithWorkers(n)` | The number of workers that split records. | One per CPU |
| `WithOrder()` | Makes `Read` return records in input order. | Off |

## What you get back

- **Header.** A line break inside a column name becomes a space. A column without a name is called `empty_column`,
  the next ones `empty_column__2`, `empty_column__3`.
- **Field values.** The enclosure is removed, a doubled enclosure becomes a single one, and CRLF and CR inside a field
  become LF.
- **Empty rows.** A record without a value in any field is skipped. Its row number is not reused.
- **Row width.** A record can hold fewer or more fields than the header. Padding or cutting it is up to you.
- **Counters.** `Reader.RowCounter` tells how many records went through the slower `encoding/csv` fallback and how
  many no splitter could split. It is complete once `Run` has returned or `Read` has returned `io.EOF`.

## Errors and stopping

- `Read` returns `io.EOF` after the last record. `Header` returns `io.EOF` for input without a header.
- A record longer than the buffer returns an error that wraps `bufio.ErrTooLong`. A wrong delimiter or enclosure
  can cause the same error, when the parser never finds the end of a record.
- An error from the input, or from a `Handler`, is returned as it is.
- Once `Read` has returned an error it returns that error on every later call.
- `Close` stops the `Reader` and waits for its goroutines. Call it when you stop before the end. A `Reader` closed
  before its input ended returns `ErrClosed`.

## How it works

One goroutine reads the input into large buffers and cuts it into records. A record is a slice of its buffer; it is
not copied. Batches of records go to the workers, which split them into fields and clean the values. That is the one
copy a record goes through.

Detection can change while a file is read: a quote first shows up at row 200, or the file turns out to be an Excel
export. Every batch is split under the detection state it was cut under, so a row does not depend on which worker
picks it up.

## Limits

- `Close` waits for a read of the input that is in flight. On an input that never returns, it does not return.
- With `WithOrder()`, records that finish ahead of a slow batch wait in memory until that batch is done.
- When records come close to the buffer size, a `Reader` holds up to five buffers per worker.
- Without `WithOrder()`, `Read` hands records over in batches of up to 128. On an input that trickles in, a record
  can wait for its batch to fill.
- A record with a broken enclosure can split differently from one run to the next: the splitter that repairs
  unescaped delimiters keeps state between the records of one worker.
- The record cutter panics with `stuck on chunking` when it cannot find the end of a record after 100 attempts, and
  delimiter detection writes one line through `log` when it finds no delimiter and falls back to a comma.

## Benchmark

```sh
go test -run '^$' -bench Reader .
```

`BenchmarkReader_Run` reads the rows of a fixture in a loop and drops the records. Its `MB/s` column is the speed of
cutting and splitting, with one worker per CPU. It has three cases:

- a pipe delimited file with enclosed fields and line breaks inside them,
- a file with a multi-byte delimiter and no enclosure,
- that same file read with `WithNewlineDelimited()`.

The second case is slow, the third is fast. As long as no enclosure is known the parser looks for one in every record,
because a quote can first show up at row 200. A file without any pays for that search on every record.
`WithNewlineDelimited()` and `WithEnclosure` switch the search off.

On an Apple M2 Max:

```
goos: darwin
goarch: arm64
pkg: github.com/terwey/dsv-parser
cpu: Apple M2 Max
BenchmarkReader_Run/pipe,_enclosed,_line_breaks_in_fields-12                 8140        141747 ns/op    3063.85 MB/s       34293 B/op           0 allocs/op
BenchmarkReader_Run/multi-byte_delimiter,_no_enclosure-12                       4     254635021 ns/op       3.14 MB/s    222889832 B/op     4830096 allocs/op
BenchmarkReader_Run/multi-byte_delimiter,_no_enclosure,_newline_delimited-12                 7114        166270 ns/op    4815.17 MB/s       43995 B/op           0 allocs/op
PASS
ok      github.com/terwey/dsv-parser    6.512s
```

## Tests

```sh
go test ./...
go test -tags debug ./...
go test -race .
```

`tests/` holds real-world files and, next to each, the records the parser returns for it as JSON lines.

The `debug` build tag switches on the tracing in `internal/debug`. Without it the tracing compiles to nothing.

## Where it comes from

This is the parser of [csv2sqlite](https://github.com/terwey/csv2sqlite), with its history since 2020. There it was
tied to a record writer that pulled in SQLite. Here it only parses.

## License

MIT, see [LICENSE](LICENSE).
