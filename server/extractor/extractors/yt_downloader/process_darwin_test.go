// SPDX-License-Identifier: AGPL-3.0-or-later
//go:build darwin

package yt_downloader

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDarwinCancellationStopsDownloaderChild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	binary := fakeDownloader(t, "sleep 30 & child=$!; printf '%s' \"$child\" > "+shellQuote(pidFile)+"; wait")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := runDownloaderCommand(ctx, binary, "download"); done <- err }()
	deadline := time.Now().Add(3 * time.Second)
	pid := 0
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(string(data))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("child never started")
	}
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("cancel did not stop command")
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := exec.Command("/bin/ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
		if err != nil || strings.HasPrefix(strings.TrimSpace(string(data)), "Z") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child remained alive after cancellation")
}
