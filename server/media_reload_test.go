// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"
)

func postReloadMediaCommand(t *testing.T, fixture *mediaCommandFixture, action, attempt string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"document_id": fixture.documentID, "attempt_id": attempt})
	if err != nil {
		t.Fatal(err)
	}
	return testutil.ServeHTTP(t, fixture.handler, http.MethodPost, "/hister/api/yt_downloader/"+action, strings.NewReader(string(body)), map[string]string{
		"Content-Type": "application/json", "Origin": "hister://", "X-Access-Token": fixture.owner.Token,
	})
}

func readReloadAttempt(t *testing.T, id string) model.MediaAttempt {
	t.Helper()
	var attempt model.MediaAttempt
	if err := model.DB.First(&attempt, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	return attempt
}

func pollReloadAttempt(t *testing.T, id, state string) model.MediaAttempt {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		attempt := readReloadAttempt(t, id)
		if attempt.State == state {
			return attempt
		}
		select {
		case <-deadline.C:
			t.Fatalf("attempt did not become %s: %#v", state, attempt)
		case <-tick.C:
		}
	}
}

// A settings save replaces the indexer (and its media queue). An active
// downloader must be interrupted as part of that replacement, without
// disturbing its source's previously selected artifacts.
func TestSettingsSaveDrainsActiveMediaAttempt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the deterministic blocking downloader fixture uses a POSIX shell")
	}
	startedPath := t.TempDir() + "/started"
	t.Setenv("HISTER_TEST_STARTED", startedPath)
	fixture := mediaCommandServer(t, `printf started >> "$HISTER_TEST_STARTED"; while :; do sleep 1; done`)

	// Reuse the real runtime/settings fixture. Reopen the indexer under its
	// runtime owner so applying settings exercises normal drain and cleanup.
	fixture.idx.Close()
	options := fixture.cfg.Extractors[config.YTDownloaderName].Options
	filename := filepath.Join(fixture.cfg.App.Directory, "config.yml")
	raw := fmt.Sprintf("app:\n  directory: %q\n  user_handling: true\n  public: true\nserver:\n  address: 127.0.0.1:0\n  base_url: http://127.0.0.1:4433/hister\n  database: media-commands.sqlite3\nindexer:\n  detect_languages: false\nextractors:\n  %s:\n    enable: true\n    options:\n      binary: %q\n      output_dir: %q\n      timeout: 30\n", fixture.cfg.App.Directory, config.YTDownloaderName, options["binary"], options["output_dir"])
	if err := os.WriteFile(filename, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(filename)
	if err != nil {
		t.Fatalf("load runtime configuration: %v", err)
	}
	fixture.cfg = loaded
	fixture.handler = startReloadTestRuntime(t, fixture.cfg)

	retry := postReloadMediaCommand(t, fixture, "retry", fixture.failed.ID)
	if retry.Code != 202 {
		t.Fatalf("retry = %d %s", retry.Code, retry.Body)
	}
	var accepted mediaCommandResponse
	if err := json.Unmarshal(retry.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("downloader did not enter its blocking section")
		}
		time.Sleep(10 * time.Millisecond)
	}

	headers := map[string]string{"Origin": "hister://", "X-Access-Token": fixture.admin.Token}
	revision, _ := readConfigurationState(t, fixture.handler, headers)
	body := fmt.Sprintf(`{"revision":%q,"edits":[{"path":"app.title","action":"set","value":"Reloaded"}]}`, revision)
	response := testutil.ServeHTTP(t, fixture.handler, http.MethodPost, "/hister/api/settings/apply", strings.NewReader(body), headers)
	if response.Code != http.StatusOK {
		t.Fatalf("settings apply = %d %s", response.Code, response.Body)
	}
	attempt := pollReloadAttempt(t, accepted.AttemptID, model.MediaInterrupted)
	if attempt.State != model.MediaInterrupted {
		t.Fatalf("old attempt remained active: %#v", attempt)
	}
	artifacts, err := model.CurrentMediaArtifacts(fixture.failed.SourceID)
	if err != nil || len(artifacts) != 2 {
		t.Fatalf("settings apply changed selected artifacts: %#v, %v", artifacts, err)
	}
	if !strings.Contains(response.Body.String(), `"saved":true`) {
		t.Fatalf("settings save response = %s", response.Body)
	}
	next := postReloadMediaCommand(t, fixture, "retry", accepted.AttemptID)
	if next.Code != 202 {
		t.Fatalf("retry on replacement runtime = %d %s", next.Code, next.Body)
	}
	var successor mediaCommandResponse
	if err := json.Unmarshal(next.Body.Bytes(), &successor); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		attempt := readReloadAttempt(t, successor.AttemptID)
		if attempt.State == model.MediaStarting || attempt.State == model.MediaDownloading {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new runtime did not start the replacement attempt: %#v", attempt)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cancelled := postReloadMediaCommand(t, fixture, "cancel", successor.AttemptID); cancelled.Code != http.StatusOK {
		t.Fatalf("cancel replacement attempt = %d %s", cancelled.Code, cancelled.Body)
	}
}
