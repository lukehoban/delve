package proc

import (
	"syscall"

	sys "golang.org/x/sys/unix"
)

func PtraceAttach(pid int) error {
	return sys.PtraceAttach(pid)
}

func PtraceDetach(tid, sig int) error {
	_, _, err := sys.Syscall6(sys.SYS_PTRACE, sys.PT_DETACH, uintptr(tid), 1, uintptr(sig), 0, 0)
	if err != syscall.Errno(0) {
		return err
	}
	return nil
}

func PtraceCont(tid, sig int) error {
	_, _, err := sys.Syscall6(sys.SYS_PTRACE, sys.PTRACE_CONT, uintptr(tid), 1, uintptr(sig), 0, 0)
	if err != syscall.Errno(0) {
		return err
	}
	return nil
}

func PtraceSingleStep(tid int) error {
	_, _, err := sys.Syscall6(sys.SYS_PTRACE, sys.PT_STEP, uintptr(tid), 1, 0, 0, 0)
	if err != syscall.Errno(0) {
		return err
	}
	return nil
}
