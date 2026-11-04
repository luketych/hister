// SPDX-License-Identifier: AGPL-3.0-or-later
//go:build linux || darwin

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func commandProcessStopped(t *testing.T, pid int) bool {
	t.Helper()
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 {
			return true
		}
		t.Fatalf("cannot inspect test downloader process: %v", err)
	}
	return strings.HasPrefix(strings.TrimSpace(string(output)), "Z")
}

func TestMediaCommandsCancelStopsDownloaderProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps is needed to verify process termination")
	}
	pidsFile := filepath.Join(t.TempDir(), "processes")
	script := fmt.Sprintf("sleep 30 & child=$!; printf '%%s %%s' $$ \"$child\" > %q; wait", pidsFile)
	fixture := mediaCommandServer(t, script)
	rec := commandPost(t, fixture, "retry", fixture.documentID, fixture.failed.ID, fixture.owner)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body)
	}
	var accepted mediaCommandResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	var pids []int
	for len(pids) != 2 {
		if raw, err := os.ReadFile(pidsFile); err == nil {
			fields := strings.Fields(string(raw))
			if len(fields) == 2 {
				for _, field := range fields {
					pid, err := strconv.Atoi(field)
					if err != nil || pid < 1 {
						t.Fatalf("invalid test process PID: %q", raw)
					}
					pids = append(pids, pid)
				}
			}
		}
		if len(pids) == 2 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("fake downloader did not start both test processes")
		case <-tick.C:
		}
	}
	t.Cleanup(func() {
		for _, pid := range pids {
			if !commandProcessStopped(t, pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	cancelled := commandPost(t, fixture, "cancel", fixture.documentID, accepted.AttemptID, fixture.owner)
	if cancelled.Code != http.StatusOK {
		t.Fatalf("cancel = %d %s", cancelled.Code, cancelled.Body)
	}
	for _, pid := range pids {
		deadline := time.NewTimer(5 * time.Second)
		for !commandProcessStopped(t, pid) {
			select {
			case <-deadline.C:
				t.Fatalf("downloader process %d survived API cancellation", pid)
			case <-tick.C:
			}
		}
		deadline.Stop()
	}
}
