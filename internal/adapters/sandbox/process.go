// file: internal/adapters/sandbox/process.go
package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/AnishPrakash/arena/internal/core"
	"github.com/AnishPrakash/arena/internal/ports"
)

// Process runs boxrun directly on the host, with NO container isolation.
//
// It exists so that domain and orchestration tests run in CI without a Docker daemon, and
// so a contributor on macOS can work on the judge logic. It is NEVER selected in
// production: NewSandbox refuses to build it when ARENA_ENV=prod.
//
// It still applies rlimits (so TLE and OLE behave), but it does NOT enforce memory limits
// (no cgroup) and does NOT isolate the filesystem or the network.
type Process struct{}

func NewProcess() *Process { return &Process{} }

func (p *Process) Run(ctx context.Context, spec ports.RunSpec) (core.ExecOutcome, error) {
	metaPath := filepath.Join(spec.BoxDir, "meta.json")
	_ = os.Remove(metaPath)

	args := []string{
		"-meta", metaPath,
		"-cpu-ms", strconv.FormatInt(spec.Limits.CPUms, 10),
		"-wall-ms", strconv.FormatInt(spec.Limits.WallMs, 10),
		"-fsize-kb", strconv.FormatInt(spec.Limits.StdoutKB, 10),
		"-stack-mb", strconv.FormatInt(spec.Limits.StackMB, 10),
		"-stdout", filepath.Join(spec.BoxDir, "stdout"),
		"-stderr", filepath.Join(spec.BoxDir, "stderr"),
		"-workdir", spec.BoxDir,
	}
	if spec.StdinPath != "" {
		args = append(args, "-stdin", filepath.Join(spec.BoxDir, filepath.Base(spec.StdinPath)))
	}
	args = append(args, "--")
	args = append(args, spec.Cmd...)

	runCtx, cancel := context.WithTimeout(ctx,
		time.Duration(spec.Limits.WallMs)*time.Millisecond+10*time.Second)
	defer cancel()

	_ = exec.CommandContext(runCtx, "boxrun", args...).Run()

	m, err := readMeta(metaPath)
	if err != nil {
		return core.ExecOutcome{}, err
	}
	return core.ExecOutcome{
		ExitCode: m.ExitCode, Signal: m.Signal, CPUms: m.CPUms, WallMs: m.WallMs,
		MaxRSSKB: m.MaxRSSKB, WallKill: m.WallKill, FSizeKill: m.FSizeKill,
		StdoutLen: m.StdoutLen,
	}, nil
}

func (p *Process) Warm(context.Context, string, int) error { return nil }
func (p *Process) Close() error                            { return nil }
