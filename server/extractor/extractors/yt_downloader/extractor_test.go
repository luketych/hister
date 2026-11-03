// SPDX-License-Identifier: AGPL-3.0-or-later

package yt_downloader

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/files"
	"github.com/asciimoo/hister/server/document"
	"github.com/asciimoo/hister/server/extractor/sdk"
)

const testVideoID = "abcdefghijk"

func fakeDownloader(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake downloader uses a POSIX shell script; Windows subprocess fixture is not available")
	}
	path := filepath.Join(t.TempDir(), "fake-yt-downloader")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// The fake implements the named-job manifest protocol. Its human output is
// deliberately unrelated to the manifest so tests catch prose-based parsing.
func fakeDownloaderScript(_ string, audioPath string) string {
	audioEnabled := "yes"
	if audioPath == "" {
		audioEnabled = "no"
	}
	return `command=$1
requested_url=$2
shift 2
while [ "$#" -gt 0 ]; do
  case "$1" in
    --job-id) job_id=$2; shift 2 ;;
    --output-root) output_root=$2; shift 2 ;;
    --artifacts-root) artifacts_root=$2; shift 2 ;;
    --result-json) result_json=$2; shift 2 ;;
    --backend|--transcript-langs|--transcript-format) shift 2 ;;
    --video-owner-transcript|--yt-auto-transcript|--no-playlist) shift ;;
    *) shift ;;
  esac
done
case "$requested_url" in
  *v=*) video_id=${requested_url#*v=}; video_id=${video_id%%&*}; video_id=${video_id%%\\#*} ;;
  *youtu.be/*|*/shorts/*|*/live/*|*/embed/*) video_id=${requested_url##*/}; video_id=${video_id%%\\?*}; video_id=${video_id%%\\#*} ;;
  *) video_id=` + testVideoID + ` ;;
esac
case "$command" in
  download) job_type=scrape_videos; file_name=video.bin; enabled=yes ;;
  audio) job_type=scrape_audio_only; file_name=audio.bin; enabled=` + audioEnabled + ` ;;
  *) echo "unknown command" >&2; exit 2 ;;
esac
job_dir="$output_root/$job_id"
mkdir -p "$job_dir" "$artifacts_root"
if [ "$enabled" = yes ]; then
  media_path="$job_dir/$file_name"
  printf '%s fake media bytes' "$video_id" > "$media_path"
  media_paths=$(printf '["%s"]' "$media_path")
else
  media_paths='[]'
fi
printf 'Different and intentionally unstructured human log output.\n'
printf '{"schema_version":1,"job_id":"%s","job_type":"%s","url":"%s","video_id":"%s","output_dir":"%s","status":"completed","media_paths":%s,"transcript_paths":[],"error":null,"future_field":"ignored"}\n' \
  "$job_id" "$job_type" "$requested_url" "$video_id" "$job_dir" "$media_paths" > "$result_json"
`
}

