package cli

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/HACKE-RC/lc/internal/sessions"
)

func TestLongOptionPrefixes(t *testing.T) {
	cases := []struct {
		argv []string
		want *args
		code int
	}{
		{[]string{"--inter", "--no-s", "--lim", "3"}, &args{browse: true, noSize: true, limit: 3}, 0},
		{[]string{"--js", "--agent=omp"}, &args{json: true, limit: 40, agent: []string{"omp"}}, 0},
		// A flag's value is never expanded, even when it looks like an option.
		{[]string{"-p", "--js"}, &args{path: "--js", limit: 40}, 0},
		{[]string{"-an", "--vers"}, nil, 2}, // -n takes "--vers" as its value, which is not a number
		{[]string{"--except-a", "omp"}, nil, 2},
		{[]string{"--", "--js"}, &args{limit: 40, positional: []string{"--js"}}, 0},
	}
	for _, c := range cases {
		got, code, done := parse(c.argv, io.Discard, io.Discard)
		if code != c.code {
			t.Errorf("%q: exit %d, want %d", c.argv, code, c.code)
			continue
		}
		if c.want == nil {
			if !done {
				t.Errorf("%q: parsed, want an error", c.argv)
			}
			continue
		}
		// %+v prints nil and empty slices alike; only values matter here.
		if fmt.Sprintf("%+v", *got) != fmt.Sprintf("%+v", *c.want) {
			t.Errorf("%q:\n got %+v\nwant %+v", c.argv, got, c.want)
		}
	}
}

// --json must match Python's json.dump: floats keep a fraction and every
// non-ASCII character is escaped.
func TestJSONMatchesPython(t *testing.T) {
	var b bytes.Buffer
	writeJSON(&b, []sessions.Session{{Agent: "pi", ID: "x", Title: "héllo 😀", Updated: 1790416804}})
	want := `[
  {
    "agent": "pi",
    "id": "x",
    "cwd": "",
    "name": "h\u00e9llo \ud83d\ude00",
    "title": "h\u00e9llo \ud83d\ude00",
    "updated": 1790416804.0,
    "path": "",
    "bytes": 0,
    "note": ""
  }
]
`
	if b.String() != want {
		t.Fatalf("got\n%s", b.String())
	}
}
