// SPDX-License-Identifier: AGPL-3.0-or-later

package indexer

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/asciimoo/hister/server/document"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"
	servertypes "github.com/asciimoo/hister/server/types"

	"gorm.io/gorm"
)

func commandIndexer(t *testing.T, runner media.Runner) (*Indexer, *model.MediaAttempt, []model.MediaArtifact) {
	t.Helper()
	idx, root := mediaIndexer(t, mediaSuccessScript, &mediaContentExtractor{})
	idx.mediaQueue.Close()
	if err := idx.Add(&document.Document{URL: mediaSourceURL, Text: "preserve searchable source", Processed: true}); err != nil {
		t.Fatal(err)
	}
	previous := testutil.SeedMediaPair(t, mediaSourceURL, 0,
		testutil.WriteFile(t, root, "previous.webm", []byte("previous video")), testutil.WriteFile(t, root, "previous.mp3", []byte("previous audio")))
	source, err := model.GetMediaSource(mediaSourceURL)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := model.AcceptMediaAttempt(source.ID, "failure", `{"binary":"obsolete","output_root":"obsolete"}`)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := model.ClaimMediaAttempt(failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.FailMediaAttempt(worker.ID, worker.ClaimToken, "download_failed"); err != nil {
		t.Fatal(err)
	}
	queue, err := media.NewQueue(2, runner, func(source *model.MediaSource, request media.Request) bool {
		return source.DocumentID == document.GetDocID(request.UserID, request.URL) && idx.GetByDocID(source.DocumentID) != nil
	})
	if err != nil {
		t.Fatal(err)
	}
	idx.mediaQueue = queue
	return idx, failed, previous
}

func commandFiles(request media.Request, events media.ModeEvents) ([]model.MediaArtifactInput, error) {
	root, err := filepath.EvalSymlinks(request.OutputRoot)
	if err != nil {
		return nil, err
	}
	var pair []model.MediaArtifactInput
	for _, role := range []string{model.MediaVideo, model.MediaAudio} {
		job := rand.Text() + "-" + role
		if err := events.Prepared(role, job); err != nil {
			return nil, err
		}
		if err := events.Started(role); err != nil {
			return nil, err
		}
		contentType := "video/webm"
		if role == model.MediaAudio {
			contentType = "audio/mpeg"
		}
		path := job + ".media"
		if err := os.WriteFile(filepath.Join(root, path), []byte("new bytes"), 0o600); err != nil {
			return nil, err
		}
		if err := events.Completed(role); err != nil {
			return nil, err
		}
		pair = append(pair, model.MediaArtifactInput{Role: role, JobID: job, StorageRoot: root, RelativePath: path, Size: 9, ContentType: contentType})
	}
	return pair, nil
}

func TestMediaCommandsConcurrentRetryAndCancellationUseCurrentSettings(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	requests := make(chan media.Request, 1)
	workerContexts := make(chan context.Context, 1)
	requestContext, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	idx, failed, previous := commandIndexer(t, func(ctx context.Context, request media.Request, _ media.ModeEvents) ([]model.MediaArtifactInput, error) {
		requests <- request
		workerContexts <- ctx
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	})
	start := make(chan struct{})
	results := make(chan *model.MediaRetryResult, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			<-start
			result, err := idx.RetryMedia(requestContext, mediaSourceURL, failed.ID, MediaActor{})
			if err != nil {
				t.Error(err)
				return
			}
			results <- result
		})
	}
	close(start)
	wg.Wait()
	close(results)
	id, created := "", 0
	for result := range results {
		if id == "" {
			id = result.Attempt.ID
		}
		if result.Attempt.ID != id {
			t.Fatal("concurrent retry duplicated accepted work")
		}
		if !result.Replayed {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created intents = %d", created)
	}
	awaitMediaSignal(t, started)
	cancelRequest()
	if workerContext := <-workerContexts; workerContext.Err() != nil {
		t.Fatal("closing the retry request cancelled accepted work")
	}
	request := <-requests
	if request.Binary == "obsolete" || request.OutputRoot == "obsolete" || request.URL != mediaSourceURL || request.UserID != 0 {
		t.Fatalf("retry replayed stale configuration/identity: %#v", request)
	}
	if _, err := idx.CancelMedia(context.Background(), mediaSourceURL, id, MediaActor{}); err != nil {
		t.Fatal(err)
	}
	awaitMediaSignal(t, stopped)
	if got := selectedMedia(t); !reflect.DeepEqual(got, previous) {
		t.Fatal("retry/cancel lost previous ready download")
	}
	if doc := idx.GetByDocID(mediaSourceURL); doc == nil || doc.Text != "preserve searchable source" || doc.AddCount != 1 {
		t.Fatalf("command re-indexed source: %#v", doc)
	}
}

