// SPDX-License-Identifier: AGPL-3.0-or-later

package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/server/document"
	"github.com/asciimoo/hister/server/extractor"
	yt "github.com/asciimoo/hister/server/extractor/extractors/yt_downloader"
	"github.com/asciimoo/hister/server/extractor/sdk"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"
)

const mediaSourceURL = "https://www.youtube.com/watch?v=abcdefghijk"

type mediaContentExtractor struct{ run func(*document.Document) }

func (*mediaContentExtractor) Name() string                   { return "media-regression-content" }
func (*mediaContentExtractor) Description() string            { return "test content extractor" }
func (*mediaContentExtractor) Capabilities() sdk.Capabilities { return sdk.Capabilities{Extract: true} }
func (*mediaContentExtractor) Match(*document.Document) bool  { return true }
func (*mediaContentExtractor) GetConfig() *sdk.Config         { return &sdk.Config{Enable: true} }
func (*mediaContentExtractor) SetConfig(*sdk.Config) error    { return nil }
func (e *mediaContentExtractor) Extract(d *document.Document) sdk.ExtractResult {
	if e.run != nil {
		e.run(d)
	}
	d.Text = "searchable " + d.Title
	return sdk.Extracted()
}

func (*mediaContentExtractor) Preview(*document.Document) sdk.PreviewResult {
	return sdk.PreviewFallback(nil)
}

const mediaSuccessScript = `command=$1
requested_url=$2
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
echo "media $job" > "$file"
printf '{"schema_version":1,"job_id":"%s","job_type":"%s","url":"%s","video_id":"abcdefghijk","output_dir":"%s/%s","status":"completed","media_paths":["%s"],"transcript_paths":[],"error":null}\n' "$job" "$kind" "$requested_url" "$root" "$job" "$file" > "$result"
`

func mediaIndexer(t *testing.T, script string, content *mediaContentExtractor, names ...string) (*Indexer, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("media indexer fixture executes a POSIX shell downloader; Windows subprocess fixture is not available")
	}
	name := config.YTDownloaderName
	if len(names) > 0 {
		name = names[0]
	}
	previous, err := extractor.NewRegistry(extractor.DefaultRegistry().Extractors()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { extractor.DefaultRegistry().Replace(previous) })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	binary := testutil.WriteFile(t, t.TempDir(), "downloader", []byte("#!/bin/sh\n"+script))
	if err := os.Chmod(binary, 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg := testutil.Config(t)
	cfg.Server.Database = "media.sqlite3"
	cfg.Extractors = map[string]*config.Extractor{name: {Enable: true, Options: map[string]any{"binary": binary, "output_dir": root, "timeout": 30}}}
	testutil.InitModelWithConfig(t, cfg)
	e := &yt.YTDownloader{}
	if err := e.SetConfig(cfg.Extractors[name]); err != nil {
		t.Fatal(err)
	}
	registry, err := extractor.NewRegistry(e, content)
	if err != nil {
		t.Fatal(err)
	}
	extractor.DefaultRegistry().Replace(registry)
	idx := newTestIndexer(t, cfg)
	t.Cleanup(idx.Close)
	return idx, root
}

func waitMedia(t *testing.T, generation uint64, state string) *model.MediaAttempt {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	var attempt model.MediaAttempt
	for {
		if err := model.DB.Where("generation = ?", generation).First(&attempt).Error; err == nil && attempt.State == state {
			return &attempt
		}
		select {
		case <-deadline.C:
			t.Fatalf("attempt generation %d never became %s: %#v", generation, state, attempt)
		case <-tick.C:
		}
	}
}

func awaitMediaSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal("media test barrier was not reached")
	}
}

func selectedMedia(t *testing.T) []model.MediaArtifact {
	t.Helper()
	source, err := model.GetMediaSource(mediaSourceURL)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := model.CurrentMediaArtifacts(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	return artifacts
}

func TestMediaLegacyAndCanonicalConfigBothDispatchOnce(t *testing.T) {
	for _, name := range []string{config.LegacyYTDownloaderName, config.YTDownloaderName} {
		t.Run(name, func(t *testing.T) {
			idx, _ := mediaIndexer(t, mediaSuccessScript, &mediaContentExtractor{}, name)
			if err := idx.Add(&document.Document{URL: mediaSourceURL, Title: "alias", HTML: "<p>source</p>"}); err != nil {
				t.Fatal(err)
			}
			waitMedia(t, 1, model.MediaReady)
			if len(selectedMedia(t)) != 2 {
				t.Fatal("alias configuration did not publish its complete pair")
			}
			var count int64
			if err := model.DB.Model(&model.MediaAttempt{}).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("alias scheduled duplicate attempts: count=%d, error=%v", count, err)
			}
		})
	}
}

