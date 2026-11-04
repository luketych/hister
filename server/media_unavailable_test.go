// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asciimoo/hister/server/document"
	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
)

func TestMediaUnavailableDiagnosticsRequireLiveOwnerBinding(t *testing.T) {
	fixture := mediaCommandServer(t, "exit 1")
	artifacts, err := model.CurrentMediaArtifacts(fixture.failed.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	artifact := fixtureVideo(t, artifacts)
	path := filepath.Join(artifact.StorageRoot, artifact.RelativePath)
	var logs bytes.Buffer
	previousLogger := log.Logger
	log.Logger = zerolog.New(&logs)
	t.Cleanup(func() { log.Logger = previousLogger })
	read := func(method, path string, user *model.User) *httptest.ResponseRecorder {
		t.Helper()
		headers := map[string]string{}
		if user != nil {
			headers["X-Access-Token"] = user.Token
		}
		return testutil.ServeHTTP(t, fixture.handler, method, path, nil, headers)
	}
	unknown := read(http.MethodGet, "/api/yt_downloader/media?id=unknown", fixture.owner)
	var attemptsBefore int64
	if err := model.DB.Model(&model.MediaAttempt{}).Count(&attemptsBefore).Error; err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"missing", "invalid", "retired", "revoked"} {
		switch state {
		case "missing":
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		case "invalid":
			if err := os.Symlink(filepath.Join(artifact.StorageRoot, "previous.mp3"), path); err != nil {
				t.Fatal(err)
			}
		case "retired":
			if err := model.DB.Model(&artifact).Update("retired_at", time.Now().UTC()).Error; err != nil {
				t.Fatal(err)
			}
		case "revoked":
			if err := fixture.idx.Delete(fixture.documentID); err != nil {
				t.Fatal(err)
			}
			if err := fixture.idx.Add(&document.Document{URL: commandSourceURL, UserID: fixture.owner.ID, Text: "re-added", Processed: true}); err != nil {
				t.Fatal(err)
			}
		}
		for _, path := range []string{
			artifactURL(artifact), legacyArtifactURL(fixture.documentID, artifact),
			strings.Replace(artifactURL(artifact), "/yt_downloader/", "/yt_video_downloader/", 1),
			strings.Replace(legacyArtifactURL(fixture.documentID, artifact), "/yt_video_downloader/", "/yt_downloader/", 1),
		} {
			// A private artifact's state is never visible to another reader,
			// including an admin (media reads follow source-visibility policy).
			for _, user := range []*model.User{nil, fixture.other, fixture.admin} {
				logs.Reset()
				rec := read(http.MethodGet, path, user)
				if rec.Code != unknown.Code || rec.Body.String() != unknown.Body.String() || rec.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("%s leaked to foreign reader: %d %s", state, rec.Code, rec.Body)
				}
				if strings.Contains(logs.String(), "media artifact unavailable") {
					t.Fatalf("unauthorized lookup produced artifact diagnostics: %s", &logs)
				}
			}
			logs.Reset()
			rec := read(http.MethodGet, path, fixture.owner)
			if state == "revoked" {
				if rec.Code != unknown.Code || rec.Body.String() != unknown.Body.String() {
					t.Fatalf("re-added source disclosed revoked artifacts: %d %s", rec.Code, rec.Body)
				}
				continue
			}
			wantStatus, wantCode := http.StatusNotFound, "media_missing"
			if state == "invalid" {
				wantCode = "media_unavailable"
			}
			if state == "retired" {
				wantStatus, wantCode = http.StatusGone, "media_retired"
			}
			var diagnostic struct{ Code, Error string }
			if err := json.Unmarshal(rec.Body.Bytes(), &diagnostic); err != nil {
				t.Fatal(err)
			}
			if rec.Code != wantStatus || diagnostic.Code != wantCode || diagnostic.Error == "" {
				t.Fatalf("%s diagnostic = %d %#v", state, rec.Code, diagnostic)
			}
			if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("unsafe unavailable headers: %#v", rec.Header())
			}
			for _, secret := range []string{artifact.StorageRoot, artifact.RelativePath, "file://", "request_json", "claim_token", "private-output"} {
				if strings.Contains(rec.Body.String(), secret) || strings.Contains(logs.String(), secret) {
					t.Fatalf("unavailable response/log disclosed %q: %s %s", secret, rec.Body, &logs)
				}
			}
			if !strings.Contains(logs.String(), `"code":"`+wantCode+`"`) || !strings.Contains(logs.String(), `"artifact_id":"`+artifact.ID+`"`) {
				t.Fatalf("missing safe correlation log: %s", &logs)
			}
			head := read(http.MethodHead, path, fixture.owner)
			if head.Code != wantStatus || head.Body.Len() != 0 || head.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("unavailable HEAD = %d %q", head.Code, head.Body)
			}
		}
	}
	var attemptsAfter int64
	if err := model.DB.Model(&model.MediaAttempt{}).Count(&attemptsAfter).Error; err != nil || attemptsAfter != attemptsBefore {
		t.Fatalf("unavailable reads scheduled work: %d -> %d, %v", attemptsBefore, attemptsAfter, err)
	}
}

