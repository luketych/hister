// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/files"
	"github.com/asciimoo/hister/server/document"
	"github.com/asciimoo/hister/server/indexer"
	"github.com/asciimoo/hister/server/testutil"
)

// Permanent full-request regression for the independent review's P1 finding.
func TestSubmittedMetadataCannotAuthorizeForeignMedia(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.App.UserHandling = true
	cfg.Server.Database = "media-security.sqlite3"
	output := t.TempDir()
	cfg.Extractors = map[string]*config.Extractor{"yt_video_downloader": {Enable: true, Options: map[string]any{"output_dir": output}}}
	testutil.InitModelWithConfig(t, cfg)
	idx, err := indexer.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(idx.Close)
	const victimURL = "https://www.youtube.com/watch?v=lmnopqrstuv"
	if err := idx.Add(&document.Document{URL: victimURL, UserID: 22, Text: "victim source", Processed: true}); err != nil {
		t.Fatal(err)
	}
	victimID := document.GetDocID(22, victimURL)
	artifact := fixtureVideo(t, testutil.SeedMediaPair(t, victimID, 22,
		testutil.WriteFile(t, output, "victim.webm", []byte("victim private media")), testutil.WriteFile(t, output, "victim.mp3", []byte("victim audio"))))
	victimURI := files.PathToFileURL(filepath.Join(artifact.StorageRoot, artifact.RelativePath))
	encoded, err := json.Marshal(map[string][]string{"video": {victimURI}, "audio": {}})
	if err != nil {
		t.Fatal(err)
	}
	const sourceURL = "https://www.youtube.com/watch?v=abcdefghijk"
	ownID := document.GetDocID(11, sourceURL)
	c := webContext{Config: cfg, Indexer: idx, UserID: 11, Authenticated: true}
	for _, endpoint := range []string{"/api/add", "/api/batch"} {
		responses := submitTestDocuments(t, c, endpoint, nil, document.Document{
			URL: sourceURL, DocumentID: victimID, UserID: 22, Text: "attacker source", Processed: true,
			Metadata: map[string]any{"yt_video_downloader_media": string(encoded), "yt_downloader_media": string(encoded), "unrelated": "retained"},
		})
		if responses[0].Status != http.StatusCreated {
			t.Fatalf("setup submission failed: %+v", responses)
		}
		stored := idx.GetByDocID(ownID)
		if stored == nil || stored.UserID != 11 || stored.Metadata["yt_video_downloader_media"] != nil || stored.Metadata["yt_downloader_media"] != nil || stored.Metadata["unrelated"] != "retained" {
			t.Fatalf("untrusted authorization survived %s: %#v", endpoint, stored)
		}
		for _, path := range []string{
			"/api/yt_video_downloader/media?document_id=" + url.QueryEscape(ownID) + "&uri=" + url.QueryEscape(victimURI),
			artifactURL(artifact), legacyArtifactURL(victimID, artifact),
		} {
			recorder := httptest.NewRecorder()
			c.Request, c.Response = httptest.NewRequest(http.MethodGet, path, nil), recorder
			serveYTDownloaderMedia(&c)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("user 11 read user 22's media after %s: status=%d body=%q", endpoint, recorder.Code, recorder.Body.String())
			}
		}
	}
	c.UserID = 22
	for _, path := range []string{artifactURL(artifact), legacyArtifactURL(victimID, artifact)} {
		recorder := httptest.NewRecorder()
		c.Request, c.Response = httptest.NewRequest(http.MethodGet, path, nil), recorder
		serveYTDownloaderMedia(&c)
		if recorder.Code != http.StatusOK || recorder.Body.String() != "victim private media" {
			t.Fatalf("authorized owner lost access: %d %q", recorder.Code, recorder.Body.String())
		}
	}
	c.Config = testutil.Config(t)
	recorder := httptest.NewRecorder()
	c.Request, c.Response = httptest.NewRequest(http.MethodGet, artifactURL(artifact), nil), recorder
	serveYTDownloaderMedia(&c)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("another configuration borrowed the global registry: %d", recorder.Code)
	}
}
