//go:build windows

package main

import (
	"golang.org/x/sys/windows"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

func replaceFile(from, to string) error {
	a, e := windows.UTF16PtrFromString(from)
	if e != nil {
		return e
	}
	b, e := windows.UTF16PtrFromString(to)
	if e != nil {
		return e
	}
	return windows.MoveFileEx(a, b, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
func prepareProcess(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000010, HideWindow: true}
}

var consoleSignalMu sync.Mutex
var kernel = windows.NewLazySystemDLL("kernel32.dll")
var attachConsole = kernel.NewProc("AttachConsole")
var freeConsole = kernel.NewProc("FreeConsole")
var setConsoleHandler = kernel.NewProc("SetConsoleCtrlHandler")
var ignoreControl = syscall.NewCallback(func(uint32) uintptr { return 1 })

func stopProcess(c *exec.Cmd, force bool) error {
	if !force {
		consoleSignalMu.Lock()
		defer consoleSignalMu.Unlock()
		freeConsole.Call()
		ok, _, e := attachConsole.Call(uintptr(c.Process.Pid))
		if ok == 0 {
			return e
		}
		defer freeConsole.Call()
		setConsoleHandler.Call(ignoreControl, 1)
		// The event arrives asynchronously; keep ignoring it a moment so it can't reach our own handler.
		defer func() { time.Sleep(time.Second); setConsoleHandler.Call(ignoreControl, 0) }()
		return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, 0)
	}
	k := exec.Command("taskkill", "/PID", fmtPID(c.Process.Pid), "/T", "/F")
	k.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return k.Run()
}
func detachProcess(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200, HideWindow: true}
}

// stageCommand passes AMP's command line through verbatim; cmd.exe and PowerShell parse their own quoting.
func stageCommand(exe, args string) *exec.Cmd {
	c := exec.Command(exe)
	c.SysProcAttr = &syscall.SysProcAttr{CmdLine: syscall.EscapeArg(exe) + " " + args, HideWindow: true}
	return c
}