func TestMediaUnavailableRegistryFailureIsRedacted(t *testing.T) {
	fixture := mediaCommandServer(t, "exit 1")
	artifacts, err := model.CurrentMediaArtifacts(fixture.failed.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	artifact := fixtureVideo(t, artifacts)
	var logs bytes.Buffer
	previousLogger := log.Logger
	log.Logger = zerolog.New(&logs)
	t.Cleanup(func() { log.Logger = previousLogger })
	const callback = "test:media_registry_unavailable"
	if err := model.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "media_artifacts" {
			_ = tx.AddError(errors.New("private database path and SQL credentials"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := model.DB.Callback().Query().Remove(callback); err != nil {
			t.Error(err)
		}
	})
	for _, path := range []string{artifactURL(artifact), legacyArtifactURL(fixture.documentID, artifact)} {
		logs.Reset()
		rec := testutil.ServeHTTP(t, fixture.handler, http.MethodGet, path, nil, map[string]string{"X-Access-Token": fixture.owner.Token})
		var diagnostic struct{ Code, Error string }
		if err := json.Unmarshal(rec.Body.Bytes(), &diagnostic); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusServiceUnavailable || diagnostic.Code != "media_registry_unavailable" {
			t.Fatalf("failed registry read = %d %s", rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "private") || strings.Contains(logs.String(), "private") || strings.Contains(logs.String(), artifact.StorageRoot) {
			t.Fatalf("database failure leaked secrets: %s %s", rec.Body, &logs)
		}
	}
}

func TestMediaUnavailableSharedSourceDiagnosticsDoNotInventQueuedWork(t *testing.T) {
	fixture := mediaCommandServer(t, "sleep 30")
	root := t.TempDir()
	artifact := fixtureVideo(t, testutil.SeedMediaPair(t, commandSourceURL, 0,
		testutil.WriteFile(t, root, "shared.webm", []byte("video")), testutil.WriteFile(t, root, "shared.mp3", []byte("audio"))))
	if err := os.Remove(filepath.Join(artifact.StorageRoot, artifact.RelativePath)); err != nil {
		t.Fatal(err)
	}
	source, err := model.GetMediaSource(commandSourceURL)
	if err != nil {
		t.Fatal(err)
	}
	// A real accepted replacement can coexist with missing older artifacts.
	// The serving diagnostic must not claim there is no work scheduled either.
	failed, err := model.AcceptMediaAttempt(source.ID, "failed-replacement", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	failed, err = model.ClaimMediaAttempt(failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.FailMediaAttempt(failed.ID, failed.ClaimToken, "download_failed"); err != nil {
		t.Fatal(err)
	}
	retry := commandPost(t, fixture, "retry", commandSourceURL, failed.ID, fixture.admin)
	if retry.Code != http.StatusAccepted {
		t.Fatalf("replacement = %d %s", retry.Code, retry.Body)
	}
	var queued mediaCommandResponse
	if err := json.Unmarshal(retry.Body.Bytes(), &queued); err != nil {
		t.Fatal(err)
	}
	for _, headers := range []map[string]string{nil, {"X-Access-Token": fixture.other.Token}} {
		rec := testutil.ServeHTTP(t, fixture.handler, http.MethodGet, artifactURL(artifact), nil, headers)
		var diagnostic struct{ Code, Error string }
		if err := json.Unmarshal(rec.Body.Bytes(), &diagnostic); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusNotFound || diagnostic.Code != "media_missing" || strings.Contains(diagnostic.Error, "No download has been scheduled") {
			t.Fatalf("shared missing-file response invented work state: %d %s", rec.Code, rec.Body)
		}
	}
	var stored model.MediaAttempt
	if err := model.DB.First(&stored, "id = ?", queued.AttemptID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != model.MediaQueued && stored.State != model.MediaStarting && stored.State != model.MediaDownloading {
		t.Fatalf("unavailable read changed accepted work: %#v", stored)
	}
}

func TestMediaUnavailableDiagnosticsDoNotExposeSuspendedOrUnboundArtifacts(t *testing.T) {
	c := statusContext(t)
	const sourceURL = "https://www.youtube.com/watch?v=abcdefghijk"
	id := document.GetDocID(c.UserID, sourceURL)
	if err := c.Indexer.Add(&document.Document{URL: sourceURL, UserID: c.UserID, Text: "source", Processed: true}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	artifact := fixtureVideo(t, testutil.SeedMediaPair(t, id, c.UserID,
		testutil.WriteFile(t, root, "private.webm", []byte("video")), testutil.WriteFile(t, root, "private.mp3", []byte("audio"))))
	source, err := model.GetMediaSource(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Model(&artifact).Update("retired_at", time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.SuspendMediaTransfer(source.ID, "destination", 22); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{artifactURL(artifact), legacyArtifactURL(id, artifact), "/api/yt_downloader/media?id=unknown"} {
		rec := httptest.NewRecorder()
		c.Request, c.Response = httptest.NewRequest(http.MethodGet, path, nil), rec
		serveRegisteredMedia(c)
		if rec.Code != http.StatusNotFound || rec.Body.String() != "404 page not found\n" {
			t.Fatalf("unresolved binding disclosed artifact: %d %s", rec.Code, rec.Body)
		}
	}
}
