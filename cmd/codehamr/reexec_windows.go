//go:build windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
)

// reExec runs the freshly installed binary. Windows has no execve, so
// instead of the Unix same PID swap we spawn it as a child sharing our
// stdio, wait, and forward its exit code: one continuous session despite
// two brief PIDs.
//
// signal.Ignore(os.Interrupt) is load bearing: otherwise the parent's
// default Ctrl+C handler kills the parent while the child keeps running
// on the same console, interleaving the shell prompt with its TUI. Ignoring
// it funnels all console Ctrl+C to the child, which has its own handler.
func reExec(exe string, args []string, env []string) error {
	signal.Ignore(os.Interrupt)
	cmd := exec.Command(exe, args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		// Nonzero child exit is success here: the new binary ran, so
		// just propagate its code to the shell. Anything else (spawn
		// failure, missing binary) is a real error the caller surfaces,
		// falling through to the old in memory binary.
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	os.Exit(0)
	return nil
}
