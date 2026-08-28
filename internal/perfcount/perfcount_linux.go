//go:build linux

// Package perfcount opens hardware performance counters via perf_event_open(2).
//
// Retired instruction count is a strictly better efficiency metric than wall or CPU time
// for ranking algorithms: it is reproducible to roughly +/-0.1% and is immune to noisy
// neighbours, frequency scaling and cache state. It is also frequently unavailable, so
// every use site must probe first and fall back to normalised CPU time. See ADR-009.
package perfcount

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	sysPerfEventOpen = 298 // amd64

	typeHardware       = 0
	countHWInstructions = 1

	// perf_event_attr bit flags, in struct bit order.
	flagDisabled      = 1 << 0
	flagInherit       = 1 << 1
	flagExcludeKernel = 1 << 5
	flagExcludeHV     = 1 << 6
	flagEnableOnExec  = 1 << 12
)

// attr mirrors the kernel's perf_event_attr up to PERF_ATTR_SIZE_VER6 (112 bytes).
// The kernel accepts any size it recognises and zero-extends the rest.
type attr struct {
	Type             uint32
	Size             uint32
	Config           uint64
	SamplePeriod     uint64
	SampleType       uint64
	ReadFormat       uint64
	Bits             uint64
	WakeupEvents     uint32
	BPType           uint32
	Ext1             uint64
	Ext2             uint64
	BranchSampleType uint64
	SampleRegsUser   uint64
	SampleStackUser  uint32
	ClockID          int32
	SampleRegsIntr   uint64
	AuxWatermark     uint32
	SampleMaxStack   uint16
	_                uint16
}

// Open starts an instruction counter attached to pid (0 = the calling thread).
// The counter is created disabled and starts on the next execve, and inherit is set so
// threads and children created after that are counted too.
func Open(pid int) (fd int, err error) {
	a := attr{
		Type:   typeHardware,
		Config: countHWInstructions,
		Bits:   flagDisabled | flagInherit | flagExcludeKernel | flagExcludeHV | flagEnableOnExec,
	}
	a.Size = uint32(unsafe.Sizeof(a))

	r, _, errno := syscall.Syscall6(sysPerfEventOpen,
		uintptr(unsafe.Pointer(&a)),
		uintptr(pid),
		^uintptr(0), // cpu = -1 : any CPU
		^uintptr(0), // group_fd = -1 : leader
		0, 0)
	if errno != 0 {
		return -1, fmt.Errorf("perf_event_open: %w", errno)
	}
	return int(r), nil
}

// Read returns the counter value.
func Read(fd int) (uint64, error) {
	var buf [8]byte
	n, err := syscall.Read(fd, buf[:])
	if err != nil {
		return 0, err
	}
	if n != 8 {
		return 0, fmt.Errorf("perf counter: short read (%d bytes)", n)
	}
	var v uint64
	for i := 7; i >= 0; i-- {
		v = v<<8 | uint64(buf[i])
	}
	return v, nil
}

func Close(fd int) { _ = syscall.Close(fd) }

// Available probes once whether hardware instruction counting works here, and returns a
// human-readable reason when it does not. Callers cache the result for the process
// lifetime; the answer cannot change without a reboot.
func Available() (bool, string) {
	if b, err := os.ReadFile("/proc/sys/kernel/perf_event_paranoid"); err == nil {
		// >2 forbids even self-monitoring of userspace events.
		if len(b) > 0 && b[0] == '3' {
			return false, "kernel.perf_event_paranoid=3 forbids userspace perf events"
		}
	}
	fd, err := Open(0)
	if err != nil {
		return false, err.Error() + " (no virtualised PMU? common under WSL2, GCP and AWS shared instances)"
	}
	Close(fd)
	return true, ""
}