func TestMediaCommandsFailedCancellationDoesNotSignalWorker(t *testing.T) {
	workerContexts := make(chan context.Context, 1)
	idx, failed, previous := commandIndexer(t, func(ctx context.Context, _ media.Request, _ media.ModeEvents) ([]model.MediaArtifactInput, error) {
		workerContexts <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	})
	result, err := idx.RetryMedia(context.Background(), mediaSourceURL, failed.ID, MediaActor{})
	if err != nil {
		t.Fatal(err)
	}
	var workerContext context.Context
	select {
	case workerContext = <-workerContexts:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	if err := model.DB.Exec(`CREATE TRIGGER reject_cancel BEFORE UPDATE OF state ON media_attempts WHEN NEW.state = 'cancelled' BEGIN SELECT RAISE(ABORT, 'injected cancellation failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := idx.CancelMedia(context.Background(), mediaSourceURL, result.Attempt.ID, MediaActor{}); err == nil {
		t.Fatal("ignored failed durable cancellation fence")
	}
	if workerContext.Err() != nil {
		t.Fatal("signalled worker before cancellation committed")
	}
	if got := selectedMedia(t); !reflect.DeepEqual(got, previous) {
		t.Fatal("failed cancellation changed selected artifacts")
	}
	if err := model.DB.Exec("DROP TRIGGER reject_cancel").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := idx.CancelMedia(context.Background(), mediaSourceURL, result.Attempt.ID, MediaActor{}); err != nil {
		t.Fatal(err)
	}
}

func TestMediaCommandsFinalizingCancelFencesLateProducerReturn(t *testing.T) {
	finalizing, released, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	idx, failed, previous := commandIndexer(t, func(_ context.Context, request media.Request, events media.ModeEvents) ([]model.MediaArtifactInput, error) {
		pair, err := commandFiles(request, events)
		if err != nil {
			return nil, err
		}
		close(finalizing)
		// Deliberately model a producer returning a candidate despite cancellation.
		<-released
		close(returned)
		return pair, nil
	})
	t.Cleanup(release)
	result, err := idx.RetryMedia(context.Background(), mediaSourceURL, failed.ID, MediaActor{})
	if err != nil {
		t.Fatal(err)
	}
	awaitMediaSignal(t, finalizing)
	if _, err := idx.CancelMedia(context.Background(), mediaSourceURL, result.Attempt.ID, MediaActor{}); err != nil {
		t.Fatal(err)
	}
	release()
	awaitMediaSignal(t, returned)
	idx.mediaQueue.Close()
	var stored model.MediaAttempt
	if err := model.DB.First(&stored, "id = ?", result.Attempt.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != model.MediaCancelled {
		t.Fatalf("late return invalidated cancellation: %#v", stored)
	}
	if got := selectedMedia(t); !reflect.DeepEqual(got, previous) {
		t.Fatal("cancelled finalizing candidate replaced prior pair")
	}
}

func TestMediaCommandsDoNotRetryAfterDeletionOrWhileClosed(t *testing.T) {
	idx, failed, _ := commandIndexer(t, func(context.Context, media.Request, media.ModeEvents) ([]model.MediaArtifactInput, error) {
		return nil, errors.New("must not execute")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := idx.RetryMedia(ctx, mediaSourceURL, failed.ID, MediaActor{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request accepted work: %v", err)
	}
	if err := idx.Delete(mediaSourceURL); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add(&document.Document{URL: mediaSourceURL, Text: "re-added", Processed: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.RetryMedia(context.Background(), mediaSourceURL, failed.ID, MediaActor{}); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("re-add revived an obsolete retry: %v", err)
	}
	if _, err := idx.CancelMedia(context.Background(), mediaSourceURL, failed.ID, MediaActor{}); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("re-add revived an obsolete cancellation: %v", err)
	}
	idx.stopMediaQueue()
	if _, err := idx.CancelMedia(context.Background(), mediaSourceURL, "unknown", MediaActor{}); !errors.Is(err, media.ErrDispatcherUnavailable) {
		t.Fatalf("closed dispatcher accepted commands: %v", err)
	}
}

func TestMediaCommandsOwnershipMoveRechecksActualBinding(t *testing.T) {
	started := make(chan struct{})
	requests := make(chan media.Request, 1)
	idx, failed, _ := commandIndexer(t, func(ctx context.Context, request media.Request, _ media.ModeEvents) ([]model.MediaArtifactInput, error) {
		requests <- request
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	idx.userHandling = true
	const owner = uint(22)
	if _, err := idx.UpdateByQuery("*", nil, servertypes.DocumentChanges{UserID: new(owner)}, false); err != nil {
		t.Fatal(err)
	}
	newID := document.GetDocID(owner, mediaSourceURL)
	if _, err := idx.RetryMedia(context.Background(), newID, failed.ID, MediaActor{UserID: 11}); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("old owner retained retry authority: %v", err)
	}
	result, err := idx.RetryMedia(context.Background(), newID, failed.ID, MediaActor{UserID: owner})
	if err != nil {
		t.Fatal(err)
	}
	awaitMediaSignal(t, started)
	request := <-requests
	if request.UserID != owner {
		t.Fatalf("retry after move used previous ownership: %#v", request)
	}
	if _, err := idx.CancelMedia(context.Background(), newID, result.Attempt.ID, MediaActor{UserID: owner}); err != nil {
		t.Fatal(err)
	}
	var stored model.MediaAttempt
	if err := model.DB.First(&stored, "id = ?", result.Attempt.ID).Error; err != nil {
		t.Fatal(err)
	}
	var frozen media.Request
	if err := json.Unmarshal([]byte(stored.RequestJSON), &frozen); err != nil || frozen.UserID != owner {
		t.Fatalf("persisted request retained previous owner: %#v, %v", frozen, err)
	}
}
