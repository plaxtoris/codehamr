//go:build windows

package tools

import "os/exec"

// setProcessGroup is a no operation on Windows: the bash tool needs /bin/sh and POSIX
// process groups, so it isn't expected to run on a native Windows host. This
// build exists only to keep CI cross compile green; default single process
// cancellation is fine for a non functional target.
func setProcessGroup(_ *exec.Cmd) {}
