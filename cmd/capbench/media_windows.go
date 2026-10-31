package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// passMedia makes w inheritable and tells captured its handle value, since
// Windows has no fd 3.
func passMedia(cmd *exec.Cmd, w *os.File) {
	h := windows.Handle(w.Fd())
	windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT)
	cmd.SysProcAttr = &syscall.SysProcAttr{AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(h)}}
	cmd.Args = append(cmd.Args, "--media", fmt.Sprintf("fd:%d", h))
}
