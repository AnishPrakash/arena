// file: internal/core/limits.go
package core

import (
	"errors"
	"fmt"
	"time"
)

// Limits is one resource envelope. Every field is an absolute cap enforced by a
// *different* mechanism, deliberately: see the comments.
type Limits struct {
	// CPUms is enforced by RLIMIT_CPU inside the sandbox (SIGXCPU at the soft limit,
	// SIGKILL one second later at the hard limit). Catches busy loops.
	CPUms int64 `json:"cpu_ms"`

	// WallMs is enforced by the supervisor's own timer, which kills the whole process
	// group. Catches sleep(), blocking reads and deadlocks, which burn no CPU at all.
	WallMs int64 `json:"wall_ms"`

	// MemMB is enforced by the cgroup (memory.max) with swap disabled.
	// NOT by RLIMIT_AS: the JVM and Node reserve huge *virtual* address space at startup,
	// so an address-space limit fails them spuriously. cgroups count RSS, which is what
	// "memory used" actually means.
	MemMB int64 `json:"mem_mb"`

	// StdoutKB is enforced by RLIMIT_FSIZE (SIGXFSZ). Without it,
	// `while True: print("x")` fills the node's disk and your log pipeline in seconds.
	StdoutKB int64 `json:"stdout_kb"`

	// Pids is enforced by the cgroup pids controller. Fork-bomb defence.
	Pids int64 `json:"pids"`

	// StackMB is enforced by RLIMIT_STACK. Deep recursion should produce a clean RE,
	// not an ambiguous OOM.
	StackMB int64 `json:"stack_mb"`

	// OpenFiles is enforced by RLIMIT_NOFILE.
	OpenFiles int64 `json:"open_files"`
}

// LimitSet holds the two envelopes a submission passes through. They are separate on
// purpose: g++ with heavy templates legitimately needs ~512 MB and several seconds, and
// reusing run limits for compilation produces phantom CEs that look like judge bugs.
type LimitSet struct {
	Compile Limits `json:"compile"`
	Run     Limits `json:"run"`
}

// DefaultCompileLimits is the envelope every compiler gets unless a language overrides it.
func DefaultCompileLimits() Limits {
	return Limits{
		CPUms: 10_000, WallMs: 20_000, MemMB: 512,
		StdoutKB: 64, Pids: 128, StackMB: 64, OpenFiles: 256,
	}
}

// DefaultRunLimits is the envelope for participant code unless the problem overrides it.
func DefaultRunLimits() Limits {
	return Limits{
		CPUms: 2_000, WallMs: 6_000, MemMB: 256,
		StdoutKB: 1_024, Pids: 64, StackMB: 64, OpenFiles: 64,
	}
}

// wallRatio is how much slower than its CPU budget a program is allowed to be in wall
// time before we assume it is stuck rather than merely descheduled. 3x absorbs page
// faults, container start and scheduler jitter without letting a sleep() run forever.
const wallRatio = 3

// Normalize fills in derived and missing fields and clamps everything to sane bounds.
// It is total: it never returns an unusable Limits.
func (l Limits) Normalize() Limits {
	if l.CPUms <= 0 {
		l.CPUms = 2_000
	}
	if l.WallMs <= 0 {
		l.WallMs = l.CPUms * wallRatio
	}
	if l.WallMs < l.CPUms {
		// A wall limit below the CPU limit can never be satisfied.
		l.WallMs = l.CPUms * wallRatio
	}
	if l.MemMB <= 0 {
		l.MemMB = 256
	}
	if l.StdoutKB <= 0 {
		l.StdoutKB = 1_024
	}
	if l.Pids <= 0 {
		l.Pids = 64
	}
	if l.StackMB <= 0 {
		l.StackMB = 64
	}
	if l.OpenFiles <= 0 {
		l.OpenFiles = 64
	}
	return l
}

// ErrLimitOutOfRange guards against a malicious or fat-fingered problem definition
// consuming a whole runner node.
var ErrLimitOutOfRange = errors.New("limit out of allowed range")

// Validate enforces platform-wide ceilings. Problem setters choose limits; they do not get
// to choose limits that take the fleet down.
func (l Limits) Validate() error {
	switch {
	case l.CPUms > 30_000:
		return fmt.Errorf("%w: cpu_ms=%d exceeds 30000", ErrLimitOutOfRange, l.CPUms)
	case l.WallMs > 60_000:
		return fmt.Errorf("%w: wall_ms=%d exceeds 60000", ErrLimitOutOfRange, l.WallMs)
	case l.MemMB > 2_048:
		return fmt.Errorf("%w: mem_mb=%d exceeds 2048", ErrLimitOutOfRange, l.MemMB)
	case l.StdoutKB > 65_536:
		return fmt.Errorf("%w: stdout_kb=%d exceeds 65536", ErrLimitOutOfRange, l.StdoutKB)
	case l.Pids > 512:
		return fmt.Errorf("%w: pids=%d exceeds 512", ErrLimitOutOfRange, l.Pids)
	}
	return nil
}

// Scale applies a language's time multiplier and memory overhead.
//
// Interpreted and JIT languages pay a fixed startup tax that has nothing to do with the
// participant's algorithm: CPython ~30 ms, JVM ~120 ms plus ~100 MB of reserved heap.
// Charging that to the participant would make identical algorithms fail in Python and pass
// in C++. The multiplier lives in the language manifest, so it is data, not code.
func (l Limits) Scale(timeMultiplier float64, memOverheadMB int64) Limits {
	if timeMultiplier <= 0 {
		timeMultiplier = 1
	}
	out := l
	out.CPUms = int64(float64(l.CPUms) * timeMultiplier)
	out.WallMs = int64(float64(l.WallMs) * timeMultiplier)
	out.MemMB = l.MemMB + memOverheadMB
	return out.Normalize()
}

// CPUDuration and WallDuration are convenience accessors for the supervisor.
func (l Limits) CPUDuration() time.Duration  { return time.Duration(l.CPUms) * time.Millisecond }
func (l Limits) WallDuration() time.Duration { return time.Duration(l.WallMs) * time.Millisecond }
