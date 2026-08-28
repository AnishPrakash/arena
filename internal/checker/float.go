// file: internal/checker/float.go
package checker

import (
	"fmt"
	"math"
	"os"
	"strconv"

	"github.com/AnishPrakash/arena/internal/core"
)

func init() { Register("float", &Float{}) }

// Float compares numerically with a tolerance.
//
// It accepts a value if EITHER the absolute or the relative error is within epsilon. That
// pairing is not optional: absolute-only rejects correct answers of magnitude 1e9, and
// relative-only rejects correct answers near zero. Contest judges that get this wrong
// produce "wrong answer" on mathematically correct output.
type Float struct{}

func (f *Float) Check(expectedPath, actualPath string, cfg core.CheckerConfig) (bool, string, error) {
	eps := cfg.Epsilon
	if eps <= 0 {
		eps = 1e-6
	}
	ef, err := os.Open(expectedPath)
	if err != nil {
		return false, "", err
	}
	defer ef.Close()
	af, err := os.Open(actualPath)
	if err != nil {
		return false, "no output produced", nil
	}
	defer af.Close()

	es, as := newTokenScanner(ef), newTokenScanner(af)
	n := 0
	for {
		e, eok := es.next()
		a, aok := as.next()
		if !eok && !aok {
			return true, "", nil
		}
		if !eok || !aok {
			return false, fmt.Sprintf("token count mismatch at position %d", n+1), nil
		}
		n++

		ev, eerr := strconv.ParseFloat(e, 64)
		av, aerr := strconv.ParseFloat(a, 64)
		if eerr != nil || aerr != nil {
			// Non-numeric tokens fall back to exact string comparison, so a problem can
			// mix "YES/NO" with numbers.
			if e != a {
				return false, fmt.Sprintf("token %d: expected %q, got %q", n, trunc(e), trunc(a)), nil
			}
			continue
		}
		if math.IsNaN(av) || math.IsInf(av, 0) {
			return false, fmt.Sprintf("token %d: got %v", n, av), nil
		}
		absErr := math.Abs(ev - av)
		relErr := absErr / math.Max(1e-18, math.Abs(ev))
		if absErr > eps && relErr > eps {
			return false, fmt.Sprintf("token %d: expected %g, got %g (abs err %g > %g)",
				n, ev, av, absErr, eps), nil
		}
	}
}
