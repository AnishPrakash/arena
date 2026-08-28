// file: internal/checker/token.go
package checker

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"github.com/AnishPrakash/arena/internal/core"
)

func init() {
	Register("token", &Token{})
	Register("exact", &Exact{})
}

// Token compares whitespace-separated tokens.
//
// This is the right DEFAULT for a contest. Exact byte comparison rejects a correct answer
// because the participant printed a trailing newline or used "\r\n" on Windows, which
// generates support tickets and destroys trust in the judge for reasons unrelated to
// algorithms.
type Token struct{}

func (t *Token) Check(expectedPath, actualPath string, _ core.CheckerConfig) (bool, string, error) {
	ef, err := os.Open(expectedPath)
	if err != nil {
		return false, "", err
	}
	defer ef.Close()
	af, err := os.Open(actualPath)
	if err != nil {
		// No output file at all means the program produced nothing — a wrong answer, not
		// an infrastructure failure.
		return false, "no output produced", nil
	}
	defer af.Close()

	es := newTokenScanner(ef)
	as := newTokenScanner(af)

	n := 0
	for {
		e, eok := es.next()
		a, aok := as.next()
		switch {
		case !eok && !aok:
			return true, "", nil
		case !eok:
			return false, fmt.Sprintf("extra output after token %d (got %q)", n, trunc(a)), nil
		case !aok:
			return false, fmt.Sprintf("output ended early: expected token %d to be %q", n+1, trunc(e)), nil
		case e != a:
			return false, fmt.Sprintf("token %d: expected %q, got %q", n+1, trunc(e), trunc(a)), nil
		}
		n++
	}
}

// Exact is byte-for-byte apart from a single optional trailing newline. Use it only for
// problems where formatting genuinely is the answer (ASCII art, fixed-width tables).
type Exact struct{}

func (x *Exact) Check(expectedPath, actualPath string, _ core.CheckerConfig) (bool, string, error) {
	e, err := os.ReadFile(expectedPath)
	if err != nil {
		return false, "", err
	}
	a, err := os.ReadFile(actualPath)
	if err != nil {
		return false, "no output produced", nil
	}
	et, at := trimTrailingNewline(e), trimTrailingNewline(a)
	if len(et) != len(at) {
		return false, fmt.Sprintf("length mismatch: expected %d bytes, got %d", len(et), len(at)), nil
	}
	for i := range et {
		if et[i] != at[i] {
			return false, fmt.Sprintf("first difference at byte %d", i), nil
		}
	}
	return true, "", nil
}

func trimTrailingNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// tokenScanner streams whitespace-separated tokens with a bounded buffer, so a 100 MB
// output costs 1 MB of memory rather than 100.
type tokenScanner struct{ sc *bufio.Scanner }

func newTokenScanner(r io.Reader) *tokenScanner {
	sc := bufio.NewScanner(bufio.NewReaderSize(r, 1<<20))
	sc.Split(bufio.ScanWords)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24) // allow single tokens up to 16 MiB
	return &tokenScanner{sc: sc}
}

func (t *tokenScanner) next() (string, bool) {
	if t.sc.Scan() {
		return t.sc.Text(), true
	}
	return "", false
}

func trunc(s string) string {
	if len(s) <= 40 {
		return s
	}
	return s[:40] + "..."
}
