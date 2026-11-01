// SPDX-License-Identifier: AGPL-3.0-or-later

package model_test

import (
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/asciimoo/hister/server/model"

	"gorm.io/gorm"
)

func failedMediaCommand(t *testing.T, sourceID, requestID string) *model.MediaAttempt {
	t.Helper()
	attempt := acceptMedia(t, sourceID, requestID)
	worker, err := model.ClaimMediaAttempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.FailMediaAttempt(worker.ID, worker.ClaimToken, "download_failed"); err != nil {
		t.Fatal(err)
	}
	return worker
}

func TestMediaCommandRetryReceiptAndTargetedCancellation(t *testing.T) {
	source := mediaSource(t)
	scope := model.MediaScope{DocumentID: source.DocumentID, UserID: source.UserID}
	publishMedia(t, finalizeMedia(t, acceptMedia(t, source.ID, "ready")))
	before := currentMedia(t, source.ID)
	failed := failedMediaCommand(t, source.ID, "failure")
	first, err := model.RetryMediaAttempt(scope, failed.ID, `{"binary":"current"}`)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Attempt.ID == failed.ID || first.Attempt.State != model.MediaQueued || first.Attempt.Generation != 3 || first.Attempt.RequestJSON != `{"binary":"current"}` {
		t.Fatalf("retry = %#v", first)
	}
	if _, err := model.CancelScopedMediaAttempt(scope, first.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		replay, err := model.RetryMediaAttempt(scope, failed.ID, `{"binary":"changed"}`)
		if err != nil || !replay.Replayed || replay.Attempt.ID != first.Attempt.ID || replay.Attempt.State != model.MediaCancelled {
			t.Fatalf("retry receipt restarted cancelled work: %#v, %v", replay, err)
		}
	}
	second, err := model.RetryMediaAttempt(scope, first.Attempt.ID, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.CancelScopedMediaAttempt(scope, first.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	if got := storedMediaAttempt(t, second.Attempt.ID); got.State != model.MediaQueued {
		t.Fatalf("old cancellation affected successor: %#v", got)
	}
	if got := currentMedia(t, source.ID); !reflect.DeepEqual(got, before) {
		t.Fatal("retry/cancel changed the previous selected pair")
	}
}

func TestMediaCommandScopeCheckedBeforeSourceState(t *testing.T) {
	source := mediaSource(t)
	failed := failedMediaCommand(t, source.ID, "failure")
	scope := model.MediaScope{DocumentID: source.DocumentID, UserID: source.UserID}
	wrong := []model.MediaScope{{DocumentID: "another-document", UserID: source.UserID}, {DocumentID: source.DocumentID, UserID: 22}}
	for _, phase := range []string{"live", "suspended", "revoked", "re-added"} {
		switch phase {
		case "suspended":
			if err := model.SuspendMediaTransfer(source.ID, "22:destination", 22); err != nil {
				t.Fatal(err)
			}
		case "revoked":
			if err := model.RevokeMediaSource(source.ID); err != nil {
				t.Fatal(err)
			}
		case "re-added":
			if _, err := model.EnsureMediaSource(source.DocumentID, source.UserID); err != nil {
				t.Fatal(err)
			}
		}
		for _, other := range wrong {
			if _, err := model.RetryMediaAttempt(other, failed.ID, `{}`); !errors.Is(err, gorm.ErrRecordNotFound) {
				t.Fatalf("%s foreign retry revealed source state: %v", phase, err)
			}
			if _, err := model.CancelScopedMediaAttempt(other, failed.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
				t.Fatalf("%s foreign cancel revealed source state: %v", phase, err)
			}
		}
		if phase == "suspended" {
			if _, err := model.RetryMediaAttempt(scope, failed.ID, `{}`); !errors.Is(err, model.ErrMediaConflict) {
				t.Fatalf("suspended source accepted retry: %v", err)
			}
		}
		if phase == "revoked" || phase == "re-added" {
			if _, err := model.RetryMediaAttempt(scope, failed.ID, `{}`); !errors.Is(err, gorm.ErrRecordNotFound) {
				t.Fatalf("deleted incarnation revived retry: %v", err)
			}
		}
	}
}

func TestMediaCommandRetryAcceptanceRollsBackAsOneUnit(t *testing.T) {
	source := mediaSource(t)
	publishMedia(t, finalizeMedia(t, acceptMedia(t, source.ID, "ready")))
	before := currentMedia(t, source.ID)
	failed := failedMediaCommand(t, source.ID, "failure")
	if err := model.DB.Exec(`CREATE TRIGGER reject_retry BEFORE UPDATE OF state ON media_attempts WHEN NEW.state = 'queued' BEGIN SELECT RAISE(ABORT, 'injected acceptance failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	scope := model.MediaScope{DocumentID: source.DocumentID, UserID: source.UserID}
	if _, err := model.RetryMediaAttempt(scope, failed.ID, `{}`); err == nil {
		t.Fatal("retry ignored injected queue acceptance failure")
	}
	var stored model.MediaSource
	if err := model.DB.First(&stored, "id = ?", source.ID).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := model.DB.Model(&model.MediaAttempt{}).Count(&count).Error; err != nil || count != 2 || stored.NextGeneration != 2 {
		t.Fatalf("failed retry left receipt/work/generation: count=%d, generation=%d, error=%v", count, stored.NextGeneration, err)
	}
	if got := currentMedia(t, source.ID); !reflect.DeepEqual(got, before) {
		t.Fatal("failed retry changed current selection")
	}
	if err := model.DB.Exec("DROP TRIGGER reject_retry").Error; err != nil {
		t.Fatal(err)
	}
	if result, err := model.RetryMediaAttempt(scope, failed.ID, `{}`); err != nil || result.Attempt.Generation != 3 || result.Replayed {
		t.Fatalf("retry after failure = %#v, %v", result, err)
	}
}

func TestMediaCommandConcurrentRetryCreatesOneSuccessor(t *testing.T) {
	source := mediaSource(t)
	failed := failedMediaCommand(t, source.ID, "failure")
	scope := model.MediaScope{DocumentID: source.DocumentID, UserID: source.UserID}
	start := make(chan struct{})
	results := make(chan *model.MediaRetryResult, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			<-start
			result, err := model.RetryMediaAttempt(scope, failed.ID, `{}`)
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
		if id != result.Attempt.ID {
			t.Fatal("concurrent retry created duplicate successors")
		}
		if !result.Replayed {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created retry intents = %d", created)
	}
}

func TestMediaCommandStaleOrUnpublishedRetryCannotSchedule(t *testing.T) {
	source := mediaSource(t)
	scope := model.MediaScope{DocumentID: source.DocumentID, UserID: source.UserID}
	failed := failedMediaCommand(t, source.ID, "failure")
	newer := finalizeMedia(t, acceptMedia(t, source.ID, "newer"))
	publishMedia(t, newer)
	for _, id := range []string{failed.ID, newer.ID} {
		if _, err := model.RetryMediaAttempt(scope, id, `{}`); !errors.Is(err, model.ErrMediaConflict) {
			t.Fatalf("stale/ready retry = %v", err)
		}
	}
	pending, err := model.ReserveMediaAttempt(source.ID, "not-committed", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.RetryMediaAttempt(scope, pending.ID, `{}`); !errors.Is(err, model.ErrMediaConflict) {
		t.Fatalf("uncommitted retry = %v", err)
	}
	if _, err := model.CancelScopedMediaAttempt(scope, pending.ID); !errors.Is(err, model.ErrMediaConflict) {
		t.Fatalf("uncommitted cancellation = %v", err)
	}
}

func TestMediaCommandCoalescedRetryReportsActualAttempt(t *testing.T) {
	source := mediaSource(t)
	scope := model.MediaScope{DocumentID: source.DocumentID, UserID: source.UserID}
	older, err := model.AcceptMediaAttempt(source.ID, "older", `{"backend":"current"}`)
	if err != nil {
		t.Fatal(err)
	}
	older, err = model.ClaimMediaAttempt(older.ID)
	if err != nil {
		t.Fatal(err)
	}
	failed := failedMediaCommand(t, source.ID, "failure")
	for n := range 2 {
		result, err := model.RetryMediaAttempt(scope, failed.ID, `{"backend":"current"}`)
		if err != nil || result.Attempt.ID != older.ID || result.Attempt.PublicationGeneration != 3 || result.Replayed != (n > 0) {
			t.Fatalf("coalesced retry = %#v, %v", result, err)
		}
	}
	var receipt model.MediaAttempt
	if err := model.DB.First(&receipt, "request_id = ?", "retry:"+failed.ID).Error; err != nil {
		t.Fatal(err)
	}
	if receipt.JoinedAttemptID != older.ID || receipt.State != model.MediaCancelled {
		t.Fatalf("coalescing invented queued work: %#v", receipt)
	}
	if _, err := model.CancelScopedMediaAttempt(scope, receipt.ID); !errors.Is(err, model.ErrMediaConflict) {
		t.Fatalf("receipt could cancel a fabricated attempt: %v", err)
	}
}

func TestMediaCommandCancelAndPublicationHaveOneWinner(t *testing.T) {
	for _, winner := range []string{"cancel", "publish"} {
		t.Run(winner, func(t *testing.T) {
			source := mediaSource(t)
			scope := model.MediaScope{DocumentID: source.DocumentID, UserID: source.UserID}
			worker := finalizeMedia(t, acceptMedia(t, source.ID, "request"))
			release, done := make(chan struct{}), make(chan error, 1)
			if winner == "cancel" {
				go func() {
					<-release
					_, err := model.PublishMediaPair(worker.ID, worker.ClaimToken, mediaPair(worker))
					done <- err
				}()
				_, err := model.CancelScopedMediaAttempt(scope, worker.ID)
				close(release)
				if loser := <-done; err != nil || !errors.Is(loser, model.ErrMediaConflict) {
					t.Fatalf("cancel winner=%v, publication loser=%v", err, loser)
				}
				if got := currentMedia(t, source.ID); len(got) != 0 {
					t.Fatal("cancelled candidate became available")
				}
			} else {
				go func() { <-release; _, err := model.CancelScopedMediaAttempt(scope, worker.ID); done <- err }()
				_, err := model.PublishMediaPair(worker.ID, worker.ClaimToken, mediaPair(worker))
				close(release)
				if loser := <-done; err != nil || !errors.Is(loser, model.ErrMediaConflict) {
					t.Fatalf("publication winner=%v, cancel loser=%v", err, loser)
				}
				if got := storedMediaAttempt(t, worker.ID); got.State != model.MediaReady {
					t.Fatal("late cancellation invalidated published artifacts")
				}
			}
		})
	}
}
