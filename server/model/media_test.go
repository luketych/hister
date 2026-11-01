// SPDX-License-Identifier: AGPL-3.0-or-later

package model_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"

	"gorm.io/gorm"
)

func mediaSource(t *testing.T) *model.MediaSource {
	t.Helper()
	testutil.InitModel(t)
	// Serialize SQLite connections, not downloads. Concurrent callers below
	// still exercise the transactional generation/idempotency boundary.
	db, err := model.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	source, err := model.EnsureMediaSource("11:https://www.youtube.com/watch?v=abcdefghijk", 11)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func acceptMedia(t *testing.T, sourceID, requestID string) *model.MediaAttempt {
	t.Helper()
	attempt, err := model.AcceptMediaAttempt(sourceID, requestID, `{"backend":"yt-dlp"}`)
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}

func finalizeMedia(t *testing.T, attempt *model.MediaAttempt) *model.MediaAttempt {
	t.Helper()
	claimed, err := model.ClaimMediaAttempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != model.MediaStarting || claimed.ClaimToken == "" {
		t.Fatalf("claim is not a pending process start: %#v", claimed)
	}
	for _, role := range []string{model.MediaVideo, model.MediaAudio} {
		if err := model.StartMediaMode(claimed.ID, claimed.ClaimToken, role); err != nil {
			t.Fatal(err)
		}
		if err := model.CompleteMediaMode(claimed.ID, claimed.ClaimToken, role); err != nil {
			t.Fatal(err)
		}
	}
	return claimed
}

func mediaPair(attempt *model.MediaAttempt) []model.MediaArtifactInput {
	return []model.MediaArtifactInput{
		{Role: model.MediaVideo, JobID: attempt.ID + "-video", StorageRoot: "/media", RelativePath: attempt.ID + "/video.webm", Size: 100, ContentType: "video/webm"},
		{Role: model.MediaAudio, JobID: attempt.ID + "-audio", StorageRoot: "/media", RelativePath: attempt.ID + "/audio.mp3", Size: 50, ContentType: "audio/mpeg"},
	}
}

func publishMedia(t *testing.T, attempt *model.MediaAttempt) *model.MediaSet {
	t.Helper()
	set, err := model.PublishMediaPair(attempt.ID, attempt.ClaimToken, mediaPair(attempt))
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func storedMediaAttempt(t *testing.T, id string) *model.MediaAttempt {
	t.Helper()
	var attempt model.MediaAttempt
	if err := model.DB.First(&attempt, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	return &attempt
}

func currentMedia(t *testing.T, sourceID string) []model.MediaArtifact {
	t.Helper()
	artifacts, err := model.CurrentMediaArtifacts(sourceID)
	if err != nil {
		t.Fatal(err)
	}
	return artifacts
}

func TestMediaRegistryReadinessAndFailedRefresh(t *testing.T) {
	source := mediaSource(t)
	attempt := acceptMedia(t, source.ID, "initial")
	if attempt.State != model.MediaQueued || attempt.VideoState != model.MediaQueued || attempt.AudioState != model.MediaQueued {
		t.Fatalf("accepted attempt = %#v", attempt)
	}
	if got := currentMedia(t, source.ID); len(got) != 0 {
		t.Fatalf("queued attempt exposed artifacts: %#v", got)
	}
	claimed, err := model.ClaimMediaAttempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.ClaimMediaAttempt(attempt.ID); !errors.Is(err, model.ErrMediaConflict) {
		t.Fatalf("second claim = %v", err)
	}
	if err := model.StartMediaMode(attempt.ID, "wrong-worker", model.MediaVideo); !errors.Is(err, model.ErrMediaConflict) {
		t.Fatalf("foreign claim = %v", err)
	}
	if err := model.StartMediaMode(attempt.ID, claimed.ClaimToken, model.MediaVideo); err != nil {
		t.Fatal(err)
	}
	if err := model.CompleteMediaMode(attempt.ID, claimed.ClaimToken, model.MediaVideo); err != nil {
		t.Fatal(err)
	}
	waiting := storedMediaAttempt(t, attempt.ID)
	if waiting.VideoState != model.MediaDownloaded || waiting.AudioState != model.MediaQueued || waiting.State != model.MediaDownloading {
		t.Fatalf("finished video must wait for audio: %#v", waiting)
	}
	if _, err := model.PublishMediaPair(attempt.ID, claimed.ClaimToken, mediaPair(claimed)); !errors.Is(err, model.ErrMediaConflict) {
		t.Fatalf("partial pair publication = %v", err)
	}
	if err := model.StartMediaMode(attempt.ID, claimed.ClaimToken, model.MediaAudio); err != nil {
		t.Fatal(err)
	}
	if err := model.CompleteMediaMode(attempt.ID, claimed.ClaimToken, model.MediaAudio); err != nil {
		t.Fatal(err)
	}
	if got := storedMediaAttempt(t, attempt.ID); got.State != model.MediaFinalizing {
		t.Fatalf("unpublished pair state = %q", got.State)
	}
	if got := currentMedia(t, source.ID); len(got) != 0 {
		t.Fatalf("finalizing attempt exposed artifacts: %#v", got)
	}
	set := publishMedia(t, claimed)
	available := currentMedia(t, source.ID)
	if len(available) != 2 || available[0].SetID != set.ID || storedMediaAttempt(t, attempt.ID).State != model.MediaReady {
		t.Fatalf("committed pair not ready: %#v", available)
	}

	refresh := acceptMedia(t, source.ID, "refresh")
	worker, err := model.ClaimMediaAttempt(refresh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.FailMediaAttempt(refresh.ID, worker.ClaimToken, "download_failed"); err != nil {
		t.Fatal(err)
	}
	if got := currentMedia(t, source.ID); len(got) != 2 || got[0].SetID != set.ID {
		t.Fatalf("failed refresh lost the previous pair: %#v", got)
	}
	if got := storedMediaAttempt(t, refresh.ID); got.State != model.MediaFailed || got.ErrorCode != "download_failed" {
		t.Fatalf("failed attempt = %#v", got)
	}
}

func TestMediaRegistryLateCompletionRetainsHistoryWithoutRegressingSelection(t *testing.T) {
	source := mediaSource(t)
	older := finalizeMedia(t, acceptMedia(t, source.ID, "older"))
	newer := finalizeMedia(t, acceptMedia(t, source.ID, "newer"))
	// Force the older worker to publish after the newer publication, without
	// sleeps or depending on which download finishes first.
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-release
		_, err := model.PublishMediaPair(older.ID, older.ClaimToken, mediaPair(older))
		done <- err
	}()
	newerSet, newerErr := model.PublishMediaPair(newer.ID, newer.ClaimToken, mediaPair(newer))
	close(release)
	olderErr := <-done
	if newerErr != nil || olderErr != nil {
		t.Fatalf("publication errors: newer=%v, older=%v", newerErr, olderErr)
	}
	if got := currentMedia(t, source.ID); len(got) != 2 || got[0].SetID != newerSet.ID {
		t.Fatalf("late result regressed current selection: %#v", got)
	}
	var artifacts []model.MediaArtifact
	if err := model.DB.Find(&artifacts).Error; err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 4 {
		t.Fatalf("historical artifacts = %d, want 4", len(artifacts))
	}
	for _, artifact := range artifacts {
		if _, err := model.LookupMediaArtifact(artifact.ID, source.UserID, source.DocumentID); err != nil {
			t.Fatalf("historical binding revoked on refresh: %v", err)
		}
		for _, scope := range []struct {
			userID uint
			docID  string
		}{{22, source.DocumentID}, {source.UserID, "another-document"}} {
			if _, err := model.LookupMediaArtifact(artifact.ID, scope.userID, scope.docID); !errors.Is(err, gorm.ErrRecordNotFound) {
				t.Fatalf("artifact escaped its source/owner binding: %v", err)
			}
		}
	}
}

func TestMediaRegistryNewerFailureDoesNotBlockOlderSuccess(t *testing.T) {
	source := mediaSource(t)
	older := finalizeMedia(t, acceptMedia(t, source.ID, "older"))
	newer := acceptMedia(t, source.ID, "newer")
	worker, err := model.ClaimMediaAttempt(newer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.FailMediaAttempt(worker.ID, worker.ClaimToken, "download_failed"); err != nil {
		t.Fatal(err)
	}
	set := publishMedia(t, older)
	if got := currentMedia(t, source.ID); len(got) != 2 || got[0].SetID != set.ID {
		t.Fatalf("newer failed request blocked usable older success: %#v", got)
	}
}

func TestMediaRegistryIdempotencyAndInvalidTransitions(t *testing.T) {
	source := mediaSource(t)
	again, err := model.EnsureMediaSource(source.DocumentID, source.UserID)
	if err != nil || again.ID != source.ID {
		t.Fatalf("source lookup created a new incarnation: %#v, %v", again, err)
	}
	attempt := acceptMedia(t, source.ID, "request")
	repeated := acceptMedia(t, source.ID, "request")
	if repeated.ID != attempt.ID || repeated.Generation != attempt.Generation {
		t.Fatal("duplicate acceptance allocated new work")
	}
	if _, err := model.AcceptMediaAttempt(source.ID, "request", `{"backend":"changed"}`); !errors.Is(err, model.ErrMediaConflict) {
		t.Fatalf("request ID reused with different options = %v", err)
	}
	if _, err := model.AcceptMediaAttempt(source.ID, "invalid", "not json"); err == nil {
		t.Fatal("invalid persisted request accepted")
	}
	if err := model.CompleteMediaMode(attempt.ID, "", model.MediaVideo); !errors.Is(err, model.ErrMediaConflict) {
		t.Fatalf("queued mode completed without a process = %v", err)
	}
	worker := finalizeMedia(t, attempt)
	if err := model.StartMediaMode(attempt.ID, worker.ClaimToken, model.MediaVideo); !errors.Is(err, model.ErrMediaConflict) {
		t.Fatalf("downloaded mode restarted = %v", err)
	}
	if err := model.FailMediaAttempt(attempt.ID, worker.ClaimToken, "/private/path: secret error"); err == nil {
		t.Fatal("raw diagnostics accepted as public error code")
	}
	set := publishMedia(t, worker)
	replayed := publishMedia(t, worker)
	if replayed.ID != set.ID {
		t.Fatal("replayed publication created a second set")
	}
	changed := mediaPair(worker)
	changed[0].RelativePath = "another-file.webm"
	if _, err := model.PublishMediaPair(worker.ID, worker.ClaimToken, changed); !errors.Is(err, model.ErrMediaConflict) {
		t.Fatalf("replayed publication changed artifact binding = %v", err)
	}
	if err := model.CancelMediaAttempt(worker.ID); !errors.Is(err, model.ErrMediaConflict) {
		t.Fatalf("completed attempt cancelled = %v", err)
	}
}

func TestMediaRegistryRejectsUnsafeArtifactRecords(t *testing.T) {
	source := mediaSource(t)
	worker := finalizeMedia(t, acceptMedia(t, source.ID, "request"))
	for name, mutate := range map[string]func([]model.MediaArtifactInput) []model.MediaArtifactInput{
		"incomplete pair": func(p []model.MediaArtifactInput) []model.MediaArtifactInput { return p[:1] },
		"duplicate role":  func(p []model.MediaArtifactInput) []model.MediaArtifactInput { p[1].Role = p[0].Role; return p },
		"shared job":      func(p []model.MediaArtifactInput) []model.MediaArtifactInput { p[1].JobID = p[0].JobID; return p },
		"escape": func(p []model.MediaArtifactInput) []model.MediaArtifactInput {
			p[0].RelativePath = "../foreign.webm"
			return p
		},
		"absolute": func(p []model.MediaArtifactInput) []model.MediaArtifactInput {
			p[0].RelativePath = "/foreign.webm"
			return p
		},
		"unclean": func(p []model.MediaArtifactInput) []model.MediaArtifactInput {
			p[0].RelativePath = "job/../foreign.webm"
			return p
		},
		"directory": func(p []model.MediaArtifactInput) []model.MediaArtifactInput {
			p[0].RelativePath = "."
			return p
		},
		"disguised report": func(p []model.MediaArtifactInput) []model.MediaArtifactInput {
			p[0].RelativePath = "job/report.json"
			return p
		},
		"relative root": func(p []model.MediaArtifactInput) []model.MediaArtifactInput { p[0].StorageRoot = "media"; return p },
		"empty file":    func(p []model.MediaArtifactInput) []model.MediaArtifactInput { p[0].Size = 0; return p },
		"report": func(p []model.MediaArtifactInput) []model.MediaArtifactInput {
			p[0].ContentType = "application/json"
			return p
		},
		"html": func(p []model.MediaArtifactInput) []model.MediaArtifactInput {
			p[0].ContentType = "text/html"
			return p
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := model.PublishMediaPair(worker.ID, worker.ClaimToken, mutate(mediaPair(worker))); err == nil {
				t.Fatal("unsafe pair accepted")
			}
			if got := currentMedia(t, source.ID); len(got) != 0 {
				t.Fatalf("invalid publication exposed files: %#v", got)
			}
		})
	}
}

func TestMediaRegistryPublicationRollsBackAsOneUnit(t *testing.T) {
	source := mediaSource(t)
	previous := publishMedia(t, finalizeMedia(t, acceptMedia(t, source.ID, "previous")))
	worker := finalizeMedia(t, acceptMedia(t, source.ID, "new"))
	if err := model.DB.Exec(`CREATE TRIGGER reject_audio BEFORE INSERT ON media_artifacts
		WHEN NEW.role = 'audio' BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := model.PublishMediaPair(worker.ID, worker.ClaimToken, mediaPair(worker)); err == nil {
		t.Fatal("injected publication failure was ignored")
	}
	if got := currentMedia(t, source.ID); len(got) != 2 || got[0].SetID != previous.ID {
		t.Fatalf("partial publication replaced old selection: %#v", got)
	}
	if got := storedMediaAttempt(t, worker.ID); got.State != model.MediaFinalizing {
		t.Fatalf("failed commit claimed readiness: %#v", got)
	}
	var count int64
	if err := model.DB.Model(&model.MediaArtifact{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("uncommitted artifacts leaked: count=%d, error=%v", count, err)
	}
	if err := model.DB.Model(&model.MediaSet{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("uncommitted set leaked: count=%d, error=%v", count, err)
	}
	if err := model.DB.Exec("DROP TRIGGER reject_audio").Error; err != nil {
		t.Fatal(err)
	}
	publishMedia(t, worker)
}

func TestMediaRegistryTransferDeletionAndReAdd(t *testing.T) {
	source := mediaSource(t)
	publishMedia(t, finalizeMedia(t, acceptMedia(t, source.ID, "previous")))
	artifact := currentMedia(t, source.ID)[0]
	pending := finalizeMedia(t, acceptMedia(t, source.ID, "pending"))
	const newDocID = "22:https://www.youtube.com/watch?v=abcdefghijk"
	if err := model.TransferMediaSource(source.ID, newDocID, 22); err != nil {
		t.Fatal(err)
	}
	if _, err := model.LookupMediaArtifact(artifact.ID, source.UserID, source.DocumentID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("old owner retained access: %v", err)
	}
	if _, err := model.LookupMediaArtifact(artifact.ID, 22, newDocID); err != nil {
		t.Fatalf("new owner lost retained media: %v", err)
	}
	if _, err := model.PublishMediaPair(pending.ID, pending.ClaimToken, mediaPair(pending)); !errors.Is(err, model.ErrMediaConflict) {
		t.Fatalf("old-owner worker published after transfer: %v", err)
	}
	if err := model.RevokeMediaSource(source.ID); err != nil {
		t.Fatal(err)
	}
	if err := model.RevokeMediaSource(source.ID); err != nil {
		t.Fatalf("repeat revocation = %v", err)
	}
	if _, err := model.LookupMediaArtifact(artifact.ID, 22, newDocID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("deleted source retained playback binding: %v", err)
	}
	if _, err := model.AcceptMediaAttempt(source.ID, "after-delete", `{}`); !errors.Is(err, model.ErrMediaSourceRevoked) {
		t.Fatalf("deleted incarnation accepted new work: %v", err)
	}
	readded, err := model.EnsureMediaSource(newDocID, 22)
	if err != nil || readded.ID == source.ID {
		t.Fatalf("re-add revived the old incarnation: %#v, %v", readded, err)
	}
	if got := currentMedia(t, readded.ID); len(got) != 0 {
		t.Fatalf("re-added document inherited revoked media: %#v", got)
	}
	if _, err := model.LookupMediaArtifact(artifact.ID, 22, newDocID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("re-add revived an old artifact binding: %v", err)
	}
}

func TestMediaRegistryRevocationInvalidatesUnpublishedWork(t *testing.T) {
	source := mediaSource(t)
	reserved, err := model.ReserveMediaAttempt(source.ID, "uncommitted", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	queued := acceptMedia(t, source.ID, "queued")
	finishing := finalizeMedia(t, acceptMedia(t, source.ID, "finishing"))
	if err := model.RevokeMediaSource(source.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{reserved.ID, queued.ID, finishing.ID} {
		got := storedMediaAttempt(t, id)
		if got.State != model.MediaCancelled || got.ClaimToken != "" || got.ErrorCode != "source_deleted" {
			t.Fatalf("source deletion retained incompatible work: %#v", got)
		}
	}
	if _, err := model.ClaimMediaAttempt(queued.ID); !errors.Is(err, model.ErrMediaSourceRevoked) {
		t.Fatalf("worker claimed work for deleted source: %v", err)
	}
	if _, err := model.PublishMediaPair(finishing.ID, finishing.ClaimToken, mediaPair(finishing)); !errors.Is(err, model.ErrMediaSourceRevoked) {
		t.Fatalf("worker published after source deletion: %v", err)
	}
	var count int64
	if err := model.DB.Model(&model.MediaArtifact{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("revoked publication leaked artifacts: count=%d, error=%v", count, err)
	}
}

func TestMediaRegistryConcurrentAcceptance(t *testing.T) {
	source := mediaSource(t)
	const workers = 8
	var wg sync.WaitGroup
	results := make(chan *model.MediaAttempt, workers)
	errorsCh := make(chan error, workers)
	for n := range workers {
		wg.Go(func() {
			attempt, err := model.AcceptMediaAttempt(source.ID, fmt.Sprintf("request-%d", n), `{}`)
			if err != nil {
				errorsCh <- err
				return
			}
			results <- attempt
		})
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	seen := make(map[uint64]bool)
	for attempt := range results {
		if seen[attempt.Generation] || attempt.Generation == 0 {
			t.Fatalf("generation allocated twice: %d", attempt.Generation)
		}
		seen[attempt.Generation] = true
	}
	if len(seen) != workers {
		t.Fatalf("allocated %d generations, want %d", len(seen), workers)
	}
}

func TestMediaRegistryReservationIsNotQueuedAndKeepsRequestOrdering(t *testing.T) {
	source := mediaSource(t)
	older, err := model.ReserveMediaAttempt(source.ID, "older", `{"backend":"yt-dlp"}`)
	if err != nil {
		t.Fatal(err)
	}
	if older.State != model.MediaPendingCommit {
		t.Fatalf("reservation misrepresented accepted work: %#v", older)
	}
	if _, err := model.ClaimMediaAttempt(older.ID); !errors.Is(err, model.ErrMediaConflict) {
		t.Fatalf("worker claimed an uncommitted document: %v", err)
	}
	newer := finalizeMedia(t, acceptMedia(t, source.ID, "newer"))
	newerSet := publishMedia(t, newer)
	// A's later content extraction / batch save must not allocate a new
	// generation at acceptance, after B has already been committed.
	accepted := acceptMedia(t, source.ID, "older")
	if accepted.Generation != older.Generation || accepted.Generation >= newer.Generation {
		t.Fatalf("late acceptance changed reserved ordering: %#v", accepted)
	}
	publishMedia(t, finalizeMedia(t, accepted))
	if got := currentMedia(t, source.ID); len(got) != 2 || got[0].SetID != newerSet.ID {
		t.Fatalf("late document acceptance regressed media selection: %#v", got)
	}
}

func TestMediaRegistryConcurrentDuplicateAcceptance(t *testing.T) {
	source := mediaSource(t)
	const workers = 8
	var wg sync.WaitGroup
	results := make(chan *model.MediaAttempt, workers)
	for range workers {
		wg.Go(func() {
			attempt, err := model.AcceptMediaAttempt(source.ID, "same-request", `{}`)
			if err != nil {
				t.Error(err)
				return
			}
			results <- attempt
		})
	}
	wg.Wait()
	close(results)
	var id string
	for attempt := range results {
		if id == "" {
			id = attempt.ID
		}
		if attempt.ID != id || attempt.Generation != 1 {
			t.Fatalf("duplicate acceptance created independent work: %#v", attempt)
		}
	}
}

func TestMediaRegistryOwnershipConflictRollsBackCancellation(t *testing.T) {
	source := mediaSource(t)
	publishMedia(t, finalizeMedia(t, acceptMedia(t, source.ID, "previous")))
	artifact := currentMedia(t, source.ID)[0]
	pending := acceptMedia(t, source.ID, "pending")
	target, err := model.EnsureMediaSource("22:another-source", 22)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.TransferMediaSource(source.ID, target.DocumentID, target.UserID); err == nil {
		t.Fatal("transfer silently merged two live source identities")
	}
	if got := storedMediaAttempt(t, pending.ID); got.State != model.MediaQueued {
		t.Fatalf("failed transfer cancelled accepted work: %#v", got)
	}
	if _, err := model.LookupMediaArtifact(artifact.ID, source.UserID, source.DocumentID); err != nil {
		t.Fatalf("failed transfer revoked the old owner: %v", err)
	}
	if _, err := model.LookupMediaArtifact(artifact.ID, target.UserID, target.DocumentID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("failed transfer granted the target access: %v", err)
	}
}

func TestMediaRegistryProducerOutputCannotBeRebound(t *testing.T) {
	source := mediaSource(t)
	original := finalizeMedia(t, acceptMedia(t, source.ID, "original"))
	publishMedia(t, original)
	other, err := model.EnsureMediaSource("22:another-source", 22)
	if err != nil {
		t.Fatal(err)
	}
	worker := finalizeMedia(t, acceptMedia(t, other.ID, "foreign"))
	if _, err := model.PublishMediaPair(worker.ID, worker.ClaimToken, mediaPair(original)); err == nil {
		t.Fatal("registered producer job outputs acquired a second owner")
	}
	if got := currentMedia(t, other.ID); len(got) != 0 {
		t.Fatalf("failed output registration leaked artifacts: %#v", got)
	}
}

func TestMediaRegistryRetiredArtifactIsUnavailableNotQueued(t *testing.T) {
	source := mediaSource(t)
	worker := finalizeMedia(t, acceptMedia(t, source.ID, "original"))
	publishMedia(t, worker)
	artifact := currentMedia(t, source.ID)[0]
	if err := model.DB.Model(&artifact).Update("retired_at", time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := model.LookupMediaArtifact(artifact.ID, source.UserID, source.DocumentID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("retired artifact remained eligible for playback: %v", err)
	}
	if record, err := model.LookupMediaArtifactRecord(artifact.ID, source.UserID, source.DocumentID); err != nil || record.RetiredAt == nil {
		t.Fatalf("owner lost retirement diagnostics: %#v, %v", record, err)
	}
	if _, err := model.LookupMediaArtifactRecord(artifact.ID, source.UserID+1, source.DocumentID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("retirement diagnostics bypassed owner scope: %v", err)
	}
	if _, err := model.LookupMediaArtifactRecord(artifact.ID, source.UserID, "another-document"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("retirement diagnostics bypassed document scope: %v", err)
	}
	if got := storedMediaAttempt(t, worker.ID); got.State != model.MediaReady {
		t.Fatalf("retirement invented work or rewrote download history: %#v", got)
	}
}

func closeMediaDB(t *testing.T) {
	t.Helper()
	db, err := model.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMediaRegistryRestartRecovery(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.Server.Database = "registry.sqlite3"
	testutil.InitModelWithConfig(t, cfg)
	source, err := model.EnsureMediaSource("https://www.youtube.com/watch?v=abcdefghijk", 0)
	if err != nil {
		t.Fatal(err)
	}
	published := finalizeMedia(t, acceptMedia(t, source.ID, "ready"))
	publishMedia(t, published)
	before := currentMedia(t, source.ID)
	queued := acceptMedia(t, source.ID, "queued")
	starting := acceptMedia(t, source.ID, "starting")
	if _, err := model.ClaimMediaAttempt(starting.ID); err != nil {
		t.Fatal(err)
	}
	downloading, err := model.ClaimMediaAttempt(acceptMedia(t, source.ID, "downloading").ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.StartMediaMode(downloading.ID, downloading.ClaimToken, model.MediaVideo); err != nil {
		t.Fatal(err)
	}
	finalizing := finalizeMedia(t, acceptMedia(t, source.ID, "finalizing"))
	reserved, err := model.ReserveMediaAttempt(source.ID, "uncommitted", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	closeMediaDB(t)
	testutil.InitModelWithConfig(t, cfg)
	if err := model.InterruptMediaAttempts(); err != nil {
		t.Fatal(err)
	}
	if err := model.InterruptMediaAttempts(); err != nil {
		t.Fatalf("repeat recovery: %v", err)
	}
	for _, id := range []string{starting.ID, downloading.ID, finalizing.ID, reserved.ID} {
		got := storedMediaAttempt(t, id)
		if got.State != model.MediaInterrupted || got.ClaimToken != "" || got.ErrorCode != "interrupted" {
			t.Fatalf("abandoned work was not interrupted: %#v", got)
		}
	}
	if got := storedMediaAttempt(t, queued.ID); got.State != model.MediaQueued {
		t.Fatalf("unclaimed work was lost: %#v", got)
	}
	if _, err := model.ClaimMediaAttempt(queued.ID); err != nil {
		t.Fatalf("persisted queued work cannot be dispatched: %v", err)
	}
	if _, err := model.PublishMediaPair(finalizing.ID, finalizing.ClaimToken, mediaPair(finalizing)); !errors.Is(err, model.ErrMediaConflict) {
		t.Fatalf("abandoned worker published after restart: %v", err)
	}
	if got := currentMedia(t, source.ID); len(got) != 2 || got[0].ID != before[0].ID || got[1].ID != before[1].ID {
		t.Fatalf("restart lost committed media identities: %#v", got)
	}
	for _, artifact := range before {
		if _, err := model.LookupMediaArtifact(artifact.ID, 0, source.DocumentID); err != nil {
			t.Fatalf("retained binding did not survive reopening: %v", err)
		}
	}
}

func TestMediaRegistryConcurrentFileDatabaseWriters(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.Server.Database = "concurrent.sqlite3"
	testutil.InitModelWithConfig(t, cfg)
	db, err := model.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	source, err := model.EnsureMediaSource("11:source", 11)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	start := make(chan struct{})
	errorsCh := make(chan error, workers)
	run := func(n int) error {
		attempt, err := model.AcceptMediaAttempt(source.ID, fmt.Sprintf("request-%d", n), `{}`)
		if err != nil {
			return err
		}
		worker, err := model.ClaimMediaAttempt(attempt.ID)
		if err != nil {
			return err
		}
		for _, role := range []string{model.MediaVideo, model.MediaAudio} {
			if err := model.StartMediaMode(worker.ID, worker.ClaimToken, role); err != nil {
				return err
			}
			if err := model.CompleteMediaMode(worker.ID, worker.ClaimToken, role); err != nil {
				return err
			}
		}
		_, err = model.PublishMediaPair(worker.ID, worker.ClaimToken, mediaPair(worker))
		return err
	}
	var wg sync.WaitGroup
	for n := range workers {
		wg.Go(func() {
			<-start
			// Separate database connections must serialize only the short SQL
			// operations, not lose generations or double-claim an attempt.
			errorsCh <- run(n)
		})
	}
	close(start)
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Error(err)
		}
	}
	var stored model.MediaSource
	if err := model.DB.First(&stored, "id = ?", source.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.NextGeneration != workers || stored.AcceptedGeneration != workers {
		t.Fatalf("concurrent writers lost/regressed generations: %#v", stored)
	}
	var count int64
	if err := model.DB.Model(&model.MediaArtifact{}).Count(&count).Error; err != nil || count != 2*workers {
		t.Fatalf("concurrent publication lost history: count=%d, error=%v", count, err)
	}
}

func TestMediaRegistryAdditiveMigrationAndReadOnlyInitialization(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.Server.Database = "pre-media.sqlite3"
	testutil.InitModelWithConfig(t, cfg)
	if err := model.CreateCrawlJob("retained", "https://example.com", "{}", "existing data"); err != nil {
		t.Fatal(err)
	}
	// Simulate an existing schema from before the additive registry patch.
	for _, table := range []any{&model.MediaArtifact{}, &model.MediaSet{}, &model.MediaAttempt{}, &model.MediaSource{}} {
		if err := model.DB.Migrator().DropTable(table); err != nil {
			t.Fatal(err)
		}
	}
	closeMediaDB(t)
	if err := model.InitReadOnly(cfg); err != nil {
		t.Fatal(err)
	}
	if model.DB.Migrator().HasTable(&model.MediaSource{}) {
		t.Fatal("read-only initialization created the registry")
	}
	closeMediaDB(t)
	testutil.InitModelWithConfig(t, cfg)
	for _, table := range []any{&model.MediaSource{}, &model.MediaAttempt{}, &model.MediaSet{}, &model.MediaArtifact{}} {
		if !model.DB.Migrator().HasTable(table) {
			t.Fatalf("additive migration omitted %T", table)
		}
	}
	if job, err := model.GetCrawlJob("retained"); err != nil || job == nil || job.Label != "existing data" {
		t.Fatalf("migration changed existing data: %#v, %v", job, err)
	}
	source, err := model.EnsureMediaSource("source", 11)
	if err != nil {
		t.Fatal(err)
	}
	publishMedia(t, finalizeMedia(t, acceptMedia(t, source.ID, "ready")))
	before := currentMedia(t, source.ID)
	closeMediaDB(t)
	testutil.InitModelWithConfig(t, cfg)
	if got := currentMedia(t, source.ID); len(got) != 2 || got[0].ID != before[0].ID {
		t.Fatalf("repeated migration changed the registry: %#v", got)
	}
}
