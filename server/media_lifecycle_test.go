// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/files"
	"github.com/asciimoo/hister/server/document"
	"github.com/asciimoo/hister/server/indexer"
	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"
)

func fixtureVideo(t *testing.T, artifacts []model.MediaArtifact) model.MediaArtifact {
	t.Helper()
	for _, artifact := range artifacts {
		if artifact.Role == model.MediaVideo {
			return artifact
		}
	}
	t.Fatal("fixture has no video")
	return model.MediaArtifact{}
}

func artifactURL(artifact model.MediaArtifact) string {
	return "/api/yt_downloader/media?id=" + url.QueryEscape(artifact.ID)
}

func legacyArtifactURL(documentID string, artifact model.MediaArtifact) string {
	return "/api/yt_video_downloader/media?" + url.Values{"document_id": {documentID}, "uri": {files.PathToFileURL(filepath.Join(artifact.StorageRoot, artifact.RelativePath))}}.Encode()
}

func TestMediaHistoricalLinksSurviveUpdatesAndRestartButNotDeletion(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.App.Public = true
	cfg.Server.Database = "media.sqlite3"
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
	t.Cleanup(idx.Close)
	handler := registerEndpoints(cfg, idx)
	const sourceURL = "https://www.youtube.com/watch?v=abcdefghijk"
	if err := idx.Add(&document.Document{URL: sourceURL, Text: "original", Processed: true}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	firstPair := testutil.SeedMediaPair(t, sourceURL, 0,
		testutil.WriteFile(t, root, "first.webm", []byte("first video")), testutil.WriteFile(t, root, "first.mp3", []byte("first audio")))
	first := fixtureVideo(t, firstPair)
	if err := idx.Add(&document.Document{URL: sourceURL, Text: "fresh update", Processed: true}); err != nil {
		t.Fatal(err)
	}
	secondPair := testutil.SeedMediaPair(t, sourceURL, 0,
		testutil.WriteFile(t, root, "second.webm", []byte("second video")), testutil.WriteFile(t, root, "second.mp3", []byte("second audio")))
	checkLinks := func() {
		t.Helper()
		for _, fixture := range []struct {
			artifacts []model.MediaArtifact
			label     string
		}{{firstPair, "first"}, {secondPair, "second"}} {
			for _, artifact := range fixture.artifacts {
				for _, path := range []string{artifactURL(artifact), legacyArtifactURL(sourceURL, artifact)} {
					recorder := httptest.NewRecorder()
					handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
					if recorder.Code != http.StatusOK || recorder.Body.String() != fixture.label+" "+artifact.Role {
						t.Fatalf("retained link %s = %d %q", path, recorder.Code, recorder.Body.String())
					}
					if recorder.Header().Get("Content-Type") != artifact.ContentType || recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
						t.Fatalf("unsafe media headers: %#v", recorder.Header())
					}
				}
			}
		}
	}
	checkLinks()
	if err := idx.Reindex(&config.Rules{}, false, cfg.Indexer.DetectLanguages, cfg.Indexer.KeepStopwords, cfg.Indexer.Directories); err != nil {
		t.Fatal(err)
	}
	checkLinks()
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		request := httptest.NewRequest(method, artifactURL(first), nil)
		if method == http.MethodGet {
			request.Header.Set("Range", "bytes=0-4")
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if method == http.MethodGet {
			if recorder.Code != http.StatusPartialContent || recorder.Body.String() != "first" || recorder.Header().Get("Content-Range") != "bytes 0-4/11" {
				t.Fatalf("range response = %d %#v %q", recorder.Code, recorder.Header(), recorder.Body.String())
			}
		} else if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 || recorder.Header().Get("Content-Length") != "11" {
			t.Fatalf("HEAD response = %d %#v %q", recorder.Code, recorder.Header(), recorder.Body.String())
		}
	}
	idx.Close()
	db, err := model.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	testutil.InitModelWithConfig(t, cfg)
	idx, err = indexer.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(idx.Close)
	handler = registerEndpoints(cfg, idx)
	checkLinks()
	if err := idx.Delete(sourceURL); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add(&document.Document{URL: sourceURL, Text: "re-added", Processed: true}); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range slices.Concat(firstPair, secondPair) {
		for _, path := range []string{artifactURL(artifact), legacyArtifactURL(sourceURL, artifact)} {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("delete/re-add revived retained link: %d", recorder.Code)
			}
		}
		if _, err := os.Stat(filepath.Join(artifact.StorageRoot, artifact.RelativePath)); err != nil {
			t.Fatalf("deletion removed retained bytes: %v", err)
		}
	}
}

func TestMediaServingRejectsMissingOrSymlinkReplacements(t *testing.T) {
	cfg := testutil.Config(t)
	testutil.InitModelWithConfig(t, cfg)
	idx, err := indexer.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(idx.Close)
	const sourceURL = "https://www.youtube.com/watch?v=abcdefghijk"
	if err := idx.Add(&document.Document{URL: sourceURL, Text: "source", Processed: true}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	artifact := fixtureVideo(t, testutil.SeedMediaPair(t, sourceURL, 0,
		testutil.WriteFile(t, root, "video.webm", []byte("own bytes")), testutil.WriteFile(t, root, "audio.mp3", []byte("own audio"))))
	path := filepath.Join(artifact.StorageRoot, artifact.RelativePath)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	foreign := testutil.WriteFile(t, artifact.StorageRoot, "foreign.webm", []byte("not yours"))
	for _, replacement := range []string{"missing", "symlink"} {
		if replacement == "symlink" {
			if err := os.Symlink(foreign, path); err != nil {
				t.Fatal(err)
			}
		}
		recorder := httptest.NewRecorder()
		c := webContext{Config: cfg, Indexer: idx, UserID: 0, Authenticated: true, Request: httptest.NewRequest(http.MethodGet, artifactURL(artifact), nil), Response: recorder}
		serveYTDownloaderMedia(&c)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s remained playable: %d %q", replacement, recorder.Code, recorder.Body.String())
		}
	}
}
