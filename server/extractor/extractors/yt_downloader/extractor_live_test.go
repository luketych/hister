//go:build live

package yt_downloader

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/files"
	"github.com/asciimoo/hister/server/document"
	"github.com/asciimoo/hister/server/extractor/sdk"
)

// TestLiveDownloadRealYouTubeVideo is opt-in because it makes real YouTube
// requests and writes downloaded media to HISTER_YT_LIVE_OUTPUT_DIR.
func TestLiveDownloadRealYouTubeVideo(t *testing.T) {
	videoURL := strings.TrimSpace(os.Getenv("HISTER_YT_LIVE_URL"))
	outputDir := strings.TrimSpace(os.Getenv("HISTER_YT_LIVE_OUTPUT_DIR"))
	if videoURL == "" || outputDir == "" {
		t.Skip("set HISTER_YT_LIVE_URL and HISTER_YT_LIVE_OUTPUT_DIR to run the real download")
	}
	binary := envOrDefault("HISTER_YT_LIVE_BINARY", defaultBinary)

	absOutputDir, err := filepath.Abs(outputDir)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("step 1/6: target URL: %s", videoURL)
	t.Logf("step 2/6: downloader binary: %s", binary)
	t.Logf("step 2/6: media output root: %s", absOutputDir)
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("step 2/6: downloader binary %q is unavailable: %v", binary, err)
	}
	e := &YTDownloader{}
	if err := e.SetConfig(&config.Extractor{
		Enable: true,
		Options: map[string]any{
			"binary":              binary,
			"output_dir":          absOutputDir,
			"timeout":             1800,
			"max_concurrent_jobs": 1,
			"backend":             "auto",
		},
	}); err != nil {
		t.Fatalf("step 2/6: configure extractor: %v", err)
	}

	doc := &document.Document{URL: videoURL}
	if !e.Match(doc) {
		t.Fatalf("step 3/6: extractor does not match URL %q", videoURL)
	}
	t.Log("step 3/6: URL matches the download-only enricher")
	t.Log("step 4/6: invoking Hister enricher; yt_downloader runs video, then audio commands")

	ctx, cancel := context.WithTimeout(context.Background(), 32*time.Minute)
	defer cancel()
	defer logLiveArtifacts(t, absOutputDir)
	decision, extractErr := e.ExtractContext(ctx, doc).Unpack()
	if extractErr != nil {
		t.Fatalf("step 4/6: download failed (%v): %v", decision, extractErr)
	}
	if decision != sdk.ExtractorSuccess {
		t.Fatalf("step 4/6: enrichment decision = %v, want success", decision)
	}
	if doc.Title != "" || doc.Text != "" {
		t.Fatalf("step 5/6: downloader changed indexed content: title=%q text=%q", doc.Title, doc.Text)
	}
	encoded, ok := doc.Metadata[MediaReferencesMetadataKey].(string)
	if !ok {
		t.Fatalf("step 5/6: no downloaded-media metadata: %#v", doc.Metadata)
	}
	var references mediaReferences
	if err := json.Unmarshal([]byte(encoded), &references); err != nil {
		t.Fatalf("step 5/6: decode downloaded-media metadata: %v", err)
	}
	if len(references.Video) == 0 || len(references.Audio) == 0 {
		t.Fatalf("step 5/6: expected attached video and audio-mode media; got video=%v audio=%v", references.Video, references.Audio)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absOutputDir)
	if err != nil {
		t.Fatalf("step 5/6: resolve media output root: %v", err)
	}
	for _, uri := range append(references.Video, references.Audio...) {
		path := files.FileURLToPath(uri)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("step 5/6: stat manifest-reported media file %q: %v", path, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			t.Fatalf("step 5/6: reported media is not a non-empty regular file: %q", path)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatalf("step 5/6: resolve reported media file %q: %v", path, err)
		}
		relative, err := filepath.Rel(resolvedRoot, resolved)
		if err != nil || relative == "." || !filepath.IsLocal(relative) {
			t.Fatalf("step 5/6: reported media file %q escapes output root %q", resolved, resolvedRoot)
		}
		t.Logf("step 5/6: verified manifest-reported media: %s (%d bytes)", resolved, info.Size())
	}
	t.Log("step 6/6: media files downloaded; document content was left unchanged for Hister's normal extractor")
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func logLiveArtifacts(t *testing.T, root string) {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		count++
		t.Logf("artifact: %s (%d bytes)", path, info.Size())
		return nil
	})
	if err != nil {
		t.Logf("could not list output artifacts under %s: %v", root, err)
	} else if count == 0 {
		t.Logf("no output artifacts found under %s", root)
	}
}
