// file: internal/langs/manifest.go
package langs

import (
	"fmt"

	"github.com/AnishPrakash/arena/internal/core"
)

// Manifest describes everything Arena needs to know about a language.
//
// THIS IS THE EXTENSION POINT. Adding Rust, Go, Kotlin or Haskell is one YAML file in
// languages/ plus one Dockerfile in images/. No Go source changes anywhere.
type Manifest struct {
	ID         string `yaml:"id"          json:"id"`
	Display    string `yaml:"display"     json:"display"`
	Image      string `yaml:"image"       json:"image"`
	SourceFile string `yaml:"source_file" json:"source_file"`

	// Compile is empty for interpreted languages.
	Compile []string `yaml:"compile" json:"compile,omitempty"`
	Run     []string `yaml:"run"     json:"run"`

	// TimeMultiplier and MemoryOverheadMB neutralise the fixed startup tax of interpreted
	// and JIT runtimes so that an identical algorithm gets an identical verdict across
	// languages. Data, not code — see core.Limits.Scale.
	TimeMultiplier   float64 `yaml:"time_multiplier"    json:"time_multiplier"`
	MemoryOverheadMB int64   `yaml:"memory_overhead_mb" json:"memory_overhead_mb"`

	// CompileLimits optionally overrides the platform default (e.g. Java needs more).
	CompileLimits *core.Limits `yaml:"compile_limits,omitempty" json:"-"`

	// Env is injected into the sandbox. Keep it minimal and deterministic.
	Env []string `yaml:"env,omitempty" json:"-"`

	Enabled bool `yaml:"enabled" json:"-"`
}

func (m *Manifest) Validate() error {
	switch {
	case m.ID == "":
		return fmt.Errorf("manifest: id is required")
	case m.Image == "":
		return fmt.Errorf("manifest %s: image is required", m.ID)
	case m.SourceFile == "":
		return fmt.Errorf("manifest %s: source_file is required", m.ID)
	case len(m.Run) == 0:
		return fmt.Errorf("manifest %s: run command is required", m.ID)
	}
	if m.TimeMultiplier <= 0 {
		m.TimeMultiplier = 1.0
	}
	return nil
}

func (m *Manifest) NeedsCompile() bool { return len(m.Compile) > 0 }

// EffectiveCompileLimits returns the language override or the platform default.
func (m *Manifest) EffectiveCompileLimits() core.Limits {
	if m.CompileLimits != nil {
		return m.CompileLimits.Normalize()
	}
	return core.DefaultCompileLimits()
}

// ScaleRunLimits applies the language's fairness adjustment to a problem's limits.
func (m *Manifest) ScaleRunLimits(problem core.Limits) core.Limits {
	return problem.Normalize().Scale(m.TimeMultiplier, m.MemoryOverheadMB)
}
