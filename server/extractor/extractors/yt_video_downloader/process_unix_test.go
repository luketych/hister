// SPDX-License-Identifier: AGPL-3.0-or-later
//go:build linux

package yt_video_downloader

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCancellationStopsDownloaderProcessGroup(t *testing.T) {
	childPIDFile := filepath.Join(t.TempDir(), "child.pid")
	script := "sleep 30 & child=$!; printf '%s' \"$child\" > " + shellQuote(childPIDFile) + "; wait"
	binary := fakeDownloader(t, script)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runDownloaderCommand(ctx, binary, "download")
		done <- err
	}()

	deadline := time.Now().Add(3 * time.Second)
	var childPID int
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(childPIDFile); err == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("fake downloader did not start its child process")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("command error = %v, want context cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("downloader command did not stop after cancellation")
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if processStopped(childPID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("downloader child process %d survived cancellation", childPID)
}

func processStopped(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	// Linux reports a terminated but not-yet-reaped child as state Z.
	fields := strings.Fields(string(data[strings.LastIndex(string(data), ")")+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}