// Port of the fresh-update review reproducer. The fixture is now explicitly
// server-owned: accepting submitted metadata as its provenance would preserve
// the original authorization vulnerability instead of fixing it.
func TestMediaFailedFreshUpdatePreservesStoredSelection(t *testing.T) {
	idx, root := mediaIndexer(t, "echo unavailable >&2; exit 1\n", &mediaContentExtractor{})
	if err := idx.Add(&document.Document{URL: mediaSourceURL, Text: "old", Processed: true}); err != nil {
		t.Fatal(err)
	}
	previous := testutil.SeedMediaPair(t, mediaSourceURL, 0,
		testutil.WriteFile(t, root, "old.webm", []byte("existing video")), testutil.WriteFile(t, root, "old.mp3", []byte("existing audio")))
	if err := idx.AddContext(context.Background(), &document.Document{URL: mediaSourceURL, Title: "new visit", HTML: "<p>new visit</p>"}); err != nil {
		t.Fatal(err)
	}
	waitMedia(t, 2, model.MediaFailed)
	if got := selectedMedia(t); !reflect.DeepEqual(got, previous) {
		t.Fatalf("failed fresh update replaced retained artifacts: %#v", got)
	}
	stored := idx.GetByURLAndUser(mediaSourceURL, 0)
	if stored == nil || stored.Text != "searchable new visit" {
		t.Fatalf("media failure blocked normal indexing: %#v", stored)
	}
}

// Port of the review's deterministic delayed-content-extraction reproducer.
func TestMediaOlderIndexCommitCannotReplaceNewerSelection(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	idx, _ := mediaIndexer(t, mediaSuccessScript, &mediaContentExtractor{run: func(d *document.Document) {
		if d.Title == "older" {
			close(reached)
			<-release
		}
	}})
	done := make(chan error, 1)
	finished := make(chan struct{})
	releaseOlder := sync.OnceFunc(func() { close(release) })
	go func() {
		defer close(finished)
		done <- idx.AddContext(context.Background(), &document.Document{URL: mediaSourceURL, Title: "older", HTML: "<p>older</p>"})
	}()
	t.Cleanup(func() { releaseOlder(); <-finished })
	awaitMediaSignal(t, reached)
	newerErr := idx.AddContext(context.Background(), &document.Document{URL: mediaSourceURL, Title: "newer", HTML: "<p>newer</p>"})
	if newerErr != nil {
		releaseOlder()
		<-done
		t.Fatal(newerErr)
	}
	waitMedia(t, 2, model.MediaReady)
	accepted := selectedMedia(t)
	releaseOlder()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitMedia(t, 1, model.MediaReady)
	if got := selectedMedia(t); !reflect.DeepEqual(got, accepted) {
		t.Fatalf("late older index write regressed selected media: %#v", got)
	}
	artifacts, err := model.MediaArtifactsForDocument(mediaSourceURL, 0)
	if err != nil || len(artifacts) != 4 {
		t.Fatalf("late success did not retain history: %d, %v", len(artifacts), err)
	}
}

func TestMediaBatchWaitsForActualSaveAndKeepsReservedOrdering(t *testing.T) {
	idx, _ := mediaIndexer(t, mediaSuccessScript, &mediaContentExtractor{})
	batch := idx.NewMultiBatch()
	t.Cleanup(batch.Discard)
	if err := batch.Add(&document.Document{URL: mediaSourceURL, Title: "older batch", HTML: "<p>older</p>"}); err != nil {
		t.Fatal(err)
	}
	waitMedia(t, 1, model.MediaPendingCommit)
	if queued, err := model.NextQueuedMediaAttempt(); err != nil || queued != nil {
		t.Fatalf("staging fabricated queued work: %#v, %v", queued, err)
	}
	if idx.GetByDocID(mediaSourceURL) != nil {
		t.Fatal("batch staging committed its document")
	}
	if err := idx.Add(&document.Document{URL: mediaSourceURL, Title: "newer", HTML: "<p>newer</p>"}); err != nil {
		t.Fatal(err)
	}
	waitMedia(t, 2, model.MediaReady)
	accepted := selectedMedia(t)
	if err := batch.Save(); err != nil {
		t.Fatal(err)
	}
	waitMedia(t, 1, model.MediaReady)
	if got := selectedMedia(t); !reflect.DeepEqual(got, accepted) {
		t.Fatalf("later batch Save regressed selected media: %#v", got)
	}
}

