//go:build unix

package main

import "syscall"

// reExec replaces the process image via execve(2). Same PID, so the parent
// shell waits on one process throughout, one continuous session. Success
// never returns; only exec failure (missing binary, wrong arch, no exec bit)
// surfaces as the error. Windows lacks execve and uses a spawn and wait
// fallback for the same effect.
func reExec(exe string, args []string, env []string) error {
	return syscall.Exec(exe, args, env)
}
