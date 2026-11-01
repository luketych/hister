// SPDX-License-Identifier: AGPL-3.0-or-later
//go:build localmedia && (darwin || linux)

package yt_downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/files"
	"github.com/asciimoo/hister/server/document"
	"github.com/asciimoo/hister/server/extractor/sdk"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"
)

const (
	localFixtureVideoA = "abcdefghijk"
	localFixtureVideoB = "lmnopqrstuv"
)

var localJobIDPattern = regexp.MustCompile(`^hister-[0-9a-f]{32}-(video|audio)$`)

// This is an end-to-end consumer test, not a fake-protocol test. It launches
// the exact candidate yt_downloader executable and its installed yt-dlp
// backend. A temporary yt-dlp extractor plugin maps supported canonical
// YouTube URLs to loopback MP4 fixtures; the producer still creates, publishes,
// and validates every result manifest itself. Nothing in production URL or
// filesystem validation is bypassed.
func TestLocalMediaCandidateEndToEnd(t *testing.T) {
	binary := strings.TrimSpace(os.Getenv("HISTER_YT_LOCAL_BINARY"))
	if binary == "" {
		t.Skip("set HISTER_YT_LOCAL_BINARY to the candidate yt_downloader executable")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		t.Fatalf("candidate downloader %q is unavailable or not executable: %v", binary, err)
	}
	if ffmpeg, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required to create deterministic loopback media fixtures")
	} else {
		t.Logf("fixture encoder: %s", ffmpeg)
	}
	t.Logf("candidate downloader: %s", binary)
	t.Logf("candidate revision: %s", envOrDefault("HISTER_YT_LOCAL_REVISION", "not supplied"))

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	outputRoot := filepath.Join(root, "media")
	cacheRoot := filepath.Join(root, "cache")
	captureRoot := filepath.Join(root, "captured-results")
	fixturesRoot := filepath.Join(root, "fixtures")
	for _, dir := range []string{home, outputRoot, cacheRoot, captureRoot, fixturesRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("YTDLP_NO_PLUGINS", "")
	createLocalMediaFixtures(t, fixturesRoot)
	fixtureServer := newLocalFixtureServer(t, fixturesRoot)
	pluginFile := filepath.Join(home, ".config", "yt-dlp", "plugins", "hister-local-fixture", "yt_dlp_plugins", "extractor", "loopback.py")
	if err := os.MkdirAll(filepath.Dir(pluginFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginFile, []byte(localFixturePlugin(fixtureServer.server.URL)), 0o600); err != nil {
		t.Fatal(err)
	}

	wrappedBinary := observingCandidateCommand(t, binary, captureRoot)
	extractor := &YTDownloader{}
	if err := extractor.SetConfig(&config.Extractor{
		Enable: true,
		Options: map[string]any{
			"binary":              wrappedBinary,
			"output_dir":          outputRoot,
			"timeout":             120,
			"max_concurrent_jobs": 2,
			"backend":             "yt-dlp",
		},
	}); err != nil {
		t.Fatal(err)
	}

	firstURL := "https://www.youtube.com/watch?v=" + localFixtureVideoA
	doc := &document.Document{
		URL:        firstURL,
		DocumentID: "local-media-doc-a",
		UserID:     19,
		Title:      "Preserve this title",
		Text:       "Preserve this searchable body",
		Metadata:   map[string]any{"unrelated": "preserve", "existing_media": "not replaced until success"},
	}
	if decision, err := extractWithTimeout(extractor, doc); err != nil || decision != sdk.ExtractorSuccess {
		t.Fatalf("real candidate first extraction = %v, %v", decision, err)
	}
	if doc.Title != "Preserve this title" || doc.Text != "Preserve this searchable body" {
		t.Fatalf("successful enrichment changed searchable content: title=%q text=%q", doc.Title, doc.Text)
	}
	assertCapturedCandidateJobs(t, captureRoot, outputRoot, firstURL, localFixtureVideoA, 2)
	firstReferences := doc.Metadata[MediaReferencesMetadataKey]
	if firstReferences == nil {
		t.Fatal("successful candidate run did not attach media references")
	}
	assertDocumentReferencesMatchReports(t, doc, firstReferences.(string), outputRoot, captureRoot)

	// Make the actual loopback backend unavailable. The second pair of real
	// producer invocations must fail without replacing the successful metadata
	// or the document's normal title/text.
	fixtureServer.setFailure(true)
	if decision, err := extractWithTimeout(extractor, doc); err == nil || decision == sdk.ExtractorSuccess {
		t.Fatalf("failed real candidate extraction = %v, %v; want a failure", decision, err)
	}
	if doc.Metadata[MediaReferencesMetadataKey] != firstReferences || doc.Metadata["unrelated"] != "preserve" {
		t.Fatalf("failed download replaced existing document metadata: %#v", doc.Metadata)
	}
	if doc.Title != "Preserve this title" || doc.Text != "Preserve this searchable body" {
		t.Fatalf("failed enrichment changed searchable content: title=%q text=%q", doc.Title, doc.Text)
	}
	assertCapturedCandidateJobs(t, captureRoot, outputRoot, firstURL, localFixtureVideoA, 3)
	fixtureServer.setFailure(false)

	// Both Hister requests must overlap at the fixture server. Distinct URLs
	// and job IDs ensure neither consumer result can be attributed by shared
	// output-root discovery or title/file-name guessing.
	fixtureServer.requireConcurrentVideos(localFixtureVideoA, localFixtureVideoB)
	secondURL := "https://www.youtube.com/watch?v=" + localFixtureVideoB
	docAURL := firstURL + "&consumer-run=concurrent"
	docA := &document.Document{URL: docAURL, DocumentID: "concurrent-doc-a", UserID: 21, Title: "A", Text: "A text"}
	docB := &document.Document{URL: secondURL, DocumentID: "concurrent-doc-b", UserID: 22, Title: "B", Text: "B text"}
	type extraction struct {
		doc      *document.Document
		decision sdk.Decision
		err      error
	}
	start := make(chan struct{})
	results := make(chan extraction, 2)
	var wg sync.WaitGroup
	for _, concurrentDoc := range []*document.Document{docA, docB} {
		wg.Add(1)
		go func(concurrentDoc *document.Document) {
			defer wg.Done()
			<-start
			decision, err := extractWithTimeout(extractor, concurrentDoc)
			results <- extraction{doc: concurrentDoc, decision: decision, err: err}
		}(concurrentDoc)
	}
	close(start)
	wg.Wait()
	close(results)
	for result := range results {
		if result.err != nil || result.decision != sdk.ExtractorSuccess {
			t.Fatalf("overlapping real candidate extraction for %q = %v, %v", result.doc.URL, result.decision, result.err)
		}
		if result.doc.Title == "" || result.doc.Text == "" {
			t.Errorf("candidate enrichment changed normal content for %q", result.doc.URL)
		}
	}
	if !fixtureServer.concurrentRequestsObserved() {
		t.Fatal("loopback server did not observe both different-video jobs overlapping")
	}
	assertCapturedCandidateJobs(t, captureRoot, outputRoot, firstURL, localFixtureVideoA, 3)
	assertCapturedCandidateJobs(t, captureRoot, outputRoot, docAURL, localFixtureVideoA, 2)
	assertCapturedCandidateJobs(t, captureRoot, outputRoot, secondURL, localFixtureVideoB, 2)
	assertDocumentReferencesMatchReports(t, docA, docA.Metadata[MediaReferencesMetadataKey].(string), outputRoot, captureRoot)
	assertDocumentReferencesMatchReports(t, docB, docB.Metadata[MediaReferencesMetadataKey].(string), outputRoot, captureRoot)

	t.Run("durable worker publication", func(t *testing.T) {
		testutil.InitModel(t)
		request := media.Request{URL: firstURL, UserID: 31, Binary: wrappedBinary, OutputRoot: outputRoot, Backend: "yt-dlp", Timeout: 120}
		queue, err := media.NewQueue(1, RunMediaAttempt, func(source *model.MediaSource, request media.Request) bool {
			return source.DocumentID == document.GetDocID(request.UserID, request.URL) && source.UserID == request.UserID
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(queue.Close)
		id := document.GetDocID(request.UserID, request.URL)
		source, err := model.EnsureMediaSource(id, request.UserID)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := model.ReserveMediaAttempt(source.ID, "local-worker", string(encoded))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := queue.Accept(&media.Intent{AttemptID: attempt.ID, SourceID: source.ID, DocumentID: id, UserID: request.UserID}); err != nil {
			t.Fatal(err)
		}
		deadline := time.NewTimer(150 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			status, err := media.ReadStatus(id, request.UserID)
			if err != nil {
				t.Fatal(err)
			}
			if status.Attempt != nil && status.Attempt.State == model.MediaReady {
				if status.Active || len(status.Available) != 2 {
					t.Fatalf("producer pair was not published atomically: %#v", status)
				}
				for _, artifact := range status.Available {
					if artifact.State != model.MediaReady {
						t.Fatalf("real worker artifact is unavailable: %#v", artifact)
					}
				}
				break
			}
			if status.Attempt != nil && status.Attempt.State == model.MediaFailed {
				t.Fatalf("real durable worker failed: %#v", status.Attempt)
			}
			select {
			case <-deadline.C:
				t.Fatal("real durable worker did not complete")
			case <-tick.C:
			}
		}
	})
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func extractWithTimeout(extractor *YTDownloader, doc *document.Document) (sdk.Decision, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	return extractor.ExtractContext(ctx, doc).Unpack()
}

func createLocalMediaFixtures(t *testing.T, fixturesRoot string) {
	t.Helper()
	for _, fixture := range []struct {
		id        string
		frequency string
	}{
		{localFixtureVideoA, "440"},
		{localFixtureVideoB, "880"},
	} {
		path := filepath.Join(fixturesRoot, fixture.id+".mp4")
		command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
			"-f", "lavfi", "-i", "color=black:s=32x32:d=0.6",
			"-f", "lavfi", "-i", "sine=frequency="+fixture.frequency+":duration=0.6",
			"-c:v", "mpeg4", "-c:a", "aac", "-shortest", "-movflags", "+faststart", path)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("create loopback fixture %q: %v: %s", path, err, output)
		}
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			t.Fatalf("fixture %q is missing or empty: info=%v err=%v", path, info, err)
		}
	}
}

func localFixturePlugin(serverURL string) string {
	return fmt.Sprintf(`import re
from yt_dlp.extractor.youtube import YoutubeIE as BaseYoutubeIE

FIXTURE_BASE_URL = %q

class FixtureYoutubeIE(BaseYoutubeIE, plugin_name="hister-local-fixture"):
    def _real_extract(self, url):
        match = re.search(r"[?&]v=([^&]+)", url)
        if not match:
            raise ValueError("fixture plugin requires a watch URL with a video ID")
        video_id = match.group(1)
        fixture_url = FIXTURE_BASE_URL + "/" + video_id + ".mp4"
        return {
            "id": video_id,
            "title": "Loopback fixture " + video_id,
            "formats": [{
                "format_id": "loopback-mp4",
                "url": fixture_url,
                "ext": "mp4",
                "vcodec": "mpeg4",
                "acodec": "aac",
            }],
        }
`, serverURL)
}

type localMediaFixtureServer struct {
	server      *httptest.Server
	mu          sync.Mutex
	failed      bool
	gateIDs     map[string]struct{}
	seenIDs     map[string]struct{}
	gateRelease chan struct{}
	released    bool
}

func newLocalFixtureServer(t *testing.T, fixturesRoot string) *localMediaFixtureServer {
	t.Helper()
	fixture := &localMediaFixtureServer{}
	files := http.FileServer(http.Dir(fixturesRoot))
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		videoID := strings.TrimSuffix(filepath.Base(r.URL.Path), ".mp4")
		fixture.mu.Lock()
		failed := fixture.failed
		if r.Method == http.MethodGet && fixture.gateIDs != nil {
			if _, expected := fixture.gateIDs[videoID]; expected {
				fixture.seenIDs[videoID] = struct{}{}
				if len(fixture.seenIDs) == len(fixture.gateIDs) && !fixture.released {
					close(fixture.gateRelease)
					fixture.released = true
				}
			}
		}
		release := fixture.gateRelease
		_, gated := fixture.gateIDs[videoID]
		fixture.mu.Unlock()

		if failed {
			http.Error(w, "intentional loopback fixture failure", http.StatusServiceUnavailable)
			return
		}
		if gated {
			select {
			case <-release:
			case <-time.After(30 * time.Second):
				http.Error(w, "concurrent peer did not request its fixture", http.StatusGatewayTimeout)
				return
			}
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (s *localMediaFixtureServer) setFailure(failed bool) {
	s.mu.Lock()
	s.failed = failed
	s.mu.Unlock()
}

func (s *localMediaFixtureServer) requireConcurrentVideos(videoIDs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gateIDs = make(map[string]struct{}, len(videoIDs))
	s.seenIDs = make(map[string]struct{}, len(videoIDs))
	s.gateRelease = make(chan struct{})
	s.released = false
	for _, videoID := range videoIDs {
		s.gateIDs[videoID] = struct{}{}
	}
}

func (s *localMediaFixtureServer) concurrentRequestsObserved() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.released && len(s.seenIDs) == len(s.gateIDs)
}

func observingCandidateCommand(t *testing.T, candidate, captureRoot string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "yt-downloader-observer")
	script := fmt.Sprintf(`#!/bin/sh
set -u
candidate=%s
capture=%s
job_id=
result_file=
output_root=
artifacts_root=
key=
for argument in "$@"; do
  if [ -n "$key" ]; then
    case "$key" in
      job) job_id=$argument ;;
      result) result_file=$argument ;;
      output) output_root=$argument ;;
      artifacts) artifacts_root=$argument ;;
    esac
    key=
    continue
  fi
  case "$argument" in
    --job-id) key=job ;;
    --result-json) key=result ;;
    --output-root) key=output ;;
    --artifacts-root) key=artifacts ;;
  esac
done
if [ -z "$job_id" ] || [ -z "$result_file" ] || [ -z "$output_root" ] || [ -z "$artifacts_root" ]; then
  echo "candidate observer did not receive all named-job paths" >&2
  exit 97
fi
job_output="$output_root/$job_id"
job_reports="$artifacts_root/jobs/$job_id"
if [ -e "$job_output" ] || [ -L "$job_output" ] || [ -e "$job_reports" ] || [ -L "$job_reports" ] || [ -e "$result_file" ] || [ -L "$result_file" ]; then
  echo "candidate invocation paths were not fresh" >&2
  exit 98
fi
printf '%%s\t%%s\t%%s\t%%s\n' "$job_id" "$result_file" "$job_output" "$job_reports" >> "$capture/invocations.tsv"
"$candidate" "$@"
status=$?
if [ -f "$result_file" ]; then
  cp "$result_file" "$capture/$job_id.json" || exit 96
fi
exit "$status"
`, shellQuote(candidate), shellQuote(captureRoot))
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertCapturedCandidateJobs(t *testing.T, captureRoot, outputRoot, requestedURL, videoID string, wantCount int) {
	t.Helper()
	entries, err := os.ReadDir(captureRoot)
	if err != nil {
		t.Fatal(err)
	}
	var manifests []downloaderManifest
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(captureRoot, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var manifest downloaderManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatalf("decode producer-captured manifest %q: %v", entry.Name(), err)
		}
		if manifest.URL == requestedURL {
			manifests = append(manifests, manifest)
		}
	}
	if len(manifests) != wantCount {
		t.Fatalf("captured manifests for %q = %d, want %d", requestedURL, len(manifests), wantCount)
	}
	invocations, err := os.ReadFile(filepath.Join(captureRoot, "invocations.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	allLines := strings.Split(strings.TrimSpace(string(invocations)), "\n")
	jobIDs := make(map[string]struct{}, len(allLines))
	for _, line := range allLines {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			t.Fatalf("observer invocation line has %d fields: %q", len(fields), line)
		}
		if _, duplicate := jobIDs[fields[0]]; duplicate {
			t.Errorf("Hister reused downloader job ID %q", fields[0])
		}
		jobIDs[fields[0]] = struct{}{}
		if _, err := os.Lstat(fields[1]); !os.IsNotExist(err) {
			t.Errorf("temporary result file %q was not removed after consumption: %v", fields[1], err)
		}
		if _, err := os.Stat(fields[2]); err != nil {
			t.Errorf("per-job media directory %q missing: %v", fields[2], err)
		}
		if _, err := os.Stat(fields[3]); err != nil {
			t.Errorf("per-job operational report directory %q missing: %v", fields[3], err)
		}
		requestBytes, err := os.ReadFile(filepath.Join(fields[3], "request.json"))
		if err != nil {
			t.Errorf("read candidate operational request report in %q: %v", fields[3], err)
			continue
		}
		var request struct {
			JobID        string   `json:"job_id"`
			OutputDir    string   `json:"output_dir"`
			RequestedURL []string `json:"requested_urls"`
		}
		if err := json.Unmarshal(requestBytes, &request); err != nil {
			t.Errorf("decode candidate request report: %v", err)
			continue
		}
		if request.JobID != fields[0] || request.OutputDir != fields[2] || len(request.RequestedURL) != 1 {
			t.Errorf("candidate request report does not correlate to fresh invocation %q: %+v", fields[0], request)
			continue
		}
		manifestBytes, err := os.ReadFile(filepath.Join(captureRoot, fields[0]+".json"))
		if err != nil {
			t.Errorf("candidate did not publish result manifest for job %q: %v", fields[0], err)
			continue
		}
		var correlated downloaderManifest
		if err := json.Unmarshal(manifestBytes, &correlated); err != nil {
			t.Errorf("decode captured manifest for job %q: %v", fields[0], err)
			continue
		}
		if correlated.JobID != request.JobID || correlated.URL != request.RequestedURL[0] || correlated.OutputDir != request.OutputDir {
			t.Errorf("terminal manifest and operational request disagree for %q: request=%+v manifest=%+v", fields[0], request, correlated)
		}
	}
	for _, manifest := range manifests {
		if manifest.SchemaVersion != 1 {
			t.Errorf("captured candidate manifest has unexpected schema: %+v", manifest)
		}
		if manifest.URL != requestedURL {
			t.Errorf("captured manifest URL = %q, want %q", manifest.URL, requestedURL)
		}
		if manifest.Status == "completed" && manifest.VideoID != videoID {
			t.Errorf("completed manifest video ID = %q, want %q", manifest.VideoID, videoID)
		}
		if manifest.Status != "completed" && manifest.Status != "failed" && manifest.Status != "unavailable" && manifest.Status != "cancelled" {
			t.Errorf("captured candidate manifest has unknown terminal status: %+v", manifest)
		}
		if manifest.JobID == "" || !localJobIDPattern.MatchString(manifest.JobID) {
			t.Errorf("producer reported malformed or non-Hister job ID %q", manifest.JobID)
		}
		if _, duplicate := jobIDs[manifest.JobID]; !duplicate {
			t.Errorf("manifest job ID %q has no observed fresh invocation", manifest.JobID)
		}
		expectedOutputDir := filepath.Join(outputRoot, manifest.JobID)
		if manifest.OutputDir != expectedOutputDir {
			t.Errorf("manifest output_dir = %q, want exact job dir %q", manifest.OutputDir, expectedOutputDir)
		}
		wantJobType, wantSuffix := "scrape_videos", "-video"
		if strings.HasSuffix(manifest.JobID, "-audio") {
			wantJobType, wantSuffix = "scrape_audio_only", "-audio"
		}
		if manifest.JobType != wantJobType || !strings.HasSuffix(manifest.JobID, wantSuffix) {
			t.Errorf("job ID/type mismatch: %q / %q, want %q and %s", manifest.JobID, manifest.JobType, wantJobType, wantSuffix)
		}
		if manifest.Status != "completed" {
			if manifest.Error == nil || manifest.Error.Code == "" || manifest.Error.Message == "" || len(manifest.MediaPaths) != 0 {
				t.Errorf("failure manifest has inconsistent terminal fields: %+v", manifest)
			}
			continue
		}
		if manifest.VideoID != videoID || len(manifest.MediaPaths) == 0 || manifest.TranscriptPaths == nil || manifest.Error != nil {
			t.Errorf("completed manifest has invalid identity/media/transcript/error fields: %+v", manifest)
		}
		for _, rawPath := range manifest.MediaPaths {
			path := rawPath
			if !filepath.IsAbs(path) || filepath.Clean(path) != path {
				t.Errorf("producer media path is not canonical absolute path: %q", path)
				continue
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				t.Errorf("resolve media path %q: %v", path, err)
				continue
			}
			if !withinDirectory(expectedOutputDir, resolved) {
				t.Errorf("nested media path %q escaped job output directory %q", resolved, expectedOutputDir)
			}
			if filepath.Dir(resolved) == expectedOutputDir {
				t.Errorf("candidate did not exercise nested media containment: %q is a direct child of %q", resolved, expectedOutputDir)
			}
			info, err := os.Stat(resolved)
			if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
				t.Errorf("reported media is not a nonempty regular file: %q (stat=%v)", resolved, err)
			}
		}
	}
}

func assertDocumentReferencesMatchReports(t *testing.T, doc *document.Document, encoded, outputRoot, captureRoot string) {
	t.Helper()
	var references mediaReferences
	if err := json.Unmarshal([]byte(encoded), &references); err != nil {
		t.Fatal(err)
	}
	invocations, err := os.ReadFile(filepath.Join(captureRoot, "invocations.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	var expectedVideo, expectedAudio []string
	for _, line := range strings.Split(strings.TrimSpace(string(invocations)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			continue
		}
		manifestBytes, err := os.ReadFile(filepath.Join(captureRoot, fields[0]+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var manifest downloaderManifest
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.URL != doc.URL || manifest.Status != "completed" {
			continue
		}
		uris := fileURIs(manifest.MediaPaths)
		if manifest.JobType == "scrape_videos" {
			expectedVideo = append(expectedVideo, uris...)
		} else if manifest.JobType == "scrape_audio_only" {
			expectedAudio = append(expectedAudio, uris...)
		}
	}
	if len(expectedVideo) == 0 || len(expectedAudio) == 0 {
		t.Fatalf("no successful producer reports found for document URL %q", doc.URL)
	}
	if len(references.Video) != len(expectedVideo) || len(references.Audio) != len(expectedAudio) {
		t.Fatalf("document references video=%v audio=%v; expected producer outputs video=%v audio=%v", references.Video, references.Audio, expectedVideo, expectedAudio)
	}
	for _, group := range []struct {
		uris []string
		want []string
	}{{references.Video, expectedVideo}, {references.Audio, expectedAudio}} {
		for _, uri := range group.uris {
			if !containsString(group.want, uri) {
				t.Errorf("document %q references media not reported by its producer invocation: %q", doc.URL, uri)
			}
			if !strings.HasPrefix(files.FileURLToPath(uri), outputRoot+string(filepath.Separator)) {
				t.Errorf("document reference %q escaped media output root", uri)
			}
			if info, err := os.Lstat(files.FileURLToPath(uri)); err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
				t.Errorf("real producer output is not a nonempty regular file: %q: %v", uri, err)
			}
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
