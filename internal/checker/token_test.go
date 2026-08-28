// file: internal/checker/token_test.go
package checker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AnishPrakash/arena/internal/core"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTokenCheckerToleratesFormatting(t *testing.T) {
	d := t.TempDir()
	exp := write(t, d, "e", "1 2 3\n")

	// Every one of these is a CORRECT answer that a naive exact comparison would reject.
	for _, got := range []string{"1 2 3", "1 2 3\n", "1 2 3\r\n", "1\n2\n3\n", "  1   2  3  \n\n"} {
		ok, msg, err := (&Token{}).Check(exp, write(t, d, "a", got), core.CheckerConfig{})
		if err != nil || !ok {
			t.Fatalf("token checker rejected %q: %s (%v)", got, msg, err)
		}
	}
	ok, msg, _ := (&Token{}).Check(exp, write(t, d, "a", "1 2 4\n"), core.CheckerConfig{})
	if ok {
		t.Fatal("1 2 4 must not be accepted")
	}
	if msg == "" {
		t.Fatal("a rejection must explain itself to the participant")
	}
}

func TestFloatCheckerHandlesBothErrorKinds(t *testing.T) {
	d := t.TempDir()
	cfg := core.CheckerConfig{Epsilon: 1e-6}

	// Near zero: relative error is meaningless, absolute must decide.
	ok, _, _ := (&Float{}).Check(write(t, d, "e1", "0.0000001\n"),
		write(t, d, "a1", "0.0000002\n"), cfg)
	if !ok {
		t.Fatal("absolute tolerance must accept tiny values")
	}
	// Large magnitude: absolute error is meaningless, relative must decide.
	ok, _, _ = (&Float{}).Check(write(t, d, "e2", "1000000000.0\n"),
		write(t, d, "a2", "1000000000.0001\n"), cfg)
	if !ok {
		t.Fatal("relative tolerance must accept large values")
	}
	ok, _, _ = (&Float{}).Check(write(t, d, "e3", "1.0\n"), write(t, d, "a3", "1.1\n"), cfg)
	if ok {
		t.Fatal("1.1 must not pass for 1.0 at eps=1e-6")
	}
}
