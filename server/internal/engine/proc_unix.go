//go:build unix

package engine

import (
	"os/exec"
	"syscall"
)

// setPgid 让子进程独立成组，取消时整组杀掉（evalscope 会自己再 fork）。
func setPgid(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killTree 先 SIGTERM 整组，超时后 SIGKILL。
func killTree(cmd *exec.Cmd, graceful bool) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	sig := syscall.SIGTERM
	if !graceful {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-pid, sig)
	_ = cmd.Process.Signal(sig)
}
