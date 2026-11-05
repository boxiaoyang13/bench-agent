//go:build !unix

package engine

import "os/exec"

func setPgid(cmd *exec.Cmd) {}

func killTree(cmd *exec.Cmd, graceful bool) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
