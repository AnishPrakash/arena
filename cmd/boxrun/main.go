// file: cmd/boxrun/main.go
//
// boxrun — the in-sandbox supervisor.
//
// It is PID 1 inside the judging container. It applies per-process rlimits, execs the
// target, enforces a wall-clock deadline on the whole process group, and reports exact
// resource usage from wait4(2).
//
// Build fully static so it runs in any base image without a libc dependency:
//
//	CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w" -o boxrun ./cmd/boxrun
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// Meta is written to the path given by -meta and read by the runner on the host through
// the shared /box bind mount. A file is used rather than stdout because the participant's
// program owns stdout.
type Meta struct {
	ExitCode  int   `json:"exit_code"`
	Signal    int   `json:"signal"`
	CPUms     int64 `json:"cpu_ms"`
	UserMs    int64 `json:"user_ms"`
	SysMs     int64 `json:"sys_ms"`
	WallMs    int64 `json:"wall_ms"`
	MaxRSSKB  int64 `json:"max_rss_kb"`
	WallKill  bool  `json:"wall_kill"`
	CPUKill   bool  `json:"cpu_kill"`
	FSizeKill bool  `json:"fsize_kill"`
	StdoutLen int64 `json:"stdout_len"`
	StderrLen int64 `json:"stderr_len"`
}

const (
	sigXCPU         = 24 // RLIMIT_CPU soft limit reached
	sigXFSZ         = 25 // RLIMIT_FSIZE exceeded — our output-flood detector
	sigKILL         = 9
	addrNoRandomize = 0x0040000 // personality(2) flag
)

// Limits are passed to stage 2 through the environment rather than flags, so the child's
// argv is exactly the participant's command line — which keeps /proc/self/cmdline honest
// and avoids any chance of a limit flag being consumed by the target program.
const (
	envCPUms   = "BOXRUN_CPU_MS"
	envFsizeKB = "BOXRUN_FSIZE_KB"
	envStackMB = "BOXRUN_STACK_MB"
	envNofile  = "BOXRUN_NOFILE"
	envNoASLR  = "BOXRUN_NO_ASLR"
	envStage2  = "BOXRUN_STAGE2"
)

func main() {
	if os.Getenv(envStage2) == "1" {
		stage2()
		return
	}
	parent()
}

// ---------------------------------------------------------------- stage 2

// stage2 runs in the child. It applies rlimits to ITSELF (they are inherited across exec)
// and then replaces itself with the target program.
func stage2() {
	args := os.Args[1:]
	if len(args) == 0 {
		fatal("boxrun stage2: no command")
	}

	setLimit := func(res int, soft, hard uint64, name string) {
		if err := syscall.Setrlimit(res, &syscall.Rlimit{Cur: soft, Max: hard}); err != nil {
			fatal(fmt.Sprintf("setrlimit %s: %v", name, err))
		}
	}

	// RLIMIT_CPU is in SECONDS, so it rounds up. Soft limit fires SIGXCPU; the hard limit
	// one second later fires SIGKILL, so a program that installs a SIGXCPU handler and
	// keeps spinning still dies. Both are needed — a handler-based escape is a real trick.
	if ms := envInt(envCPUms, 0); ms > 0 {
		sec := uint64((ms + 999) / 1000)
		if sec < 1 {
			sec = 1
		}
		setLimit(syscall.RLIMIT_CPU, sec, sec+1, "cpu")
	}

	// RLIMIT_FSIZE bounds every file write, and stdout is redirected to a file, so this is
	// what turns `while True: print("x")` into a clean OLE instead of a full disk.
	if kb := envInt(envFsizeKB, 0); kb > 0 {
		b := uint64(kb) * 1024
		setLimit(syscall.RLIMIT_FSIZE, b, b, "fsize")
	}

	// A deep-recursion program should produce a clean RE, not an ambiguous OOM.
	if mb := envInt(envStackMB, 0); mb > 0 {
		b := uint64(mb) * 1024 * 1024
		setLimit(syscall.RLIMIT_STACK, b, b, "stack")
	}

	if n := envInt(envNofile, 0); n > 0 {
		setLimit(syscall.RLIMIT_NOFILE, uint64(n), uint64(n), "nofile")
	}

	// No core dumps: a 256 MB core file written into the box on every segfault would be a
	// disk-fill vector all by itself.
	setLimit(syscall.RLIMIT_CORE, 0, 0, "core")

	// Deliberately NOT set: RLIMIT_AS (address space). The JVM and Node reserve huge
	// virtual mappings at startup, so an AS limit fails them spuriously. Memory is capped
	// by the cgroup, which measures RSS — the thing we actually mean.
	//
	// Also deliberately NOT set: RLIMIT_NPROC. It is per-UID, not per-process, so with a
	// shared sandbox UID one submission's threads would count against another's. The
	// cgroup pids controller is the correct mechanism and is applied by the runner.

	// Disable ASLR so allocation layout — and therefore cache behaviour and measured time —
	// is reproducible across runs of the same program.
	if os.Getenv(envNoASLR) == "1" {
		_, _, _ = syscall.Syscall(syscall.SYS_PERSONALITY, uintptr(addrNoRandomize), 0, 0)
	}

	bin, err := exec.LookPath(args[0])
	if err != nil {
		// Exit 127 is the shell convention for "command not found"; the runner maps it to
		// a clear internal error rather than blaming the participant.
		os.Exit(127)
	}
	if err := syscall.Exec(bin, args, os.Environ()); err != nil {
		os.Exit(127)
	}
}

// ---------------------------------------------------------------- parent

