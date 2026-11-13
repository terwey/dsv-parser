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
- Records go to the workers in batches of up to 4096 records or 256 KiB. On an input that trickles in, a record can
  wait for its batch to fill.
- A record with a broken enclosure can split differently from one run to the next: the splitter that repairs
  unescaped delimiters keeps state between the records of one worker.
- The record cutter panics with `stuck on chunking` when it cannot find the end of a record after 100 attempts, and
  delimiter detection writes one line through `log` when it finds no delimiter and falls back to a comma.

## Benchmark

```sh
go test -run '^$' -bench Reader .
```

`BenchmarkReader_Run` generates about a mebibyte of comma separated rows, feeds them to a `Reader` in a loop and
counts the records. Its `MB/s` column is the speed of cutting and splitting, with one worker per CPU.

The cases are named `Rows/Options/columns`. The rows have 20 or 200 columns of numbers, words, decimals, dates and
text, in three shapes:

| Rows | What they look like |
|---|---|
| `Enclosed` | Every field enclosed. Every fifth field holds a delimiter, a doubled enclosure and a line break. |
| `EnclosedOneLine` | The same without the line break. |
| `NoEnclosure` | Plain fields. |

The options are what the caller tells the `Reader` about the rows:

| Options | What is set |
|---|---|
| `AutoDetect` | Nothing. |
| `DelimiterAndEnclosureSet` | `WithDelimiter(",")` and `WithEnclosure("\"")`. |
| `EnclosureSetNewlineDelimited` | Both of those and `WithNewlineDelimited()`. |
| `EnclosureSet` | `WithDelimiter(",")` and `WithEnclosure("\"")`, on rows that have no enclosure. |
| `NoEnclosureSet` | `WithDelimiter(",")` and an empty `WithEnclosure`. |
| `NewlineDelimited` | `WithNewlineDelimited()`. |

`NoEnclosure/AutoDetect` is slower than the other three `NoEnclosure` cases. As long as no enclosure is known the
parser looks for one in every record, because a quote can first show up at row 200. A file without any pays for that
search on every record. Each of the other three cases switches the search off.

On an Apple M2 Max:

```
goos: darwin
goarch: arm64
pkg: github.com/terwey/dsv-parser
cpu: Apple M2 Max
BenchmarkReader_Run/Enclosed/AutoDetect/20_columns-12                               909  1321492 ns/op    793.60 MB/s  141426 B/op 0 allocs/op
BenchmarkReader_Run/Enclosed/AutoDetect/200_columns-12                             1015  1150536 ns/op    913.30 MB/s  159317 B/op 0 allocs/op
BenchmarkReader_Run/Enclosed/DelimiterAndEnclosureSet/20_columns-12                 890  1311495 ns/op    799.65 MB/s  144512 B/op 0 allocs/op
BenchmarkReader_Run/Enclosed/DelimiterAndEnclosureSet/200_columns-12               1045  1147644 ns/op    915.60 MB/s  106558 B/op 0 allocs/op
BenchmarkReader_Run/EnclosedOneLine/AutoDetect/20_columns-12                       2102   558555 ns/op   1877.67 MB/s  117352 B/op 0 allocs/op
BenchmarkReader_Run/EnclosedOneLine/AutoDetect/200_columns-12                      3036   364370 ns/op   2884.54 MB/s   75416 B/op 0 allocs/op
BenchmarkReader_Run/EnclosedOneLine/DelimiterAndEnclosureSet/20_columns-12         2032   548710 ns/op   1911.35 MB/s   96535 B/op 0 allocs/op
BenchmarkReader_Run/EnclosedOneLine/DelimiterAndEnclosureSet/200_columns-12        3093   362549 ns/op   2899.03 MB/s   74029 B/op 0 allocs/op
BenchmarkReader_Run/EnclosedOneLine/EnclosureSetNewlineDelimited/20_columns-12     6628   151227 ns/op   6935.12 MB/s   75908 B/op 0 allocs/op
BenchmarkReader_Run/EnclosedOneLine/EnclosureSetNewlineDelimited/200_columns-12    7273   139297 ns/op   7545.30 MB/s   50036 B/op 0 allocs/op
BenchmarkReader_Run/NoEnclosure/AutoDetect/20_columns-12                           1432   816371 ns/op   1284.58 MB/s  101879 B/op 0 allocs/op
BenchmarkReader_Run/NoEnclosure/AutoDetect/200_columns-12                          2292   486995 ns/op   2153.60 MB/s   77891 B/op 0 allocs/op
BenchmarkReader_Run/NoEnclosure/EnclosureSet/20_columns-12                         3380   343093 ns/op   3056.58 MB/s   63442 B/op 0 allocs/op
BenchmarkReader_Run/NoEnclosure/EnclosureSet/200_columns-12                       10344   114004 ns/op   9199.60 MB/s   41636 B/op 0 allocs/op
BenchmarkReader_Run/NoEnclosure/NoEnclosureSet/20_columns-12                       9004   125828 ns/op   8334.34 MB/s   48455 B/op 0 allocs/op
BenchmarkReader_Run/NoEnclosure/NoEnclosureSet/200_columns-12                     12948    92691 ns/op  11314.96 MB/s   30684 B/op 0 allocs/op
BenchmarkReader_Run/NoEnclosure/NewlineDelimited/20_columns-12                     9282   137065 ns/op   7651.06 MB/s   59705 B/op 0 allocs/op
BenchmarkReader_Run/NoEnclosure/NewlineDelimited/200_columns-12                   13082    90790 ns/op  11551.92 MB/s   44485 B/op 0 allocs/op
```

### On one CPU

```sh
go test -run '^$' -bench OneCPU .
```

`BenchmarkReader_ReadOneCPU` runs the same cases with `WithWorkers(1)` and `GOMAXPROCS` set to 1, and takes the
records from `Read`. This is the series to hold against `encoding/csv` with `ReuseRecord`.

### Against `encoding/csv`

```sh
go test -run '^$' -bench Builtin .
```

`BenchmarkBuiltin_Read` reads the same rows with Go's `encoding/csv`, which parses on one goroutine. Its cases are
named `Rows/Options/columns` too:

| Options | What is set |
|---|---|
| `Default` | Nothing. Every record is allocated, as with `ReadStrings`. |
| `ReuseRecord` | `ReuseRecord`. A record is valid until the next `Read`, as with `Read`. |

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