func TestMediaDiscardAndFailedCommitsDoNotQueue(t *testing.T) {
	for _, operation := range []string{"discard", "single failure", "batch failure", "acceptance failure"} {
		t.Run(operation, func(t *testing.T) {
			idx, _ := mediaIndexer(t, mediaSuccessScript, &mediaContentExtractor{})
			if operation == "single failure" || operation == "batch failure" {
				for name, index := range idx.indexers {
					idx.indexers[name] = failingBlobCommitIndex{bleveIndex: index, fail: func() error { return errors.New("injected index failure") }}
				}
			}
			if operation == "acceptance failure" {
				if err := model.DB.Exec(`CREATE TRIGGER reject_queue BEFORE UPDATE OF state ON media_attempts WHEN NEW.state = 'queued' BEGIN SELECT RAISE(ABORT, 'injected queue acceptance failure'); END`).Error; err != nil {
					t.Fatal(err)
				}
			}
			doc := &document.Document{URL: mediaSourceURL, HTML: "<p>source</p>"}
			if operation == "discard" || operation == "batch failure" {
				batch := idx.NewMultiBatch()
				if err := batch.Add(doc); err != nil {
					t.Fatal(err)
				}
				if operation == "discard" {
					batch.Discard()
				} else if err := batch.Save(); err == nil {
					t.Fatal("batch ignored injected failure")
				}
			} else if err := idx.Add(doc); err == nil {
				t.Fatal("single write ignored injected failure")
			}
			waitMedia(t, 1, model.MediaInterrupted)
			if queued, err := model.NextQueuedMediaAttempt(); err != nil || queued != nil {
				t.Fatalf("failed/uncommitted write left queued work: %#v, %v", queued, err)
			}
			if operation == "acceptance failure" && idx.GetByDocID(mediaSourceURL) == nil {
				t.Fatal("SQL failure after Bleve commit erased searchable source")
			}
		})
	}
}

func TestMediaDeleteFencesDelayedIndexWrite(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	idx, _ := mediaIndexer(t, mediaSuccessScript, &mediaContentExtractor{run: func(*document.Document) { close(reached); <-release }})
	done := make(chan error, 1)
	finished := make(chan struct{})
	releaseWrite := sync.OnceFunc(func() { close(release) })
	go func() {
		defer close(finished)
		done <- idx.Add(&document.Document{URL: mediaSourceURL, HTML: "<p>delayed</p>"})
	}()
	t.Cleanup(func() { releaseWrite(); <-finished })
	awaitMediaSignal(t, reached)
	deleteErr := idx.Delete(mediaSourceURL)
	releaseWrite()
	writeErr := <-done
	if deleteErr != nil {
		t.Fatal(deleteErr)
	}
	if !errors.Is(writeErr, model.ErrMediaSourceRevoked) {
		t.Fatalf("stale write after deletion = %v", writeErr)
	}
	if idx.GetByDocID(mediaSourceURL) != nil {
		t.Fatal("delayed request resurrected deleted source")
	}
}

func TestMediaAcceptedWorkOutlivesRequestAndDoesNotBlockSearch(t *testing.T) {
	idx, _ := mediaIndexer(t, mediaSuccessScript, &mediaContentExtractor{})
	idx.mediaQueue.Close()
	started, release := make(chan struct{}), make(chan struct{})
	queue, err := media.NewQueue(1, func(ctx context.Context, request media.Request, events media.ModeEvents) ([]model.MediaArtifactInput, error) {
		close(started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
		}
		return yt.RunMediaAttempt(ctx, request, events)
	}, func(source *model.MediaSource, request media.Request) bool {
		return idx.GetByDocID(source.DocumentID) != nil
	})
	if err != nil {
		t.Fatal(err)
	}
	idx.mediaQueue = queue
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := idx.AddContext(ctx, &document.Document{URL: mediaSourceURL, Title: "visible", HTML: "<p>source</p>"}); err != nil {
		t.Fatal(err)
	}
	awaitMediaSignal(t, started)
	cancel()
	if got := idx.GetByDocID(mediaSourceURL); got == nil || got.Text != "searchable visible" {
		t.Fatalf("searchable source waited for download: %#v", got)
	}
	if artifacts := selectedMedia(t); len(artifacts) != 0 {
		t.Fatal("unpublished work exposed playback artifacts")
	}
	close(release)
	attempt := waitMedia(t, 1, model.MediaReady)
	if attempt.VideoJobID == nil || attempt.AudioJobID == nil || *attempt.VideoJobID == *attempt.AudioJobID {
		t.Fatalf("producer jobs were not persisted independently: %#v", attempt)
	}
	for _, artifact := range selectedMedia(t) {
		if _, err := os.Stat(filepath.Join(artifact.StorageRoot, artifact.RelativePath)); err != nil {
			t.Fatal(err)
		}
	}
}
