// file: internal/checker/checker.go
package checker

import (
	"fmt"
	"sync"

	"github.com/AnishPrakash/arena/internal/core"
)

// Checker compares a program's output against the expected output.
//
// Both are FILE PATHS, not strings. Outputs can be megabytes; loading them into memory on
// every test is how a long-running runner develops the "memory leak" the brief warns
// about. Every implementation here streams.
type Checker interface {
	// Check reports whether actual is acceptable, plus a short human diagnostic for the
	// participant when it is not.
	Check(expectedPath, actualPath string, cfg core.CheckerConfig) (ok bool, msg string, err error)
}

var (
	mu       sync.RWMutex
	registry = map[string]Checker{}
)

// Register adds a checker. New comparison semantics — a special judge for problems with
// multiple valid answers, an interactive checker, a graph-isomorphism comparator — are a
// new file plus one Register call in its init(). The judge never changes.
func Register(name string, c Checker) {
	mu.Lock()
	defer mu.Unlock()
	registry[name] = c
}

func Get(name string) (Checker, error) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("checker: unknown type %q", name)
	}
	return c, nil
}

func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	return out
}