func configuredExtractorWithOutput(t *testing.T, binary, outputDir string) *YTDownloader {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	e := &YTDownloader{}
	if err := e.SetConfig(&config.Extractor{
		Enable: true,
		Options: map[string]any{
			"binary":     binary,
			"output_dir": outputDir,
			"timeout":    30,
			"backend":    "auto",
		},
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

func configuredExtractor(t *testing.T, binary string) *YTDownloader {
	t.Helper()
	return configuredExtractorWithOutput(t, binary, filepath.Join(t.TempDir(), "media"))
}

func TestMatchYouTubeOnlyAndSupportedVideoIDs(t *testing.T) {
	e := &YTDownloader{}
	for _, testCase := range []struct {
		url   string
		match bool
	}{
		{"https://www.youtube.com/watch?v=" + testVideoID, true},
		{"https://youtu.be/" + testVideoID, true},
		{"https://m.youtube.com/shorts/" + testVideoID, true},
		{"https://www.youtube.com/live/" + testVideoID, true},
		{"https://youtube.com/", false},
		{"https://example.com/watch?v=" + testVideoID, false},
		{"file:///tmp/video", false},
	} {
		doc := &document.Document{URL: testCase.url}
		if got := e.Match(doc); got != testCase.match {
			t.Errorf("Match(%q) = %v, want %v", testCase.url, got, testCase.match)
		}
	}
	for rawURL, want := range map[string]string{
		"https://www.youtube.com/watch?v=" + testVideoID:    testVideoID,
		"https://youtu.be/" + testVideoID:                   testVideoID,
		"https://www.youtube.com/shorts/" + testVideoID:     testVideoID,
		"https://www.youtube.com/embed/" + testVideoID:      testVideoID,
		"https://youtube-nocookie.com/embed/" + testVideoID: testVideoID,
	} {
		if got, err := youtubeVideoID(rawURL); err != nil || got != want {
			t.Errorf("youtubeVideoID(%q) = %q, %v; want %q", rawURL, got, err, want)
		}
	}
	for _, rawURL := range []string{
		"https://www.youtube.com/watch?v=short",
		"https://www.youtube.com/watch?v=" + testVideoID + "&v=" + testVideoID,
		"https://www.youtube.com/playlist?list=PL123",
		"https://www.youtube.com/@channel",
	} {
		if id, err := youtubeVideoID(rawURL); err == nil {
			t.Errorf("youtubeVideoID(%q) = %q, want unsupported URL", rawURL, id)
		}
	}
}

func TestExtractorIsEnricherAndPreviewer(t *testing.T) {
	e := &YTDownloader{}
	defaults := e.GetConfig()
	if defaults.Options["binary"] != "yt_downloader" {
		t.Fatalf("default binary = %#v, want command name yt_downloader", defaults.Options["binary"])
	}
	if defaults.Options["output_dir"] != "~/Videos/Hister/YouTube" {
		t.Fatalf("default output_dir = %#v, want ~/Videos/Hister/YouTube", defaults.Options["output_dir"])
	}
	if defaults.Enable {
		t.Fatal("custom extractor must be opt-in")
	}
	if defaults.Options["transcribe"] != false {
		t.Fatalf("default transcribe option = %#v, want false", defaults.Options["transcribe"])
	}
	if e.Name() != "yt_downloader" {
		t.Fatalf("extractor name = %q", e.Name())
	}
	capabilities := e.Capabilities()
	if !capabilities.Enrich || capabilities.Extract || !capabilities.Preview {
		t.Fatalf("capabilities = %+v, want enrichment and preview", capabilities)
	}
	if err := e.SetConfig(&config.Extractor{Enable: true, Options: map[string]any{"unknown": true}}); err == nil {
		t.Fatal("unknown option was accepted")
	}
	if err := e.SetConfig(&config.Extractor{Enable: true, Options: map[string]any{"timeout": 0}}); err == nil {
		t.Fatal("invalid timeout was accepted")
	}
	if err := e.SetConfig(&config.Extractor{Enable: true, Options: map[string]any{"transcribe": "yes"}}); err == nil {
		t.Fatal("non-boolean transcribe option was accepted")
	}
}

func TestExtractPreservesContentAndStoresValidatedMedia(t *testing.T) {
	binary := fakeDownloader(t, fakeDownloaderScript("", "audio"))
	outputRoot := filepath.Join(t.TempDir(), "media")
	e := configuredExtractorWithOutput(t, binary, outputRoot)
	doc := &document.Document{
		URL:      "https://www.youtube.com/watch?v=" + testVideoID,
		Title:    "Existing title",
		Text:     "Existing body",
		HTML:     "<p>Existing HTML</p>",
		Metadata: map[string]any{"existing": "metadata"},
	}
	result := e.ExtractContext(context.Background(), doc)
	decision, err := result.Unpack()
	if err != nil || decision != sdk.ExtractorSuccess {
		t.Fatalf("ExtractContext() = %v, %v", decision, err)
	}
	if doc.Title != "Existing title" || doc.Text != "Existing body" || doc.HTML != "<p>Existing HTML</p>" {
		t.Fatalf("download-only extractor changed document content: title=%q text=%q html=%q", doc.Title, doc.Text, doc.HTML)
	}
	if doc.Metadata["existing"] != "metadata" {
		t.Fatalf("existing metadata was changed: %#v", doc.Metadata)
	}
	var references mediaReferences
	if err := json.Unmarshal([]byte(doc.Metadata[MediaReferencesMetadataKey].(string)), &references); err != nil {
		t.Fatalf("decode media metadata: %v", err)
	}
	if len(references.Video) != 1 || len(references.Audio) != 1 {
		t.Fatalf("media references = %+v, want one validated path per mode", references)
	}
	resolvedOutputRoot, err := filepath.EvalSymlinks(outputRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, uri := range append(references.Video, references.Audio...) {
		path := files.FileURLToPath(uri)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			t.Errorf("referenced file %q is invalid: stat=%v info=%v", path, err, info)
		}
		if !strings.HasPrefix(path, resolvedOutputRoot+string(filepath.Separator)) {
			t.Errorf("media path %q is outside output root %q", path, resolvedOutputRoot)
		}
	}
}

func TestStandaloneExtractionCannotGrantPreviewPlayback(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "media")
	binary := fakeDownloader(t, fakeDownloaderScript("", "audio"))
	e := configuredExtractorWithOutput(t, binary, outputDir)
	source := &document.Document{
		URL:        "https://www.youtube.com/watch?v=" + testVideoID,
		Title:      "Lecture",
		DocumentID: "source-id",
		UserID:     42,
		Label:      "watch-later",
	}
	decision, err := e.ExtractContext(context.Background(), source).Unpack()
	if err != nil || decision != sdk.ExtractorSuccess {
		t.Fatalf("ExtractContext() = %v, %v", decision, err)
	}
	if len(source.ExtraDocuments) != 0 {
		t.Fatalf("ExtraDocuments = %#v, want no separate search results", source.ExtraDocuments)
	}
	var references mediaReferences
	if err := json.Unmarshal([]byte(source.Metadata[MediaReferencesMetadataKey].(string)), &references); err != nil {
		t.Fatalf("decode media metadata: %v", err)
	}
	if len(references.Video) != 1 || len(references.Audio) != 1 {
		t.Fatalf("media references = %+v, want one video and audio URI", references)
	}
	preview, decision, err := e.Preview(source).Unpack()
	if err != nil || decision != sdk.ExtractorSuccess {
		t.Fatalf("Preview() = %v, %v", decision, err)
	}
	for _, uri := range append(references.Video, references.Audio...) {
		if strings.Contains(preview.Content, uri) || strings.Contains(preview.Content, "<a ") {
			t.Fatalf("standalone metadata manufactured playback: %q", preview.Content)
		}
	}
}

func TestFailedAudioPreservesExistingMetadataAndContent(t *testing.T) {
	binary := fakeDownloader(t, fakeDownloaderScript("", ""))
	e := configuredExtractor(t, binary)
	previous := `{"video":["file:///previous/video"],"audio":["file:///previous/audio"]}`
	doc := &document.Document{
		URL:      "https://www.youtube.com/watch?v=" + testVideoID,
		Title:    "Keep me",
		Text:     "Keep this searchable text",
		Metadata: map[string]any{MediaReferencesMetadataKey: previous, "other": "keep"},
	}
	decision, err := e.ExtractContext(context.Background(), doc).Unpack()
	if decision != sdk.ExtractorFallback || err == nil || !strings.Contains(err.Error(), "no media paths") {
		t.Fatalf("ExtractContext() = %v, %v; want missing audio manifest media failure", decision, err)
	}
	if doc.Metadata[MediaReferencesMetadataKey] != previous || doc.Metadata["other"] != "keep" {
		t.Fatalf("failed enrichment replaced metadata: %#v", doc.Metadata)
	}
	if doc.Title != "Keep me" || doc.Text != "Keep this searchable text" {
		t.Fatalf("failed downloader changed content: title=%q text=%q", doc.Title, doc.Text)
	}
}

func TestOlderCompletionCannotReplaceNewerAcceptedReferences(t *testing.T) {
	e := &YTDownloader{}
	newDoc := &document.Document{URL: "https://www.youtube.com/watch?v=" + testVideoID, UserID: 9, DocumentID: "stable-doc"}
	oldDoc := &document.Document{URL: newDoc.URL, UserID: newDoc.UserID, DocumentID: newDoc.DocumentID}
	key, olderID, finishOlder := e.beginPublication(oldDoc)
	defer finishOlder()
	_, newerID, finishNewer := e.beginPublication(newDoc)
	defer finishNewer()
	newer := `{"video":["file:///new/video"],"audio":["file:///new/audio"]}`
	if accepted, err := e.publishReferences(newDoc, key, newerID, newer); err != nil || !accepted {
		t.Fatalf("publish newer result = %v, %v", accepted, err)
	}
	older := `{"video":["file:///old/video"],"audio":["file:///old/audio"]}`
	if accepted, err := e.publishReferences(oldDoc, key, olderID, older); err != nil || accepted {
		t.Fatalf("publish stale result = %v, %v; want rejected", accepted, err)
	}
	if newDoc.Metadata[MediaReferencesMetadataKey] != newer || oldDoc.Metadata[MediaReferencesMetadataKey] != newer {
		t.Fatalf("stale completion failed to preserve accepted references: old=%#v new=%#v", oldDoc.Metadata, newDoc.Metadata)
	}
}

func TestConcurrentCompatibleRequestsAreCoalesced(t *testing.T) {
	callsPath := filepath.Join(t.TempDir(), "calls")
	script := "printf '%s\\n' \"$1\" >> " + shellQuote(callsPath) + "; sleep 0.2; " + fakeDownloaderScript("", "audio")
	e := configuredExtractor(t, fakeDownloader(t, script))
	start := make(chan struct{})
	type outcome struct {
		doc      *document.Document
		decision sdk.Decision
		err      error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			doc := &document.Document{URL: "https://www.youtube.com/watch?v=" + testVideoID, UserID: 7, DocumentID: string(rune('a' + index))}
			decision, err := e.ExtractContext(context.Background(), doc).Unpack()
			results <- outcome{doc: doc, decision: decision, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	for result := range results {
		if result.err != nil || result.decision != sdk.ExtractorSuccess {
			t.Fatalf("concurrent extraction = %v, %v", result.decision, result.err)
		}
		var references mediaReferences
		if err := json.Unmarshal([]byte(result.doc.Metadata[MediaReferencesMetadataKey].(string)), &references); err != nil {
			t.Fatal(err)
		}
		if len(references.Video) != 1 || len(references.Audio) != 1 {
			t.Fatalf("coalesced result for doc %q = %+v", result.doc.DocumentID, references)
		}
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(calls), "download"); got != 1 || strings.Count(string(calls), "audio") != 1 {
		t.Fatalf("downloader commands = %q, want one video and one audio invocation", calls)
	}
}

func TestCanceledWaiterDoesNotCancelLeader(t *testing.T) {
	callsPath := filepath.Join(t.TempDir(), "calls")
	script := "printf '%s\\n' \"$1\" >> " + shellQuote(callsPath) + "; sleep 0.25; " + fakeDownloaderScript("", "audio")
	e := configuredExtractor(t, fakeDownloader(t, script))
	options, err := e.downloaderOptions()
	if err != nil {
		t.Fatal(err)
	}
	doc := &document.Document{URL: "https://www.youtube.com/watch?v=" + testVideoID, UserID: 7}
	leaderDone := make(chan struct {
		result *downloadResult
		err    error
		joined bool
	}, 1)
	go func() {
		result, err, joined := e.fetchSingleflight(context.Background(), doc, options)
		leaderDone <- struct {
			result *downloadResult
			err    error
			joined bool
		}{result, err, joined}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(callsPath); err == nil && strings.Contains(string(data), "download") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if data, err := os.ReadFile(callsPath); err != nil || !strings.Contains(string(data), "download") {
		t.Fatalf("leader did not start a downloader process: %q, %v", data, err)
	}
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	cancelWaiter()
	result, err, joined := e.fetchSingleflight(waiterCtx, doc, options)
	if !joined || result != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter = result %v, error %v, joined %v; want independent cancellation", result, err, joined)
	}
	select {
	case leader := <-leaderDone:
		if leader.err != nil || leader.result == nil || leader.joined {
			t.Fatalf("leader result = %+v", leader)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("leader did not finish after waiter canceled")
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "download") != 1 || strings.Count(string(calls), "audio") != 1 {
		t.Fatalf("waiter cancellation changed leader process count: %q", calls)
	}
}

func TestConcurrentDifferentPagesKeepResultsWhenCompletionOrderReverses(t *testing.T) {
	const slowID = "abcdefghijk"
	const fastID = "lmnopqrstuv"
	callsPath := filepath.Join(t.TempDir(), "calls")
	script := "case \"$2\" in *" + slowID + "*) sleep 0.25 ;; esac\n" + fakeDownloaderScript("", "audio") +
		"\nprintf '%s %s\\n' \"$command\" \"$video_id\" >> " + shellQuote(callsPath)
	e := configuredExtractor(t, fakeDownloader(t, script))
	start := make(chan struct{})
	type outcome struct {
		id       string
		doc      *document.Document
		decision sdk.Decision
		err      error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, id := range []string{slowID, fastID} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			doc := &document.Document{URL: "https://www.youtube.com/watch?v=" + id, UserID: 12, DocumentID: id}
			decision, err := e.ExtractContext(context.Background(), doc).Unpack()
			results <- outcome{id: id, doc: doc, decision: decision, err: err}
		}(id)
	}
	close(start)
	first := <-results
	second := <-results
	wg.Wait()
	if first.err != nil || first.decision != sdk.ExtractorSuccess || first.id != fastID {
		t.Fatalf("first completion = %q, %v, %v; want fast page %q", first.id, first.decision, first.err, fastID)
	}
	if second.err != nil || second.decision != sdk.ExtractorSuccess || second.id != slowID {
		t.Fatalf("second completion = %q, %v, %v; want slow page %q", second.id, second.decision, second.err, slowID)
	}
	for _, result := range []outcome{first, second} {
		var references mediaReferences
		if err := json.Unmarshal([]byte(result.doc.Metadata[MediaReferencesMetadataKey].(string)), &references); err != nil {
			t.Fatal(err)
		}
		if len(references.Video) != 1 || len(references.Audio) != 1 {
			t.Fatalf("page %q references = %+v", result.id, references)
		}
		for _, uri := range append(references.Video, references.Audio...) {
			data, err := os.ReadFile(files.FileURLToPath(uri))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), result.id) {
				t.Errorf("page %q references media for another page: %q contains %q", result.id, uri, data)
			}
		}
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "download ") != 2 || strings.Count(string(calls), "audio ") != 2 {
		t.Fatalf("commands = %q, want separate video and audio jobs per page", calls)
	}
}

func TestSingleflightSeparatesDifferentUserScopes(t *testing.T) {
	callsPath := filepath.Join(t.TempDir(), "calls")
	script := "printf '%s\\n' \"$1\" >> " + shellQuote(callsPath) + "; sleep 0.2; " + fakeDownloaderScript("", "audio")
	e := configuredExtractor(t, fakeDownloader(t, script))
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, userID := range []uint{3, 4} {
		wg.Add(1)
		go func(userID uint) {
			defer wg.Done()
			<-start
			doc := &document.Document{URL: "https://youtu.be/" + testVideoID, UserID: userID}
			decision, err := e.ExtractContext(context.Background(), doc).Unpack()
			if err == nil && decision != sdk.ExtractorSuccess {
				err = context.Canceled
			}
			results <- err
		}(userID)
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(calls), "download"); got != 2 || strings.Count(string(calls), "audio") != 2 {
		t.Fatalf("commands across user scopes = %q, want two video and two audio invocations", calls)
	}
}

func TestTranscribeOptionPassesFlagsAndExpandedOutputRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	callsPath := filepath.Join(t.TempDir(), "args")
	script := "printf '%s\\n' \"$*\" >> " + shellQuote(callsPath) + "; " + fakeDownloaderScript("", "audio")
	binary := fakeDownloader(t, script)
	outputDir := "~/downloads"
	e := configuredExtractorWithOutput(t, binary, outputDir)
	cfg := e.GetConfig()
	cfg.Options["transcribe"] = true
	if err := e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	decision, err := e.ExtractContext(context.Background(), &document.Document{URL: "https://www.youtube.com/watch?v=" + testVideoID}).Unpack()
	if err != nil || decision != sdk.ExtractorSuccess {
		t.Fatalf("transcribe-enabled extraction = %v, %v", decision, err)
	}
	args, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--video-owner-transcript") || !strings.Contains(string(args), "--yt-auto-transcript") {
		t.Fatalf("video command args = %q, want transcript flags", args)
	}
	resolvedOutputRoot, err := filepath.EvalSymlinks(files.ExpandHome(outputDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--output-root "+resolvedOutputRoot) {
		t.Fatalf("downloader command args = %q, want canonical expanded --output-root %q", args, resolvedOutputRoot)
	}
	if !strings.Contains(string(args), "--job-id hister-") || !strings.Contains(string(args), "--result-json ") {
		t.Fatalf("downloader args = %q, want named job and result JSON flags", args)
	}
}

func TestDownloaderFailureAndMissingManifestFallback(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		script string
		want   string
	}{
		{name: "nonzero", script: "echo 'downloader detail' >&2; exit 7", want: "downloader detail"},
		{name: "nonzero despite successful-looking report", script: fakeDownloaderScript("", "audio") + "\nexit 7", want: "failed"},
		{name: "missing manifest", script: "echo 'human success text'; exit 0", want: "result manifest"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			e := configuredExtractor(t, fakeDownloader(t, testCase.script))
			doc := &document.Document{URL: "https://www.youtube.com/watch?v=" + testVideoID, Title: "Keep me", Text: "Keep this", Metadata: map[string]any{MediaReferencesMetadataKey: "old"}}
			decision, err := e.ExtractContext(context.Background(), doc).Unpack()
			if decision != sdk.ExtractorFallback || err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("ExtractContext() = %v, %v; want fallback mentioning %q", decision, err, testCase.want)
			}
			if !strings.Contains(err.Error(), "upgrade tools--yt_downloader") {
				t.Errorf("error %q lacks actionable producer upgrade guidance", err)
			}
			if doc.Metadata[MediaReferencesMetadataKey] != "old" || doc.Title != "Keep me" || doc.Text != "Keep this" {
				t.Fatalf("failed extraction changed document: %+v", doc)
			}
		})
	}
}

