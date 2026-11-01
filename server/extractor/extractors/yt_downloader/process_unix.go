// SPDX-License-Identifier: AGPL-3.0-or-later
//go:build linux || darwin

package yt_downloader

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Kill the process group so cancellation also stops downloader subprocesses
// such as ffmpeg. This is supported on Linux and macOS.
func configureDownloaderProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
}
