package characters

import (
	"math/rand"
	"reflect"
	"testing"

	"github.com/terwey/dsv-parser/internal/debug"
)

func Test_findLeftOfPos(t *testing.T) {
	type args struct {
		data     []byte
		pos      int
		charSize int
	}
	type want struct {
		left      []byte
		charSize  int
		leftCount int
	}
	tests := []struct {
		name  string
		args  args
		want  want
		debug bool
	}{
		{
			args: args{
				data:     []byte(`"""fielda""",`),
				pos:      12,
				charSize: 1,
			},
			want: want{
				left:      []byte(`"""`),
				leftCount: 3,
				charSize:  1,
			},
			debug: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			debug.DebugLog = tt.debug
			left, charSize, leftCount := findLeftOfPos(tt.args.data, tt.args.pos, tt.args.charSize)
			if !reflect.DeepEqual(left, tt.want.left) {
				t.Errorf("findLeftOfPos() left = %v, left %v", left, tt.want.left)
			}
			if charSize != tt.want.charSize {
				t.Errorf("findLeftOfPos() charSize = %v, charSize %v", charSize, tt.want.charSize)
			}
			if leftCount != tt.want.leftCount {
				t.Errorf("findLeftOfPos() leftCount = %v, leftCount %v", leftCount, tt.want.leftCount)
			}
		})
	}
}

func Test_findRightOfPos(t *testing.T) {
	type args struct {
		data     []byte
		pos      int
		charSize int
	}
	type want struct {
		right      []byte
		charSize   int
		rightCount int
	}
	tests := []struct {
		name  string
		args  args
		want  want
		debug bool
	}{
		{
			args: args{
				data:     []byte(`"""fielda""",`),
				pos:      0,
				charSize: 1,
			},
			want: want{
				right:      []byte(`"""`),
				rightCount: 3,
				charSize:   1,
			},
			debug: false,
		},
		{
			args: args{
				data:     []byte(`"fielda",`),
				pos:      0,
				charSize: 1,
			},
			want: want{
				right:      []byte(`"`),
				rightCount: 1,
				charSize:   1,
			},
			debug: false,
		},
		{
			args: args{
				data:     []byte(`😤fielda😤,`),
				pos:      0,
				charSize: 1,
			},
			want: want{
				right:      []byte(`😤`),
				rightCount: 1,
				charSize:   4,
			},
			debug: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			debug.DebugLog = tt.debug
			right, charSize, rightCount := findRightOfPos(tt.args.data, tt.args.pos, tt.args.charSize)
			if !reflect.DeepEqual(right, tt.want.right) {
				t.Errorf("findRightOfPos() right = %v, right %v", right, tt.want.right)
			}
			if charSize != tt.want.charSize {
				t.Errorf("findRightOfPos() charSize = %v, charSize %v", charSize, tt.want.charSize)
			}
			if rightCount != tt.want.rightCount {
				t.Errorf("findRightOfPos() rightCount = %v, rightCount %v", rightCount, tt.want.rightCount)
			}
		})
	}
}

func Test_startsPlain(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		delimiter string
		want      bool
	}{
		{name: "enclosed column", data: `1,"a",2`, delimiter: ",", want: false},
		{name: "enclosed first column", data: `"a",b`, delimiter: ",", want: false},
		{name: "enclosed last column", data: `a,b,'c'`, delimiter: ",", want: false},
		{name: "question mark", data: `a,?b?`, delimiter: ",", want: false},
		{name: "byte above ASCII", data: "a,\xc3\xa9", delimiter: ",", want: false},
		{name: "enclosed column, multi-byte delimiter", data: `a<#>"b"`, delimiter: "<#>", want: false},
		{name: "letters, digits and white space", data: "a,1, b,\tc\n2,d", delimiter: ",", want: true},
		{name: "empty columns", data: ",,a,,", delimiter: ",", want: true},
		{name: "enclosure inside a column", data: `a"b",c'd'`, delimiter: ",", want: true},
		{name: "multi-byte delimiter", data: "a<#><#>b<#>", delimiter: "<#>", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := startsPlain([]byte(tt.data), Character(tt.delimiter)); got != tt.want {
				t.Errorf("startsPlain() = %v, want %v", got, tt.want)
			}
		})
	}

	// what startsPlain lets through, the search finds nothing in
	t.Run("generated", func(t *testing.T) {
		// runs of one letter or digit make the search take two and three bytes as the candidate
		alphabets := []string{
			"ab1 \t\n\r\"\"'*?-.{,,,;|\xc3\xa9",
			"aaAA1100??  \v\f,,\"'",
			"aZ09 ,,,?\"\"\"'*",
		}
		rng := rand.New(rand.NewSource(1))
		plain := 0
		for i := 0; i < 400000; i++ {
			alphabet := alphabets[i%len(alphabets)]
			data := make([]byte, 1+rng.Intn(48))
			for j := range data {
				data[j] = alphabet[rng.Intn(len(alphabet))]
			}
			delimiter := Character(",")
			if i%4 == 0 {
				delimiter = Character(",;")
			}
			if !startsPlain(data, delimiter) {
				continue
			}
			plain++

			char, count, occurence := detectEnclosureWithDelimiter(data, delimiter)
			if len(char) != 0 || count != -1 || occurence != -1 {
				t.Fatalf("detectEnclosureWithDelimiter(%q, %q) = %q, %d, %d, want nothing found", data, delimiter, char, count, occurence)
			}
		}
		if plain < 1000 {
			t.Fatalf("only %d generated inputs start plain", plain)
		}
	})
}
