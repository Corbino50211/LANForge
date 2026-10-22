//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func replaceFile(from, to string) error { return os.Rename(from, to) }
func prepareProcess(c *exec.Cmd)        { c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func stopProcess(c *exec.Cmd, force bool) error {
	s := syscall.SIGINT
	if force {
		s = syscall.SIGKILL
	}
	return syscall.Kill(-c.Process.Pid, s)
}
func detachProcess(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// stageCommand lets the shell parse AMP's command line, matching how AMP runs it.
func stageCommand(exe, args string) *exec.Cmd {
	c := exec.Command("/bin/sh", "-c", "exec "+exe+" "+args)
	prepareProcess(c)
	return c
}
