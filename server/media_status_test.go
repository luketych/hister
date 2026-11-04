// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/server/document"
	"github.com/asciimoo/hister/server/extractor"
	yt "github.com/asciimoo/hister/server/extractor/extractors/yt_downloader"
	"github.com/asciimoo/hister/server/indexer"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"
)

func statusContext(t *testing.T) *webContext {
	t.Helper()
	cfg := testutil.Config(t)
	cfg.App.UserHandling = true
	cfg.Server.Database = "status.sqlite3"
	testutil.InitModelWithConfig(t, cfg)
	idx, err := indexer.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(idx.Close)
	return &webContext{Config: cfg, Indexer: idx, UserID: 11, Authenticated: true}
}

func readStatus(t *testing.T, c *webContext, id string) *media.Status {
	t.Helper()
	recorder := httptest.NewRecorder()
	c.Request, c.Response = httptest.NewRequest(http.MethodGet, "/api/yt_downloader/status?document_id="+url.QueryEscape(id), nil), recorder
	serveMediaStatus(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status read = %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("media status allowed stale caching")
	}
	var status media.Status
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"request_json", "claim_token", "storage_root", "relative_path", "binary", "output_root"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("status leaked internal field %s", secret)
		}
	}
	return &status
}

func TestMediaStatusReadinessAndPreviewUseOnlyCommittedRegistry(t *testing.T) {
	c := statusContext(t)
	const sourceURL = "https://www.youtube.com/watch?v=abcdefghijk"
	id := document.GetDocID(11, sourceURL)
	if err := c.Indexer.Add(&document.Document{
		URL: sourceURL, UserID: 11, Text: "searchable", Processed: true,
		Metadata: map[string]any{"yt_video_downloader_media": `{"video":["file:///not-ready.webm"]}`},
	}); err != nil {
		t.Fatal(err)
	}
	if status := readStatus(t, c, id); status.Active || status.Attempt != nil || len(status.Available) != 0 {
		t.Fatalf("missing files fabricated queued work: %#v", status)
	}
	source, err := model.EnsureMediaSource(id, 11)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := model.ReserveMediaAttempt(source.ID, "uncommitted", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if status := readStatus(t, c, id); status.Active || status.Attempt != nil {
		t.Fatalf("uncommitted intent was shown as accepted work: %#v", status)
	}
	if _, err := model.AcceptReservedMediaAttempt(pending.ID); err != nil {
		t.Fatal(err)
	}
	if status := readStatus(t, c, id); !status.Active || status.Attempt.State != model.MediaQueued || len(status.Available) != 0 {
		t.Fatalf("queued state = %#v", status)
	}
	claimed, err := model.ClaimMediaAttempt(pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status := readStatus(t, c, id); status.Attempt.State != model.MediaStarting || status.Attempt.Video != model.MediaQueued {
		t.Fatalf("claim was falsely downloading: %#v", status)
	}
	if err := model.StartMediaMode(claimed.ID, claimed.ClaimToken, model.MediaVideo); err != nil {
		t.Fatal(err)
	}
	if status := readStatus(t, c, id); status.Attempt.Video != model.MediaDownloading || len(status.Available) != 0 {
		t.Fatalf("downloading state = %#v", status)
	}
	if err := model.CompleteMediaMode(claimed.ID, claimed.ClaimToken, model.MediaVideo); err != nil {
		t.Fatal(err)
	}
	if status := readStatus(t, c, id); status.Attempt.Video != model.MediaDownloaded || status.Attempt.Audio != model.MediaQueued || len(status.Available) != 0 {
		t.Fatalf("completed video was prematurely available: %#v", status)
	}
	if err := model.StartMediaMode(claimed.ID, claimed.ClaimToken, model.MediaAudio); err != nil {
		t.Fatal(err)
	}
	if err := model.CompleteMediaMode(claimed.ID, claimed.ClaimToken, model.MediaAudio); err != nil {
		t.Fatal(err)
	}
	if status := readStatus(t, c, id); status.Attempt.State != model.MediaFinalizing || len(status.Available) != 0 {
		t.Fatalf("finalizing state = %#v", status)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var pair []model.MediaArtifactInput
	for _, mode := range []struct{ role, contentType string }{{model.MediaVideo, "video/webm"}, {model.MediaAudio, "audio/mpeg"}} {
		testutil.WriteFile(t, root, mode.role, []byte("media bytes"))
		pair = append(pair, model.MediaArtifactInput{Role: mode.role, JobID: claimed.ID + "-" + mode.role, StorageRoot: root, RelativePath: mode.role, Size: 11, ContentType: mode.contentType})
	}
	if _, err := model.PublishMediaPair(claimed.ID, claimed.ClaimToken, pair); err != nil {
		t.Fatal(err)
	}
	status := readStatus(t, c, id)
	if status.Active || status.Attempt.State != model.MediaReady || len(status.Available) != 2 {
		t.Fatalf("committed state = %#v", status)
	}
	for _, artifact := range status.Available {
		if artifact.State != model.MediaReady {
			t.Fatalf("available state = %#v", artifact)
		}
	}

	previous, err := extractor.NewRegistry(extractor.DefaultRegistry().Extractors()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { extractor.DefaultRegistry().Replace(previous) })
	e := &yt.YTDownloader{}
	if err := e.SetConfig(&config.Extractor{Enable: true}); err != nil {
		t.Fatal(err)
	}
	registry, err := extractor.NewRegistry(e)
	if err != nil {
		t.Fatal(err)
	}
	extractor.DefaultRegistry().Replace(registry)
	for _, name := range []string{config.YTDownloaderName, config.LegacyYTDownloaderName, "YT_VIDEO_DOWNLOADER"} {
		recorder := httptest.NewRecorder()
		c.Request, c.Response = httptest.NewRequest(http.MethodGet, "/api/preview?"+url.Values{"url": {sourceURL}, "document_id": {id}, "extractor": {name}}.Encode(), nil), recorder
		servePreview(c)
		var preview struct{ Content, Template string }
		if err := json.Unmarshal(recorder.Body.Bytes(), &preview); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusOK || preview.Template != config.YTDownloaderName || strings.Contains(preview.Content, root) || strings.Contains(preview.Content, "not-ready") {
			t.Fatalf("%s preview did not return safe canonical status without HTML: %d %#v", name, recorder.Code, preview)
		}
	}

	refresh, err := model.AcceptMediaAttempt(source.ID, "refresh", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	status = readStatus(t, c, id)
	if !status.Active || status.Attempt.ID != refresh.ID || len(status.Available) != 2 {
		t.Fatalf("queued refresh hid older available files: %#v", status)
	}
	worker, err := model.ClaimMediaAttempt(refresh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.FailMediaAttempt(refresh.ID, worker.ClaimToken, "download_failed"); err != nil {
		t.Fatal(err)
	}
	status = readStatus(t, c, id)
	if status.Active || status.Attempt.State != model.MediaFailed || status.Attempt.ErrorCode != "download_failed" || len(status.Available) != 2 {
		t.Fatalf("failed refresh hid older files: %#v", status)
	}
	if err := os.Remove(filepath.Join(root, "video")); err != nil {
		t.Fatal(err)
	}
	artifacts, err := model.CurrentMediaArtifacts(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range artifacts {
		if artifact.Role == model.MediaAudio {
			if err := model.DB.Model(&artifact).Update("retired_at", time.Now().UTC()).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	status = readStatus(t, c, id)
	for _, artifact := range status.Available {
		if (artifact.Role == model.MediaVideo && artifact.State != "missing") || (artifact.Role == model.MediaAudio && artifact.State != "retired") {
			t.Fatalf("unavailable file was misrepresented: %#v", artifact)
		}
	}
	var count int64
	if err := model.DB.Model(&model.MediaAttempt{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("status reads scheduled new work: count=%d, error=%v", count, err)
	}
}

func TestMediaStatusIsOwnerScopedAndUnavailableOnSuspendedMove(t *testing.T) {
	c := statusContext(t)
	const sourceURL = "https://www.youtube.com/watch?v=abcdefghijk"
	id := document.GetDocID(22, sourceURL)
	if err := c.Indexer.Add(&document.Document{URL: sourceURL, UserID: 22, Text: "private", Processed: true}); err != nil {
		t.Fatal(err)
	}
	source, err := model.EnsureMediaSource(id, 22)
	if err != nil {
		t.Fatal(err)
	}
	for _, requestedID := range []string{id, "unknown"} {
		recorder := httptest.NewRecorder()
		c.Request, c.Response = httptest.NewRequest(http.MethodGet, "/api/yt_downloader/status?document_id="+url.QueryEscape(requestedID), nil), recorder
		serveMediaStatus(c)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status leaked another user's source: %d", recorder.Code)
		}
	}
	c.UserID = 22
	if err := model.SuspendMediaTransfer(source.ID, document.GetDocID(33, sourceURL), 33); err != nil {
		t.Fatal(err)
	}
	status := readStatus(t, c, id)
	if status.Unavailable != "ownership_change_incomplete" || status.Active || len(status.Available) != 0 || status.Attempt != nil {
		t.Fatalf("suspended source appeared ready/queued: %#v", status)
	}
}