func TestValidateResultManifestCorrelationAndPaths(t *testing.T) {
	invocation, mediaPath := testInvocation(t)
	valid := manifestFor(t, invocation, []string{mediaPath}, nil)
	manifest, mediaPaths, _, err := validateResultManifest(valid, invocation)
	if err != nil {
		t.Fatalf("validate valid manifest: %v", err)
	}
	if manifest.JobID != invocation.jobID || !reflect.DeepEqual(mediaPaths, []string{mediaPath}) {
		t.Fatalf("validated result = %+v paths=%v", manifest, mediaPaths)
	}
	duplicateManifest := manifestFor(t, invocation, []string{mediaPath, mediaPath}, nil)
	_, deduplicated, _, err := validateResultManifest(duplicateManifest, invocation)
	if err != nil || !reflect.DeepEqual(deduplicated, []string{mediaPath}) {
		t.Fatalf("duplicate media paths = %v, %v; want one canonical path", deduplicated, err)
	}

	for _, testCase := range []struct {
		name    string
		changes map[string]any
	}{
		{name: "wrong job", changes: map[string]any{"job_id": "other-job"}},
		{name: "wrong URL", changes: map[string]any{"url": "https://www.youtube.com/watch?v=zzzzzzzzzzz"}},
		{name: "wrong video", changes: map[string]any{"video_id": "zzzzzzzzzzz"}},
		{name: "wrong mode", changes: map[string]any{"job_type": "scrape_audio_only"}},
		{name: "wrong output directory", changes: map[string]any{"output_dir": filepath.Join(invocation.outputRoot, "another-job")}},
		{name: "unknown schema", changes: map[string]any{"schema_version": 2}},
		{name: "missing media", changes: map[string]any{"media_paths": []string{}}},
		{name: "null transcripts", changes: map[string]any{"transcript_paths": nil}},
		{name: "failed status", changes: map[string]any{"status": "failed", "error": map[string]string{"code": "unavailable", "message": "not available"}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			data := manifestFor(t, invocation, []string{mediaPath}, testCase.changes)
			if _, _, _, err := validateResultManifest(data, invocation); err == nil {
				t.Fatal("invalid result manifest was accepted")
			}
		})
	}
}

