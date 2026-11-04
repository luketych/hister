// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/server/document"
	"github.com/asciimoo/hister/server/indexer"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"
)

const commandSourceURL = "https://www.youtube.com/watch?v=abcdefghijk"

type mediaCommandFixture struct {
	cfg                     *config.Config
	idx                     *indexer.Indexer
	handler                 http.Handler
	owner, other, admin     *model.User
	documentID              string
	failed, foreign, global *model.MediaAttempt
}

func failedCommandAttempt(t *testing.T, documentID string, owner uint) *model.MediaAttempt {
	t.Helper()
	source, err := model.EnsureMediaSource(documentID, owner)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := model.AcceptMediaAttempt(source.ID, "failed-fixture", `{"binary":"/obsolete/private-command","output_root":"/obsolete/private-output"}`)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = model.ClaimMediaAttempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.FailMediaAttempt(attempt.ID, attempt.ClaimToken, "download_failed"); err != nil {
		t.Fatal(err)
	}
	return attempt
}

func mediaCommandServer(t *testing.T, script string) *mediaCommandFixture {
	t.Helper()
	if runtime.GOOS == "windows" && script != "" {
		t.Skip("media command fixture executes POSIX shell scripts; Windows subprocess fixture is not available")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cfg := testutil.Config(t)
	cfg.App.UserHandling, cfg.App.Public = true, true
	cfg.Server.Database = "media-commands.sqlite3"
	cfg.Indexer.DetectLanguages = false
	if err := cfg.UpdateBaseURL("http://127.0.0.1:4433"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveRules(); err != nil {
		t.Fatal(err)
	}
	testutil.InitModelWithConfig(t, cfg)
	previousStore := sessionStore
	sessionStore = newSessionStore([]byte(strings.Repeat("x", 32)), cfg.BaseURL(""), sessionMaxAge)
	t.Cleanup(func() { sessionStore = previousStore })
	owner, other := testutil.CreateUser(t, "owner"), testutil.CreateUser(t, "other")
	admin, err := model.CreateUser("admin", "password123", true)
	if err != nil {
		t.Fatal(err)
	}
	idx := newServerTestIndexer(t, cfg)
	id := document.GetDocID(owner.ID, commandSourceURL)
	for _, uid := range []uint{owner.ID, other.ID, 0} {
		if err := idx.Add(&document.Document{URL: commandSourceURL, UserID: uid, Text: "searchable source", Title: "Keep title", Processed: true}); err != nil {
			t.Fatal(err)
		}
	}
	idx.Close()
	root := t.TempDir()
	testutil.SeedMediaPair(t, id, owner.ID, testutil.WriteFile(t, root, "previous.webm", []byte("previous video")), testutil.WriteFile(t, root, "previous.mp3", []byte("previous audio")))
	fixture := &mediaCommandFixture{cfg: cfg, owner: owner, other: other, admin: admin, documentID: id}
	fixture.failed = failedCommandAttempt(t, id, owner.ID)
	fixture.foreign = failedCommandAttempt(t, document.GetDocID(other.ID, commandSourceURL), other.ID)
	fixture.global = failedCommandAttempt(t, commandSourceURL, 0)
	var binary string
	if script == "" {
		// Request-only tests must not need a platform-specific executable. If a
		// regression launches this placeholder, the resulting attempt is still
		// observable in the registry assertions below.
		binary = testutil.WriteFile(t, t.TempDir(), "must-not-execute", []byte("placeholder downloader\n"))
	} else {
		binary = testutil.WriteFile(t, t.TempDir(), "downloader", []byte("#!/bin/sh\n"+script+"\n"))
		if err := os.Chmod(binary, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg.Extractors = map[string]*config.Extractor{config.YTDownloaderName: {Enable: true, Options: map[string]any{"binary": binary, "output_dir": filepath.Join(root, "new"), "timeout": 30}}}
	fixture.idx = newServerTestIndexer(t, cfg)
	fixture.handler = registerEndpoints(cfg, fixture.idx)
	return fixture
}

func commandPost(t *testing.T, fixture *mediaCommandFixture, action, id, attempt string, user *model.User) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"document_id": id, "attempt_id": attempt})
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Content-Type": "application/json", "Origin": "hister://"}
	if user != nil {
		headers["X-Access-Token"] = user.Token
	}
	return testutil.ServeHTTP(t, fixture.handler, http.MethodPost, "/api/yt_downloader/"+action, strings.NewReader(string(body)), headers)
}

func TestMediaCommandsRetryIsIdempotentAndKeepsPreviousDownload(t *testing.T) {
	fixture := mediaCommandServer(t, "exit 1")
	var accepted struct {
		AttemptID string `json:"attempt_id"`
		Replayed  bool   `json:"replayed"`
	}
	for n := range 2 {
		rec := commandPost(t, fixture, "retry", fixture.documentID, fixture.failed.ID, fixture.owner)
		want := http.StatusAccepted
		if n > 0 {
			want = http.StatusOK
		}
		if rec.Code != want {
			t.Fatalf("retry %d = %d %s", n, rec.Code, rec.Body.String())
		}
		var result struct {
			AttemptID string `json:"attempt_id"`
			Replayed  bool   `json:"replayed"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.AttemptID == "" || result.AttemptID == fixture.failed.ID {
			t.Fatalf("retry did not identify actual replacement: %#v", result)
		}
		if n == 0 {
			accepted = result
		} else if result.AttemptID != accepted.AttemptID || !result.Replayed {
			t.Fatalf("duplicate retry created another attempt: %#v", result)
		}
		for _, secret := range []string{"private-command", "private-output", "request_json", "claim_token", "storage_root"} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("retry response leaked %s", secret)
			}
		}
	}
	waitCommandAttempt(t, accepted.AttemptID, model.MediaFailed)
	afterCompletion := commandPost(t, fixture, "retry", fixture.documentID, fixture.failed.ID, fixture.owner)
	if afterCompletion.Code != http.StatusOK {
		t.Fatalf("replay after terminal completion = %d %s", afterCompletion.Code, afterCompletion.Body)
	}
	var replayed mediaCommandResponse
	if err := json.Unmarshal(afterCompletion.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.AttemptID != accepted.AttemptID || replayed.State != model.MediaFailed || !replayed.Replayed {
		t.Fatalf("terminal replay started new work: %#v", replayed)
	}
	var replacement model.MediaAttempt
	if err := model.DB.First(&replacement, "id = ?", accepted.AttemptID).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(replacement.RequestJSON, "obsolete") {
		t.Fatal("retry reused obsolete historical configuration")
	}
	artifacts, err := model.CurrentMediaArtifacts(fixture.failed.SourceID)
	if err != nil || len(artifacts) != 2 {
		t.Fatalf("retry lost existing selection: %#v, %v", artifacts, err)
	}
	if doc := fixture.idx.GetByDocID(fixture.documentID); doc == nil || doc.Text != "searchable source" || doc.Title != "Keep title" || doc.AddCount != 1 {
		t.Fatalf("retry re-indexed or changed the source: %#v", doc)
	}
}

func TestMediaCommandsCancelTargetsOnlyThatAttempt(t *testing.T) {
	fixture := mediaCommandServer(t, "sleep 30")
	retry := commandPost(t, fixture, "retry", fixture.documentID, fixture.failed.ID, fixture.owner)
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry = %d %s", retry.Code, retry.Body)
	}
	var successor mediaCommandResponse
	if err := json.Unmarshal(retry.Body.Bytes(), &successor); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		rec := commandPost(t, fixture, "cancel", fixture.documentID, successor.AttemptID, fixture.owner)
		if rec.Code != http.StatusOK {
			t.Fatalf("cancel = %d %s", rec.Code, rec.Body)
		}
		var cancelled mediaCommandResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &cancelled); err != nil {
			t.Fatal(err)
		}
		if cancelled.AttemptID != successor.AttemptID || cancelled.State != model.MediaCancelled {
			t.Fatalf("cancel response = %#v", cancelled)
		}
	}
	// The cancelled attempt can be retried, but a late replay of its old cancel
	// command must never cancel its successor or the available previous pair.
	next := commandPost(t, fixture, "retry", fixture.documentID, successor.AttemptID, fixture.owner)
	if next.Code != http.StatusAccepted {
		t.Fatalf("retry cancellation = %d %s", next.Code, next.Body)
	}
	var nextAttempt mediaCommandResponse
	if err := json.Unmarshal(next.Body.Bytes(), &nextAttempt); err != nil {
		t.Fatal(err)
	}
	if late := commandPost(t, fixture, "cancel", fixture.documentID, successor.AttemptID, fixture.owner); late.Code != http.StatusOK {
		t.Fatalf("late cancellation = %d", late.Code)
	}
	var stored model.MediaAttempt
	if err := model.DB.First(&stored, "id = ?", nextAttempt.AttemptID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State == model.MediaCancelled {
		t.Fatal("stale cancellation cancelled the new attempt")
	}
	artifacts, err := model.CurrentMediaArtifacts(fixture.failed.SourceID)
	if err != nil || len(artifacts) != 2 {
		t.Fatalf("cancellation removed old ready pair: %#v, %v", artifacts, err)
	}
	if end := commandPost(t, fixture, "cancel", fixture.documentID, nextAttempt.AttemptID, fixture.owner); end.Code != http.StatusOK {
		t.Fatalf("cleanup cancellation = %d", end.Code)
	}
}

func TestMediaCommandsStrictBodyAndCSRF(t *testing.T) {
	fixture := mediaCommandServer(t, "")
	valid, err := json.Marshal(map[string]string{"document_id": fixture.documentID, "attempt_id": fixture.failed.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"retry", "cancel"} {
		for _, test := range []struct {
			name, body, origin string
			want               int
		}{
			{"missing IDs", `{}`, "hister://", http.StatusBadRequest},
			{"array", `[]`, "hister://", http.StatusBadRequest},
			{"extra JSON", string(valid) + `{}`, "hister://", http.StatusBadRequest},
			{"client options", strings.TrimSuffix(string(valid), "}") + `,"binary":"/foreign/command"}`, "hister://", http.StatusBadRequest},
			{"client ownership", strings.TrimSuffix(string(valid), "}") + `,"user_id":999}`, "hister://", http.StatusBadRequest},
			{"oversize body", string(valid) + strings.Repeat(" ", 65536), "hister://", http.StatusRequestEntityTooLarge},
			{"cross site", string(valid), "https://evil.invalid", http.StatusForbidden},
		} {
			t.Run(action+"/"+test.name, func(t *testing.T) {
				rec := testutil.ServeHTTP(t, fixture.handler, http.MethodPost, "/api/yt_downloader/"+action, strings.NewReader(test.body), map[string]string{
					"Content-Type": "application/json", "X-Access-Token": fixture.owner.Token, "Origin": test.origin,
				})
				if rec.Code != test.want {
					t.Fatalf("command = %d %s, want %d", rec.Code, rec.Body, test.want)
				}
			})
		}
	}
	var count int64
	if err := model.DB.Model(&model.MediaAttempt{}).Count(&count).Error; err != nil || count != 4 {
		t.Fatalf("rejected requests accepted work: count=%d, error=%v", count, err)
	}
}

func TestMediaCommandsIgnoreSubmissionOwnershipHeadersAndAllowAdminMutation(t *testing.T) {
	fixture := mediaCommandServer(t, "sleep 30")
	body, err := json.Marshal(map[string]string{"document_id": fixture.documentID, "attempt_id": fixture.failed.ID})
	if err != nil {
		t.Fatal(err)
	}
	rec := testutil.ServeHTTP(t, fixture.handler, http.MethodPost, "/api/yt_downloader/retry", strings.NewReader(string(body)), map[string]string{
		"Content-Type": "application/json", "Origin": "hister://", "X-Access-Token": fixture.owner.Token,
		"X-Hister-Public": "true", "X-Hister-Target-User-ID": "999",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("own retry = %d %s", rec.Code, rec.Body)
	}
	var result mediaCommandResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var stored model.MediaAttempt
	if err := model.DB.First(&stored, "id = ?", result.AttemptID).Error; err != nil {
		t.Fatal(err)
	}
	var frozen media.Request
	if err := json.Unmarshal([]byte(stored.RequestJSON), &frozen); err != nil {
		t.Fatal(err)
	}
	if stored.SourceID != fixture.failed.SourceID || frozen.UserID != fixture.owner.ID || frozen.URL != commandSourceURL {
		t.Fatalf("headers rebound command ownership: %#v, %#v", stored, frozen)
	}
	for _, prior := range []*model.MediaAttempt{fixture.foreign, fixture.global} {
		source, err := model.GetMediaSourceByID(prior.SourceID)
		if err != nil {
			t.Fatal(err)
		}
		retry := commandPost(t, fixture, "retry", source.DocumentID, prior.ID, fixture.admin)
		if retry.Code != http.StatusAccepted {
			t.Fatalf("admin retry = %d %s", retry.Code, retry.Body)
		}
		var accepted mediaCommandResponse
		if err := json.Unmarshal(retry.Body.Bytes(), &accepted); err != nil {
			t.Fatal(err)
		}
		if cancelled := commandPost(t, fixture, "cancel", source.DocumentID, accepted.AttemptID, fixture.admin); cancelled.Code != http.StatusOK {
			t.Fatalf("admin cancel = %d %s", cancelled.Code, cancelled.Body)
		}
	}
}

func TestMediaCommandsRespectDisabledDownloaderAndCurrentRules(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{true: "disabled", false: "URL policy"}[disabled], func(t *testing.T) {
			fixture := mediaCommandServer(t, "sleep 30")
			if disabled {
				fixture.idx.Close()
				fixture.cfg.Extractors = nil
				fixture.idx = newServerTestIndexer(t, fixture.cfg)
				fixture.handler = registerEndpoints(fixture.cfg, fixture.idx)
				if rec := commandPost(t, fixture, "retry", fixture.documentID, "unknown", fixture.owner); rec.Code != http.StatusNotFound {
					t.Fatalf("disabled service disclosed a nonexistent attempt: %d", rec.Code)
				}
			} else if err := model.SaveUserRules(fixture.owner.ID, &config.Rules{Skip: &config.Rule{ReStrs: []string{"youtube"}}}); err != nil {
				t.Fatal(err)
			}
			rec := commandPost(t, fixture, "retry", fixture.documentID, fixture.failed.ID, fixture.owner)
			want := http.StatusNotAcceptable
			if disabled {
				want = http.StatusConflict
			}
			if rec.Code != want {
				t.Fatalf("ineligible retry = %d %s, want %d", rec.Code, rec.Body, want)
			}
			var count int64
			if err := model.DB.Model(&model.MediaAttempt{}).Count(&count).Error; err != nil || count != 4 {
				t.Fatalf("ineligible retry accepted work: %d, %v", count, err)
			}
			if !disabled {
				doc := fixture.idx.GetByDocID(fixture.documentID)
				doc.SetIgnoreSkipRules(true)
				if err := fixture.idx.Save(doc); err != nil {
					t.Fatal(err)
				}
				if retry := commandPost(t, fixture, "retry", fixture.documentID, fixture.failed.ID, fixture.owner); retry.Code != http.StatusAccepted {
					t.Fatalf("saved manual override was ignored: %d %s", retry.Code, retry.Body)
				}
			}
		})
	}
}

func TestMediaCommandsReadyOutcomeWinsAndForeignSuspensionIsNotLeaked(t *testing.T) {
	fixture := mediaCommandServer(t, "")
	var ready model.MediaAttempt
	if err := model.DB.First(&ready, "source_id = ? AND state = ?", fixture.failed.SourceID, model.MediaReady).Error; err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"retry", "cancel"} {
		if rec := commandPost(t, fixture, action, fixture.documentID, ready.ID, fixture.owner); rec.Code != http.StatusConflict {
			t.Fatalf("%s modified a completed attempt: %d %s", action, rec.Code, rec.Body)
		}
	}
	if err := model.SuspendMediaTransfer(fixture.foreign.SourceID, "another-document", fixture.other.ID); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"retry", "cancel"} {
		if rec := commandPost(t, fixture, action, fixture.documentID, fixture.foreign.ID, fixture.owner); rec.Code != http.StatusNotFound {
			t.Fatalf("foreign suspended attempt leaked via %s: %d %s", action, rec.Code, rec.Body)
		}
	}
	var count int64
	if err := model.DB.Model(&model.MediaAttempt{}).Count(&count).Error; err != nil || count != 4 {
		t.Fatalf("completed/foreign attempts accepted work: count=%d, error=%v", count, err)
	}
}

func TestMediaCommandsSingleUserWriteModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("single-user retry fixture executes a POSIX shell downloader")
	}
	for _, token := range []string{"", "test-token"} {
		t.Run(map[bool]string{true: "token", false: "local private"}[token != ""], func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			cfg := testutil.Config(t)
			cfg.App.AccessToken, cfg.App.Public = token, token != ""
			cfg.Server.Database = "single-user.sqlite3"
			if err := cfg.UpdateBaseURL("http://127.0.0.1:4433"); err != nil {
				t.Fatal(err)
			}
			if err := cfg.SaveRules(); err != nil {
				t.Fatal(err)
			}
			testutil.InitModelWithConfig(t, cfg)
			previousStore := sessionStore
			sessionStore = newSessionStore([]byte(strings.Repeat("x", 32)), cfg.BaseURL(""), sessionMaxAge)
			t.Cleanup(func() { sessionStore = previousStore })
			idx := newServerTestIndexer(t, cfg)
			if err := idx.Add(&document.Document{URL: commandSourceURL, Text: "source", Processed: true}); err != nil {
				t.Fatal(err)
			}
			idx.Close()
			failed := failedCommandAttempt(t, commandSourceURL, 0)
			binary := testutil.WriteFile(t, t.TempDir(), "downloader", []byte("#!/bin/sh\nexit 1\n"))
			if err := os.Chmod(binary, 0o755); err != nil {
				t.Fatal(err)
			}
			cfg.Extractors = map[string]*config.Extractor{config.YTDownloaderName: {Enable: true, Options: map[string]any{"binary": binary, "output_dir": t.TempDir(), "timeout": 30}}}
			idx = newServerTestIndexer(t, cfg)
			handler := registerEndpoints(cfg, idx)
			body, err := json.Marshal(map[string]string{"document_id": commandSourceURL, "attempt_id": failed.ID})
			if err != nil {
				t.Fatal(err)
			}
			headers := map[string]string{"Content-Type": "application/json", "Origin": "hister://"}
			if token != "" {
				if rec := testutil.ServeHTTP(t, handler, http.MethodPost, "/api/yt_downloader/retry", strings.NewReader(string(body)), headers); rec.Code != http.StatusForbidden {
					t.Fatalf("token-less public retry = %d", rec.Code)
				}
				headers["X-Access-Token"] = token
			}
			rec := testutil.ServeHTTP(t, handler, http.MethodPost, "/api/yt_downloader/retry", strings.NewReader(string(body)), headers)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("authorized single-user retry = %d %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestMediaCommandsPublicReadOnlyCannotMutate(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.App.Public = true
	cfg.Server.Database = "read-only-public.sqlite3"
	testutil.InitModelWithConfig(t, cfg)
	idx := newServerTestIndexer(t, cfg)
	if err := idx.Add(&document.Document{URL: commandSourceURL, Text: "shared", Processed: true}); err != nil {
		t.Fatal(err)
	}
	failed := failedCommandAttempt(t, commandSourceURL, 0)
	fixture := &mediaCommandFixture{cfg: cfg, idx: idx, handler: registerEndpoints(cfg, idx)}
	for _, action := range []string{"retry", "cancel"} {
		if rec := commandPost(t, fixture, action, commandSourceURL, failed.ID, nil); rec.Code != http.StatusForbidden {
			t.Fatalf("public read-only %s = %d %s", action, rec.Code, rec.Body)
		}
	}
}

func waitCommandAttempt(t *testing.T, id, state string) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var attempt model.MediaAttempt
		if err := model.DB.First(&attempt, "id = ?", id).Error; err != nil {
			t.Fatal(err)
		}
		if attempt.State == state {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("attempt did not become %s: %#v", state, attempt)
		case <-tick.C:
		}
	}
}

func TestMediaCommandsRejectReadAccessAsMutationAuthority(t *testing.T) {
	fixture := mediaCommandServer(t, "")
	for _, action := range []string{"retry", "cancel"} {
		for _, test := range []struct {
			name, docID, attemptID string
			user                   *model.User
			want                   int
		}{
			{"anonymous", fixture.documentID, fixture.failed.ID, nil, http.StatusForbidden},
			{"another owner", fixture.documentID, fixture.failed.ID, fixture.other, http.StatusNotFound},
			{"foreign attempt on own source", fixture.documentID, fixture.foreign.ID, fixture.owner, http.StatusNotFound},
			{"shared source reader", commandSourceURL, fixture.global.ID, fixture.owner, http.StatusNotFound},
			{"unknown source", "unknown", fixture.failed.ID, fixture.owner, http.StatusNotFound},
		} {
			t.Run(action+"/"+test.name, func(t *testing.T) {
				rec := commandPost(t, fixture, action, test.docID, test.attemptID, test.user)
				if rec.Code != test.want {
					t.Fatalf("mutation = %d %s, want %d", rec.Code, rec.Body.String(), test.want)
				}
			})
		}
	}
	var count int64
	if err := model.DB.Model(&model.MediaAttempt{}).Count(&count).Error; err != nil || count != 4 {
		t.Fatalf("denied commands changed work: count=%d, error=%v", count, err)
	}
}
