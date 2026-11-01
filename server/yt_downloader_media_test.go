// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/files"
	"github.com/asciimoo/hister/server/document"
	"github.com/asciimoo/hister/server/indexer"
	"github.com/asciimoo/hister/server/testutil"
)

func TestServeYTDownloaderMediaStreamsRegisteredFile(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.App.Public = true
	cfg.Server.Database = "file::memory:"
	outputDir := t.TempDir()
	mediaPath := testutil.WriteFile(t, outputDir, "clip.webm", []byte("video bytes"))
	cfg.Extractors = map[string]*config.Extractor{
		"yt_video_downloader": {Enable: true, Options: map[string]any{"output_dir": outputDir}},
	}
	if err := cfg.UpdateBaseURL("http://127.0.0.1:4433"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveRules(); err != nil {
		t.Fatal(err)
	}
	testutil.InitModelWithConfig(t, cfg)
	idx, err := indexer.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	handler := registerEndpoints(cfg, idx)

	sourceURL := "https://www.youtube.com/watch?v=abcdefghijk"
	if err := idx.Add(&document.Document{
		URL:       sourceURL,
		HTML:      "<html><body>video</body></html>",
		Title:     "YouTube source",
		Processed: true,
	}); err != nil {
		t.Fatal(err)
	}

	id := document.GetDocID(0, sourceURL)
	audioPath := testutil.WriteFile(t, outputDir, "clip.mp3", []byte("audio bytes"))
	artifacts := testutil.SeedMediaPair(t, id, 0, mediaPath, audioPath)
	for _, artifact := range artifacts {
		if artifact.Role == "video" {
			mediaPath = filepath.Join(artifact.StorageRoot, artifact.RelativePath)
		}
	}
	requestURL := "/api/yt_video_downloader/media?document_id=" + url.QueryEscape(id) + "&uri=" + url.QueryEscape(files.PathToFileURL(mediaPath))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, requestURL, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET media = %d %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != "video bytes" {
		t.Fatalf("media body = %q, want original bytes", got)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "video/webm") {
		t.Errorf("Content-Type = %q, want video/webm", got)
	}
	if got := recorder.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline;") {
		t.Errorf("Content-Disposition = %q, want inline", got)
	}
	if got := recorder.Header().Get("Content-Security-Policy"); got != "default-src 'none'; media-src 'self'" {
		t.Errorf("Content-Security-Policy = %q, want a policy that permits same-origin media without sandboxing", got)
	}

	outsideURI := files.PathToFileURL(filepath.Join(t.TempDir(), "outside.webm"))
	badURL := "/api/yt_video_downloader/media?document_id=" + url.QueryEscape(id) + "&uri=" + url.QueryEscape(outsideURI)
	badRecorder := httptest.NewRecorder()
	handler.ServeHTTP(badRecorder, httptest.NewRequest(http.MethodGet, badURL, nil))
	if badRecorder.Code != http.StatusNotFound {
		t.Errorf("GET unassociated media = %d, want 404", badRecorder.Code)
	}
}