func TestValidateResultManifestRejectsForeignAndUnsafePaths(t *testing.T) {
	invocation, mediaPath := testInvocation(t)
	foreignDir := filepath.Join(invocation.outputRoot, "another-job")
	if err := os.MkdirAll(foreignDir, 0o755); err != nil {
		t.Fatal(err)
	}
	foreignPath := filepath.Join(foreignDir, "foreign.webm")
	if err := os.WriteFile(foreignPath, []byte("foreign bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "escape.webm")
	if err := os.WriteFile(outsidePath, []byte("outside bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(invocation.expectedOutputDir, "escape-link.webm")
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	emptyPath := filepath.Join(invocation.expectedOutputDir, "empty.webm")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, rawPath := range []string{
		foreignPath,
		outsidePath,
		linkPath,
		filepath.Join(invocation.expectedOutputDir, "missing.webm"),
		emptyPath,
		invocation.expectedOutputDir,
	} {
		data := manifestFor(t, invocation, []string{rawPath}, nil)
		if _, _, _, err := validateResultManifest(data, invocation); err == nil {
			t.Errorf("unsafe path %q was accepted", rawPath)
		}
	}
	_ = mediaPath
}

func TestValidateResultManifestRejectsMalformedDuplicateAndMissingFields(t *testing.T) {
	invocation, mediaPath := testInvocation(t)
	valid := manifestFor(t, invocation, []string{mediaPath}, nil)
	validObject := make(map[string]any)
	if err := json.Unmarshal(valid, &validObject); err != nil {
		t.Fatal(err)
	}
	delete(validObject, "error")
	missing, err := json.Marshal(validObject)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"truncated":       []byte(`{"schema_version":1`),
		"missing field":   missing,
		"duplicate key":   []byte(strings.Replace(string(valid), `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1)),
		"multiple values": append(append(append([]byte{}, valid...), '\n'), valid...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := validateResultManifest(data, invocation); err == nil {
				t.Fatal("malformed manifest was accepted")
			}
		})
	}
}

func TestReadManifestRejectsOversizedAndNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	large := filepath.Join(dir, "large.json")
	if err := os.WriteFile(large, make([]byte, maxManifestSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readResultManifest(large); err == nil {
		t.Fatal("oversized manifest was accepted")
	}
	if _, err := readResultManifest(dir); err == nil {
		t.Fatal("directory result was accepted")
	}
}

func TestExtractorFallsBackForUnsupportedVideoPage(t *testing.T) {
	e := configuredExtractor(t, fakeDownloader(t, "echo should-not-run >&2; exit 23"))
	decision, err := e.ExtractContext(context.Background(), &document.Document{URL: "https://www.youtube.com/playlist?list=PL123"}).Unpack()
	if decision != sdk.ExtractorFallback || err == nil || !strings.Contains(err.Error(), "does not identify a supported video") {
		t.Fatalf("unsupported page extraction = %v, %v", decision, err)
	}
}

func TestDisabledExtractorFallsBackWithoutLaunchingDownloader(t *testing.T) {
	binary := fakeDownloader(t, "echo should-not-run >&2; exit 23")
	e := &YTDownloader{}
	if err := e.SetConfig(&config.Extractor{Enable: false, Options: map[string]any{"binary": binary}}); err != nil {
		t.Fatal(err)
	}
	decision, err := e.ExtractContext(context.Background(), &document.Document{URL: "https://www.youtube.com/watch?v=" + testVideoID}).Unpack()
	if decision != sdk.ExtractorFallback || err != nil {
		t.Fatalf("disabled extraction = %v, %v", decision, err)
	}
}

func testInvocation(t *testing.T) (*downloaderInvocation, string) {
	t.Helper()
	root := t.TempDir()
	var err error
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	jobID := "hister-test-video"
	jobDir := filepath.Join(root, jobID)
	if err := os.Mkdir(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mediaPath := filepath.Join(jobDir, "media.bin")
	if err := os.WriteFile(mediaPath, []byte("non-empty media"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &downloaderInvocation{
		jobID:             jobID,
		sourceDocumentID:  "doc-id",
		userID:            11,
		requestedURL:      "https://www.youtube.com/watch?v=" + testVideoID,
		expectedVideoID:   testVideoID,
		jobType:           "scrape_videos",
		outputRoot:        root,
		expectedOutputDir: jobDir,
		context:           context.Background(),
	}, mediaPath
}

func manifestFor(t *testing.T, invocation *downloaderInvocation, mediaPaths []string, changes map[string]any) []byte {
	t.Helper()
	value := map[string]any{
		"schema_version":   1,
		"job_id":           invocation.jobID,
		"job_type":         invocation.jobType,
		"url":              invocation.requestedURL,
		"video_id":         invocation.expectedVideoID,
		"output_dir":       invocation.expectedOutputDir,
		"status":           "completed",
		"media_paths":      mediaPaths,
		"transcript_paths": []string{},
		"error":            nil,
		"future_field":     map[string]any{"additive": true},
	}
	for key, replacement := range changes {
		value[key] = replacement
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
