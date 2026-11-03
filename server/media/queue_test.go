// SPDX-License-Identifier: AGPL-3.0-or-later

package media_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asciimoo/hister/server/document"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"
)

func queueDatabase(t *testing.T) string {
	t.Helper()
	cfg := testutil.Config(t)
	cfg.Server.Database = "queue.sqlite3"
	testutil.InitModelWithConfig(t, cfg)
	db, err := model.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	// SQLite permits one writer at a time. Keep queue-level goroutine tests
	// deterministic across platforms; model-level writer tests opt into a pool.
	db.SetMaxOpenConns(1)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func reservation(t *testing.T, request media.Request) *media.Intent {
	t.Helper()
	id := document.GetDocID(request.UserID, request.URL)
	source, err := model.EnsureMediaSource(id, request.UserID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := model.ReserveMediaAttempt(source.ID, rand.Text(), string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	return &media.Intent{AttemptID: attempt.ID, SourceID: source.ID, DocumentID: id, UserID: request.UserID}
}

func getAttempt(t *testing.T, id string) model.MediaAttempt {
	t.Helper()
	var attempt model.MediaAttempt
	if err := model.DB.First(&attempt, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	return attempt
}

func awaitAttempt(t *testing.T, id, state string) model.MediaAttempt {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		attempt := getAttempt(t, id)
		if attempt.State == state {
			return attempt
		}
		select {
		case <-deadline.C:
			t.Fatalf("attempt did not become %s: %#v", state, attempt)
		case <-tick.C:
		}
	}
}

func receiveStart(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
}

func fakeRunner(ctx context.Context, request media.Request, events media.ModeEvents) ([]model.MediaArtifactInput, error) {
	var pair []model.MediaArtifactInput
	for _, role := range []string{model.MediaVideo, model.MediaAudio} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		job := rand.Text() + "-" + role
		if err := events.Prepared(role, job); err != nil {
			return nil, err
		}
		if err := events.Started(role); err != nil {
			return nil, err
		}
		data := []byte("media " + job)
		name, contentType := job+".webm", "video/webm"
		if role == model.MediaAudio {
			name, contentType = job+".mp3", "audio/mpeg"
		}
		if err := os.WriteFile(filepath.Join(request.OutputRoot, name), data, 0o600); err != nil {
			return nil, err
		}
		if err := events.Completed(role); err != nil {
			return nil, err
		}
		pair = append(pair, model.MediaArtifactInput{Role: role, JobID: job, StorageRoot: request.OutputRoot, RelativePath: name, Size: int64(len(data)), ContentType: contentType})
	}
	return pair, nil
}

func TestQueueBoundedWorkersAndPersistedWaitingWork(t *testing.T) {
	root := queueDatabase(t)
	started, release := make(chan struct{}, 4), make(chan struct{})
	q, err := media.NewQueue(2, func(ctx context.Context, request media.Request, events media.ModeEvents) ([]model.MediaArtifactInput, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
		}
		return fakeRunner(ctx, request, events)
	}, func(*model.MediaSource, media.Request) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	var intents []*media.Intent
	for n := range 4 {
		intent := reservation(t, media.Request{URL: string(rune('a' + n)), OutputRoot: root})
		if _, err := q.Accept(intent); err != nil {
			t.Fatal(err)
		}
		intents = append(intents, intent)
	}
	receiveStart(t, started)
	receiveStart(t, started)
	select {
	case <-started:
		t.Fatal("dispatcher exceeded its worker limit")
	default:
	}
	var queued int64
	if err := model.DB.Model(&model.MediaAttempt{}).Where("state = ?", model.MediaQueued).Count(&queued).Error; err != nil || queued != 2 {
		t.Fatalf("waiting work was not durably queued: %d, %v", queued, err)
	}
	close(release)
	for _, intent := range intents {
		awaitAttempt(t, intent.AttemptID, model.MediaReady)
	}
}

func TestQueueCoalescesOnlySameSourceOwnerAndOptions(t *testing.T) {
	root := queueDatabase(t)
	started, release := make(chan struct{}, 3), make(chan struct{})
	q, err := media.NewQueue(2, func(ctx context.Context, request media.Request, events media.ModeEvents) ([]model.MediaArtifactInput, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
		}
		return fakeRunner(ctx, request, events)
	}, func(*model.MediaSource, media.Request) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	request := media.Request{URL: "source", UserID: 11, OutputRoot: root, Backend: "auto"}
	first := reservation(t, request)
	accepted, err := q.Accept(first)
	if err != nil {
		t.Fatal(err)
	}
	receiveStart(t, started)
	if replayed, err := q.Accept(first); err != nil || replayed.ID != accepted.ID {
		t.Fatalf("acceptance was not idempotent: %#v, %v", replayed, err)
	}
	joined := reservation(t, request)
	reused, err := q.Accept(joined)
	if err != nil || reused.ID != accepted.ID {
		t.Fatalf("compatible work did not report its real attempt: %#v, %v", reused, err)
	}
	if got := getAttempt(t, joined.AttemptID); got.ErrorCode != "coalesced" || got.JoinedAttemptID != accepted.ID || got.State == model.MediaQueued {
		t.Fatalf("coalescing fabricated another queue entry: %#v", got)
	}
	latest, err := model.LatestMediaAttempt(first.SourceID)
	if err != nil || latest.ID != accepted.ID {
		t.Fatalf("coalesced reservation displaced actual status: %#v, %v", latest, err)
	}
	request.Backend = "yt-dlp"
	changed := reservation(t, request)
	if other, err := q.Accept(changed); err != nil || other.ID == accepted.ID {
		t.Fatalf("different options reused work: %#v, %v", other, err)
	}
	request.UserID = 22
	otherOwner := reservation(t, request)
	if other, err := q.Accept(otherOwner); err != nil || other.SourceID == accepted.SourceID {
		t.Fatalf("another owner reused work: %#v, %v", other, err)
	}
	close(release)
	for _, intent := range []*media.Intent{first, changed, otherOwner} {
		awaitAttempt(t, intent.AttemptID, model.MediaReady)
	}
}

func TestQueueCoalescedLaterIntentPreservesSuccessfulSelectionOrdering(t *testing.T) {
	root := queueDatabase(t)
	started := make(chan struct{}, 2)
	releaseA, releaseB := make(chan struct{}), make(chan struct{})
	q, err := media.NewQueue(2, func(ctx context.Context, request media.Request, events media.ModeEvents) ([]model.MediaArtifactInput, error) {
		started <- struct{}{}
		release := releaseA
		if request.Backend == "yt-dlp" {
			release = releaseB
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
		}
		return fakeRunner(ctx, request, events)
	}, func(*model.MediaSource, media.Request) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	request := media.Request{URL: "source", UserID: 11, OutputRoot: root, Backend: "auto"}
	first := reservation(t, request)
	if _, err := q.Accept(first); err != nil {
		t.Fatal(err)
	}
	receiveStart(t, started)
	request.Backend = "yt-dlp"
	second := reservation(t, request)
	if _, err := q.Accept(second); err != nil {
		t.Fatal(err)
	}
	receiveStart(t, started)
	request.Backend = "auto"
	third := reservation(t, request)
	if joined, err := q.Accept(third); err != nil || joined.ID != first.AttemptID {
		t.Fatalf("later compatible request did not reuse real work: %#v, %v", joined, err)
	}
	close(releaseB)
	awaitAttempt(t, second.AttemptID, model.MediaReady)
	close(releaseA)
	awaitAttempt(t, first.AttemptID, model.MediaReady)
	source, err := model.GetMediaSourceByID(first.SourceID)
	if err != nil || source.AcceptedGeneration != 3 {
		t.Fatalf("coalescing discarded the later successful intent's ordering: %#v, %v", source, err)
	}
	status, err := media.ReadStatus(first.DocumentID, request.UserID)
	if err != nil || status.Attempt.ID != first.AttemptID || status.Attempt.Generation != 3 {
		t.Fatalf("coalescing displaced real latest status: %#v, %v", status, err)
	}
}

func TestQueueCancelAndShutdownFencePublication(t *testing.T) {
	for _, stop := range []string{"cancel", "shutdown", "delete"} {
		t.Run(stop, func(t *testing.T) {
			root := queueDatabase(t)
			started := make(chan struct{})
			q, err := media.NewQueue(1, func(ctx context.Context, _ media.Request, _ media.ModeEvents) ([]model.MediaArtifactInput, error) {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			}, func(*model.MediaSource, media.Request) bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(q.Close)
			intent := reservation(t, media.Request{URL: "source", OutputRoot: root})
			if _, err := q.Accept(intent); err != nil {
				t.Fatal(err)
			}
			receiveStart(t, started)
			state := model.MediaCancelled
			switch stop {
			case "cancel":
				if err := q.CancelAttempt(intent.AttemptID); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				q.Close()
				state = model.MediaInterrupted
			case "delete":
				if err := model.RevokeMediaSource(intent.SourceID); err != nil {
					t.Fatal(err)
				}
				q.CancelSource(intent.SourceID)
			}
			awaitAttempt(t, intent.AttemptID, state)
			var count int64
			if err := model.DB.Model(&model.MediaArtifact{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("cancelled worker published files: %d, %v", count, err)
			}
		})
	}
}

func TestQueueRestartDispatchesQueuedButNotInterruptedOrUncommittedWork(t *testing.T) {
	root := queueDatabase(t)
	started := make(chan struct{})
	q, err := media.NewQueue(1, func(ctx context.Context, _ media.Request, _ media.ModeEvents) ([]model.MediaArtifactInput, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}, func(*model.MediaSource, media.Request) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	active := reservation(t, media.Request{URL: "active", OutputRoot: root})
	if _, err := q.Accept(active); err != nil {
		t.Fatal(err)
	}
	receiveStart(t, started)
	queued := reservation(t, media.Request{URL: "queued", OutputRoot: root})
	if _, err := q.Accept(queued); err != nil {
		t.Fatal(err)
	}
	pending := reservation(t, media.Request{URL: "uncommitted", OutputRoot: root})
	q.Close()
	resumed, err := media.NewQueue(1, fakeRunner, func(*model.MediaSource, media.Request) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resumed.Close)
	awaitAttempt(t, active.AttemptID, model.MediaInterrupted)
	awaitAttempt(t, pending.AttemptID, model.MediaInterrupted)
	awaitAttempt(t, queued.AttemptID, model.MediaReady)
}

func TestQueuePublicationFailureRemainsFailedNotReady(t *testing.T) {
	root := queueDatabase(t)
	q, err := media.NewQueue(1, fakeRunner, func(*model.MediaSource, media.Request) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	if err := model.DB.Exec(`CREATE TRIGGER reject_artifact BEFORE INSERT ON media_artifacts BEGIN SELECT RAISE(ABORT, 'injected publication failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	intent := reservation(t, media.Request{URL: "source", OutputRoot: root})
	if _, err := q.Accept(intent); err != nil {
		t.Fatal(err)
	}
	failed := awaitAttempt(t, intent.AttemptID, model.MediaFailed)
	if failed.ErrorCode != "publication_failed" {
		t.Fatalf("failure status = %#v", failed)
	}
	artifacts, err := model.CurrentMediaArtifacts(intent.SourceID)
	if err != nil || len(artifacts) != 0 {
		t.Fatalf("failed publication exposed artifacts: %#v, %v", artifacts, err)
	}
}

func TestOpenArtifactRejectsSymlinksIncludingOtherJobsInsideRoot(t *testing.T) {
	root := queueDatabase(t)
	data := []byte("known bytes")
	path := testutil.WriteFile(t, root, "clip.webm", data)
	candidate := model.MediaArtifactInput{StorageRoot: root, RelativePath: "clip.webm", Size: int64(len(data))}
	file, _, err := media.OpenArtifact(candidate)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for _, foreign := range []string{testutil.WriteFile(t, root, "other-job.webm", data), testutil.WriteFile(t, t.TempDir(), "outside.webm", data)} {
		if err := os.Symlink(foreign, path); err != nil {
			t.Fatal(err)
		}
		if file, _, err := media.OpenArtifact(candidate); err == nil {
			_ = file.Close()
			t.Fatal("symlink became authorized media")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	foreignDir := t.TempDir()
	testutil.WriteFile(t, foreignDir, "clip.webm", data)
	if err := os.Symlink(foreignDir, filepath.Join(root, "job")); err != nil {
		t.Fatal(err)
	}
	candidate.RelativePath = "job/clip.webm"
	if file, _, err := media.OpenArtifact(candidate); err == nil {
		_ = file.Close()
		t.Fatal("symlinked job directory became authorized")
	}
	candidate.RelativePath = "../outside.webm"
	if _, _, err := media.OpenArtifact(candidate); !errors.Is(err, media.ErrInvalidOutput) {
		t.Fatalf("path escape error = %v", err)
	}
}
