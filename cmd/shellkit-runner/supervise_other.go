//go:build !unix

package main

import (
	"errors"
	"os/exec"
)

// The runner targets linux/darwin; process-group control is a build-time no-op
// elsewhere so the package still compiles. errNoProcGroups keeps kill attempts
// honest on a platform where they cannot work.
var errNoProcGroups = errors.New("process-group control unsupported on this platform")

func setpgidAttr(*exec.Cmd) {}

func termGroup(int) error { return errNoProcGroups }

func killGroupHard(int) error { return errNoProcGroups }

func signaledExit(*exec.ExitError) (int, bool) { return 0, false }
