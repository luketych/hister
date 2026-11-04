// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/server/document"
	"github.com/asciimoo/hister/server/extractor"
	yt "github.com/asciimoo/hister/server/extractor/extractors/yt_downloader"
	"github.com/asciimoo/hister/server/extractor/sdk"
	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"
	"gorm.io/gorm"
)

type onDemandContentExtractor struct{}

func (*onDemandContentExtractor) Name() string        { return "on-demand-content" }
func (*onDemandContentExtractor) Description() string { return "synthetic indexed content" }
func (*onDemandContentExtractor) Capabilities() sdk.Capabilities {
	return sdk.Capabilities{Extract: true}
}
func (*onDemandContentExtractor) Match(*sdk.Document) bool { return true }
func (*onDemandContentExtractor) Extract(d *sdk.Document) sdk.ExtractResult {
	d.Text = "indexed locally"
	return sdk.Extracted()
}
func (*onDemandContentExtractor) Preview(*sdk.Document) sdk.PreviewResult {
	return sdk.PreviewFallback(nil)
}
func (*onDemandContentExtractor) GetConfig() *sdk.Config      { return &sdk.Config{Enable: true} }
func (*onDemandContentExtractor) SetConfig(*sdk.Config) error { return nil }

func TestOnDemandMediaEndToEndThroughAuthenticatedCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("synthetic downloader fixture uses a POSIX shell")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	cfg := testutil.Config(t)
	cfg.App.UserHandling, cfg.App.Public = true, true
	cfg.Server.Database = "on-demand-media.sqlite3"
	cfg.Indexer.DetectLanguages = false
	if err := cfg.UpdateBaseURL("http://127.0.0.1:4433"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveRules(); err != nil {
		t.Fatal(err)
	}

	producerLog := filepath.Join(t.TempDir(), "producer-invocations.log")
	outputRoot := t.TempDir()
	script := `command=$1
requested_url=$2
printf '%s\n' "$command" >> "$HISTER_TEST_INVOCATIONS"
shift 2
while [ "$#" -gt 0 ]; do
 case "$1" in
 --job-id) job=$2; shift 2;;
 --output-root) root=$2; shift 2;;
 --result-json) result=$2; shift 2;;
 --artifacts-root|--backend) shift 2;;
 *) shift;;
 esac
done
kind=scrape_videos
[ "$command" = audio ] && kind=scrape_audio_only
mkdir -p "$root/$job"
file="$root/$job/$command.webm"
printf 'synthetic %s payload\n' "$command" > "$file"
printf '{"schema_version":1,"job_id":"%s","job_type":"%s","url":"%s","video_id":"abcdefghijk","output_dir":"%s/%s","status":"completed","media_paths":["%s"],"transcript_paths":[],"error":null}\n' "$job" "$kind" "$requested_url" "$root" "$job" "$file" > "$result"
`
	binary := testutil.WriteFile(t, t.TempDir(), "synthetic-downloader", []byte("#!/bin/sh\n"+script))
	if err := os.Chmod(binary, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HISTER_TEST_INVOCATIONS", producerLog)
	cfg.Extractors = map[string]*config.Extractor{
		config.YTDownloaderName: {Enable: true, Options: map[string]any{
			"binary": binary, "output_dir": outputRoot, "timeout": 30,
		}},
		"on-demand-content": {Enable: true},
	}

	testutil.InitModelWithConfig(t, cfg)
	previousSessionStore := sessionStore
	sessionStore = newSessionStore([]byte(strings.Repeat("x", 32)), cfg.BaseURL(""), sessionMaxAge)
	t.Cleanup(func() { sessionStore = previousSessionStore })
	owner := testutil.CreateUser(t, "media-owner")
	previous, err := extractor.NewRegistry(extractor.DefaultRegistry().Extractors()...)
	if err != nil {
		t.Fatal(err)
	}
	downloader := &yt.YTDownloader{}
	if err := downloader.SetConfig(cfg.Extractors[config.YTDownloaderName]); err != nil {
		t.Fatal(err)
	}
	registry, err := extractor.NewRegistry(downloader, &onDemandContentExtractor{})
	if err != nil {
		t.Fatal(err)
	}
	extractor.DefaultRegistry().Replace(registry)
	t.Cleanup(func() { extractor.DefaultRegistry().Replace(previous) })

	idx := newServerTestIndexer(t, cfg)
	defer idx.Close()
	const sourceURL = "https://www.youtube.com/watch?v=abcdefghijk"
	docID := document.GetDocID(owner.ID, sourceURL)
	if err := idx.Add(&document.Document{URL: sourceURL, UserID: owner.ID, Title: "Synthetic video", HTML: "<p>indexed locally</p>"}); err != nil {
		t.Fatal(err)
	}
	stored := idx.GetByDocID(docID)
	if stored == nil {
		t.Fatal("indexing did not preserve the YouTube document")
	}
	if _, err := model.GetMediaSource(docID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("indexing eagerly created a media source: %v", err)
	}
	var attempts int64
	if err := model.DB.Model(&model.MediaAttempt{}).Count(&attempts).Error; err != nil || attempts != 0 {
		t.Fatalf("indexing created attempts: %d, %v", attempts, err)
	}
	if _, err := os.Stat(producerLog); !os.IsNotExist(err) {
		t.Fatalf("indexing invoked producer (log stat error %v)", err)
	}

	fixture := &mediaCommandFixture{cfg: cfg, idx: idx, handler: registerEndpoints(cfg, idx), owner: owner, documentID: docID}
	loopback := httptest.NewServer(fixture.handler)
	defer loopback.Close()
	body, err := json.Marshal(map[string]string{"document_id": docID})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, loopback.URL+"/api/yt_downloader/start", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "hister://")
	request.Header.Set("X-Access-Token", owner.Token)
	response, err := loopback.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var accepted mediaCommandResponse
	decodeErr := json.NewDecoder(response.Body).Decode(&accepted)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusAccepted || accepted.AttemptID == "" || accepted.State != model.MediaQueued || accepted.Replayed {
		t.Fatalf("start response = status %d, body %#v, decode error %v", response.StatusCode, accepted, decodeErr)
	}
	waitCommandAttempt(t, accepted.AttemptID, model.MediaReady)

	var attempt model.MediaAttempt
	if err := model.DB.First(&attempt, "id = ?", accepted.AttemptID).Error; err != nil {
		t.Fatal(err)
	}
	if attempt.VideoJobID == nil || attempt.AudioJobID == nil || *attempt.VideoJobID == *attempt.AudioJobID {
		t.Fatalf("video/audio producer jobs were not both published: %#v", attempt)
	}
	artifacts, err := model.CurrentMediaArtifacts(attempt.SourceID)
	if err != nil || len(artifacts) != 2 {
		t.Fatalf("published artifacts = %#v, error %v", artifacts, err)
	}
	roles := map[string]bool{}
	source, err := model.GetMediaSource(docID)
	if err != nil || source.UserID != owner.ID {
		t.Fatalf("media source is not bound to existing document: %#v, %v", source, err)
	}
	for _, artifact := range artifacts {
		roles[artifact.Role] = true
		content, err := os.ReadFile(filepath.Join(artifact.StorageRoot, artifact.RelativePath))
		if err != nil || !strings.HasPrefix(string(content), "synthetic ") {
			t.Fatalf("artifact bytes = %q, error %v", content, err)
		}
	}
	if !roles[model.MediaVideo] || !roles[model.MediaAudio] {
		t.Fatalf("missing published video/audio roles: %#v", roles)
	}
	if got := idx.GetByDocID(docID); got == nil || got.Title != "Synthetic video" || !strings.Contains(got.Text, "indexed locally") {
		t.Fatalf("media command changed or removed indexed document: %#v", got)
	}

	request, err = http.NewRequest(http.MethodPost, loopback.URL+"/api/yt_downloader/start", strings.NewReader(fmt.Sprintf(`{"document_id":%q}`, docID)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "hister://")
	request.Header.Set("X-Access-Token", owner.Token)
	response, err = loopback.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var replay mediaCommandResponse
	decodeErr = json.NewDecoder(response.Body).Decode(&replay)
	response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusOK || !replay.Replayed || replay.AttemptID != accepted.AttemptID || replay.State != model.MediaReady {
		t.Fatalf("duplicate start = status %d, body %#v, decode error %v", response.StatusCode, replay, decodeErr)
	}
	if err := model.DB.Model(&model.MediaAttempt{}).Count(&attempts).Error; err != nil || attempts != 1 {
		t.Fatalf("duplicate start created media attempts: %d, %v", attempts, err)
	}
	invocations, err := os.ReadFile(producerLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(invocations))
	if len(lines) != 2 || strings.Count(string(invocations), "download") != 1 || strings.Count(string(invocations), "audio") != 1 {
		t.Fatalf("producer invocation log = %q, want one video-download and one audio invocation", invocations)
	}

}
