//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// setpgidAttr makes the command lead a NEW process group whose id equals its pid,
// so the runner can signal the whole descendant tree with kill(-pgid). Both linux
// and darwin — the runner's only targets — support Setpgid.
func setpgidAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// termGroup delivers SIGTERM to the whole process group led by pgid (the
// negative-pid kill(2) convention). An already-reaped group returns ESRCH, which
// callers ignore.
func termGroup(pgid int) error {
	return syscall.Kill(-pgid, syscall.SIGTERM)
}

// killGroupHard delivers SIGKILL to the whole process group led by pgid.
func killGroupHard(pgid int) error {
	return syscall.Kill(-pgid, syscall.SIGKILL)
}

// signaledExit reports (128+signal, true) when the process was terminated by a
// signal, mirroring interp.DefaultExecHandler's convention; (0, false) otherwise.
func signaledExit(ee *exec.ExitError) (int, bool) {
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), true
	}
	return 0, false
}