func parent() {
	var (
		metaPath = flag.String("meta", "/box/meta.json", "where to write metrics")
		cpuMs    = flag.Int64("cpu-ms", 0, "RLIMIT_CPU in milliseconds (rounded up to seconds)")
		wallMs   = flag.Int64("wall-ms", 0, "wall-clock deadline for the whole process group")
		fsizeKB  = flag.Int64("fsize-kb", 0, "RLIMIT_FSIZE in KiB")
		stackMB  = flag.Int64("stack-mb", 0, "RLIMIT_STACK in MiB")
		nofile   = flag.Int64("nofile", 0, "RLIMIT_NOFILE")
		stdinP   = flag.String("stdin", "", "file to feed as stdin (default /dev/null)")
		stdoutP  = flag.String("stdout", "/box/stdout", "file to capture stdout")
		stderrP  = flag.String("stderr", "/box/stderr", "file to capture stderr")
		noASLR   = flag.Bool("no-aslr", true, "disable address space randomisation")
		workdir  = flag.String("workdir", "/box", "working directory")
	)
	flag.Parse()
	target := flag.Args()
	if len(target) == 0 {
		fatal("boxrun: no command given (use -- before the command)")
	}

	self, err := os.Executable()
	if err != nil {
		fatal("boxrun: cannot locate self: " + err.Error())
	}

	// --- wire up I/O -------------------------------------------------------
	var stdin *os.File
	if *stdinP != "" {
		stdin, err = os.Open(*stdinP)
		if err != nil {
			fatal("boxrun: open stdin: " + err.Error())
		}
	} else {
		stdin, _ = os.Open(os.DevNull)
	}
	defer stdin.Close()

	stdout, err := os.OpenFile(*stdoutP, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		fatal("boxrun: open stdout: " + err.Error())
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(*stderrP, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		fatal("boxrun: open stderr: " + err.Error())
	}
	defer stderr.Close()

	// --- launch stage 2 ----------------------------------------------------
	cmd := exec.Command(self, target...)
	cmd.Dir = *workdir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = append(os.Environ(),
		envStage2+"=1",
		envCPUms+"="+strconv.FormatInt(*cpuMs, 10),
		envFsizeKB+"="+strconv.FormatInt(*fsizeKB, 10),
		envStackMB+"="+strconv.FormatInt(*stackMB, 10),
		envNofile+"="+strconv.FormatInt(*nofile, 10),
		envNoASLR+"="+boolStr(*noASLR),
	)
	// Setpgid puts the child in its own process group, so one kill(-pgid) reaps the whole
	// tree. Without it, a program that forks leaves orphans running after the "kill",
	// which then keep consuming CPU on a core we are about to hand to the next submission.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		writeMeta(*metaPath, Meta{ExitCode: 127})
		fatal("boxrun: start: " + err.Error())
	}
	pgid := cmd.Process.Pid

	// --- wall-clock watchdog ----------------------------------------------
	// This is the timer that catches sleep(), blocked reads and deadlocks — none of which
	// consume CPU, so RLIMIT_CPU never fires for them.
	wallKilled := make(chan bool, 1)
	var timer *time.Timer
	if *wallMs > 0 {
		timer = time.AfterFunc(time.Duration(*wallMs)*time.Millisecond, func() {
			_ = syscall.Kill(-pgid, sigKILL) // negative pid = the whole process group
			wallKilled <- true
		})
	}

	err = cmd.Wait()
	wall := time.Since(start)
	if timer != nil {
		timer.Stop()
	}

	// Belt and braces: even after the direct child exits, kill anything it left behind.
	// A submission must never leave a process running on a judging core.
	_ = syscall.Kill(-pgid, sigKILL)

	// --- collect metrics ---------------------------------------------------
	m := Meta{WallMs: wall.Milliseconds()}
	select {
	case <-wallKilled:
		m.WallKill = true
	default:
	}

	if ps := cmd.ProcessState; ps != nil {
		ws, _ := ps.Sys().(syscall.WaitStatus)
		if ws.Signaled() {
			m.Signal = int(ws.Signal())
			m.ExitCode = 128 + m.Signal
			switch m.Signal {
			case sigXCPU:
				m.CPUKill = true
			case sigXFSZ:
				m.FSizeKill = true
			}
		} else {
			m.ExitCode = ps.ExitCode()
		}
		if ru, ok := ps.SysUsage().(*syscall.Rusage); ok {
			m.UserMs = tvms(ru.Utime)
			m.SysMs = tvms(ru.Stime)
			m.CPUms = m.UserMs + m.SysMs
			m.MaxRSSKB = ru.Maxrss // Linux reports kilobytes
		}
	} else if err != nil {
		m.ExitCode = 127
	}

	// A wall kill that happens to land while the program is spinning shows up as SIGKILL;
	// mark it as a CPU kill too if the CPU budget was actually consumed, so the runner's
	// classifier has both facts.
	if *cpuMs > 0 && m.CPUms >= *cpuMs {
		m.CPUKill = true
	}

	if fi, err := os.Stat(*stdoutP); err == nil {
		m.StdoutLen = fi.Size()
	}
	if fi, err := os.Stat(*stderrP); err == nil {
		m.StderrLen = fi.Size()
	}

	writeMeta(*metaPath, m)

	// boxrun itself always exits 0. Its job is to REPORT what happened, not to propagate
	// it: a non-zero exit here would be indistinguishable from Docker or the runner
	// failing, and the runner must be able to tell "the program crashed" from
	// "our infrastructure broke".
	os.Exit(0)
}

// ---------------------------------------------------------------- helpers

func tvms(t syscall.Timeval) int64 { return t.Sec*1000 + int64(t.Usec)/1000 }

func writeMeta(path string, m Meta) {
	b, _ := json.Marshal(m)
	_ = os.WriteFile(path, b, 0o644)
}

func envInt(k string, d int64) int64 {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return d
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(126)
}
