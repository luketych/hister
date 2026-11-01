// SPDX-License-Identifier: AGPL-3.0-or-later
//go:build !linux && !darwin

package yt_downloader

import "os/exec"

// On other platforms exec.CommandContext stops the downloader process itself.
// Process-tree termination is currently guaranteed only on Linux and macOS.
func configureDownloaderProcess(_ *exec.Cmd) {}
