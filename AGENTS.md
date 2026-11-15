# AGENTS.md

Package dsv reads delimiter separated files from an `io.Reader`: CSV, TSV, pipe separated, and the ones that
break RFC 4180. It finds the delimiter, the enclosure and the line ending itself, uses every core, and does not
hold the file in memory.

```sh
go get github.com/terwey/dsv-parser
```

```go
import dsv "github.com/terwey/dsv-parser"
```

> **"Must the records come one at a time, or in the order of the input?"**
>
> Yes → `Read`. No → `Run`. A `Reader` uses one of them.

## Read

```go
r := dsv.NewReader(file, dsv.WithOrder()) // without WithOrder: any order
defer r.Close()

header, err := r.ReadStrings() // the first record is the header
if err != nil {
	return err // io.EOF: empty input
}
for {
	record, err := r.ReadStrings()
	if err == io.EOF {
		break
	}
	if err != nil {
		return err
	}
	// len(record) can differ from len(header)
}
if n := r.RowCounter.Dropped; n != 0 {
	return fmt.Errorf("%d records could not be split", n) // the library handed these to nobody
}
```

`ReadStrings` returns strings you may keep. `Read` returns `[][]byte` that are valid until the next `Read`.

## Run

`Run` gives every worker its own `Handler` and calls it from that worker only.

```go
// counter counts the values of one column.
type counter struct {
	column int
	counts map[string]uint64 // this worker's
	mu     *sync.Mutex
	total  map[string]uint64 // everyone's
}

// flat holds the fields back to back, lengths the length of each.
func (c *counter) Record(row uint64, flat []byte, lengths []uint32) error {
	if c.column >= len(lengths) {
		return nil // shorter than the header
	}
	var start uint32
	for _, n := range lengths[:c.column] {
		start += n
	}
	field := flat[start : start+lengths[c.column]]
	c.counts[string(field)]++ // string copies; flat is reused after Record returns
	return nil
}

// Close runs once per worker, after its last record.
func (c *counter) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for value, n := range c.counts {
		c.total[value] += n
	}
	return nil
}
```

```go
r := dsv.NewReader(file)
defer r.Close()

header, err := r.Header() // Run does not pass the header to a Handler
if err != nil {
	return err // io.EOF: no header
}
column := -1
for i, name := range header {
	if string(name) == "city" {
		column = i
	}
}

var mu sync.Mutex
total := map[string]uint64{}
err = r.Run(func() (dsv.Handler, error) {
	return &counter{column: column, counts: map[string]uint64{}, mu: &mu, total: total}, nil
})
if err != nil {
	return err
}
if n := r.RowCounter.Dropped; n != 0 {
	return fmt.Errorf("%d records could not be split", n) // the library handed these to nobody
}
```

`row` numbers the records of the input from 1, the header not counted. Records arrive in any order.
An error from `Record` stops the `Reader`; `Run` returns it.

## Get it right

> **"Is anything between the input and `NewReader`: `io.ReadAll`, `bufio.Scanner`, `strings.Split`?"**
>
> Yes → remove it. A field can hold a line break. Only the library knows where a record ends.

> **"Are you sure of the delimiter and the enclosure?"**
>
> Yes → `dsv.WithDelimiter([]byte(";"))`, `dsv.WithEnclosure([]byte(`"`))`. No → set neither.

> **"Do you index a field without checking how many there are?"**
>
> Yes → a short record panics. Records have fewer or more fields than the header.

> **"Do you keep `flat`, `lengths`, or a record from `Read` after the call returns?"**
>
> Yes → copy it. The library reuses the memory.

> **"Do two `Handler`s write to the same variable in `Record`?"**
>
> Yes → give each its own and merge in `Close`.

## Options

| | |
|---|---|
| `WithOrder()` | `Read` returns the records in input order |
| `WithDelimiter(b)`, `WithEnclosure(b)` | set instead of detect; an empty enclosure means none |
| `WithNewlineDelimited()` | no field holds a line break; much faster |
| `WithStart(line)` | the header is on this line, counted from 1 |
| `WithSkip(n)`, `WithLimit(n)` | skip `n` records; stop after `n` |
| `WithExclude(columns...)` | drop these columns, counted from 0 |
| `WithBufferSize(bytes)` | the longest record; 16 MiB unless set |
| `WithWorkers(n)` | one per CPU unless set |

What comes back: the enclosure is removed, `""` is `"`, and a line break inside a field is `\n`. A record with no
value in any field is skipped.

The rest is in `README.md`; runnable examples are in `example_test.go`.
