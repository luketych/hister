// SPDX-License-Identifier: AGPL-3.0-or-later

package model_test

import (
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/asciimoo/hister/server/model"
)

func TestAcceptInitialMediaAttemptPostgresConcurrentReceipt(t *testing.T) {
	initPostgresModel(t)
	pool, err := model.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	const callers = 8
	pool.SetMaxOpenConns(callers)

	scope := model.MediaScope{DocumentID: "postgres-initial-concurrent", UserID: 71}
	const request = `{"url":"https://www.youtube.com/watch?v=abcdefghijk"}`
	type outcome struct {
		result *model.MediaInitialResult
		err    error
	}
	start := make(chan struct{})
	results := make(chan outcome, callers)
	var workers sync.WaitGroup
	for range callers {
		workers.Go(func() {
			<-start
			result, err := model.AcceptInitialMediaAttempt(scope, request)
			results <- outcome{result, err}
		})
	}
	close(start)
	workers.Wait()
	close(results)

	id, sourceID, created := "", "", 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent PostgreSQL initial acceptance: %v", result.err)
		}
		if result.result == nil {
			t.Fatal("concurrent acceptance returned no receipt")
		}
		attempt := result.result.Attempt
		if id == "" {
			id, sourceID = attempt.ID, attempt.SourceID
		}
		if attempt.ID != id || attempt.SourceID != sourceID || attempt.RequestID != "initial:"+sourceID ||
			attempt.Generation != 1 || attempt.State != model.MediaQueued || attempt.RequestJSON != request {
			t.Fatalf("inconsistent PostgreSQL initial receipt: %#v", result.result)
		}
		if result.result.Replayed {
			continue
		}
		created++
	}
	if created != 1 {
		t.Fatalf("concurrent callers created %d initial attempts, want exactly one", created)
	}
	var sourceCount, attemptCount int64
	if err := model.DB.Model(&model.MediaSource{}).Where("document_id = ?", scope.DocumentID).Count(&sourceCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Model(&model.MediaAttempt{}).Where("source_id = ?", sourceID).Count(&attemptCount).Error; err != nil {
		t.Fatal(err)
	}
	if sourceCount != 1 || attemptCount != 1 {
		t.Fatalf("concurrent acceptance left %d sources and %d attempts; want one each", sourceCount, attemptCount)
	}
}

func TestAcceptInitialMediaAttemptPostgresRollsBackSourceOnAttemptFailure(t *testing.T) {
	initPostgresModel(t)
	scope := model.MediaScope{DocumentID: "postgres-initial-rollback", UserID: 73}
	const request = `{"url":"https://www.youtube.com/watch?v=abcdefghijk"}`
	if err := model.DB.Exec(`CREATE FUNCTION reject_initial_attempt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected attempt insert failure'; END $$`).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Exec(`CREATE TRIGGER reject_initial_attempt BEFORE INSERT ON media_attempts FOR EACH ROW EXECUTE FUNCTION reject_initial_attempt()`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := model.AcceptInitialMediaAttempt(scope, request); err == nil {
		t.Fatal("initial acceptance ignored injected PostgreSQL attempt insert failure")
	}
	var sourceCount, attemptCount int64
	if err := model.DB.Model(&model.MediaSource{}).Where("document_id = ?", scope.DocumentID).Count(&sourceCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Model(&model.MediaAttempt{}).Count(&attemptCount).Error; err != nil {
		t.Fatal(err)
	}
	if sourceCount != 0 || attemptCount != 0 {
		t.Fatalf("failed acceptance left %d sources and %d attempts; want transaction rollback", sourceCount, attemptCount)
	}
}

func TestAcceptInitialMediaAttemptPostgresConcurrentRevokeDocument(t *testing.T) {
	initPostgresModel(t)
	pool, err := model.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(2)
	const races = 40
	const request = `{"url":"https://www.youtube.com/watch?v=abcdefghijk"}`
	type acceptOutcome struct {
		result *model.MediaInitialResult
		err    error
	}
	type revokeOutcome struct {
		source *model.MediaSource
		err    error
	}

	for n := range races {
		scope := model.MediaScope{DocumentID: "postgres-initial-delete-race-" + strconv.Itoa(n), UserID: 74}
		start := make(chan struct{})
		accepted := make(chan acceptOutcome, 1)
		revoked := make(chan revokeOutcome, 1)
		acceptCall := func() {
			<-start
			result, err := model.AcceptInitialMediaAttempt(scope, request)
			accepted <- acceptOutcome{result, err}
		}
		revokeCall := func() {
			<-start
			source, err := model.RevokeMediaDocument(scope.DocumentID, scope.UserID)
			revoked <- revokeOutcome{source, err}
		}
		var workers sync.WaitGroup
		if n%2 == 0 {
			workers.Go(revokeCall)
			workers.Go(acceptCall)
		} else {
			workers.Go(acceptCall)
			workers.Go(revokeCall)
		}
		close(start)
		workers.Wait()
		accept := <-accepted
		remove := <-revoked
		if remove.err != nil || remove.source == nil {
			t.Fatalf("concurrent PostgreSQL document revocation failed on race %d", n)
		}
		if accept.err != nil && !errors.Is(accept.err, model.ErrMediaSourceRevoked) {
			t.Fatalf("concurrent PostgreSQL acceptance returned an unexpected error on race %d", n)
		}
		if (accept.err == nil) != (accept.result != nil) {
			t.Fatalf("acceptance result/error mismatch on race %d", n)
		}

		var sources []model.MediaSource
		if err := model.DB.Where("document_id = ?", scope.DocumentID).Find(&sources).Error; err != nil {
			t.Fatal(err)
		}
		if len(sources) != 1 || sources[0].UserID != scope.UserID || sources[0].RevokedAt == nil || remove.source.ID != sources[0].ID {
			t.Fatalf("race %d did not leave the single revoked source tombstone", n)
		}
		var attempts []model.MediaAttempt
		if err := model.DB.Where("source_id = ?", sources[0].ID).Find(&attempts).Error; err != nil {
			t.Fatal(err)
		}
		if len(attempts) > 1 {
			t.Fatalf("race %d left %d initial attempts; want at most one", n, len(attempts))
		}
		if accept.result != nil {
			if accept.result.Attempt.SourceID != sources[0].ID || len(attempts) != 1 || attempts[0].ID != accept.result.Attempt.ID {
				t.Fatalf("race %d accepted receipt does not match the persisted attempt", n)
			}
		}
		for _, attempt := range attempts {
			switch attempt.State {
			case model.MediaPendingCommit, model.MediaQueued, model.MediaStarting, model.MediaDownloading, model.MediaFinalizing:
				t.Fatalf("race %d left an attempt runnable after revocation", n)
			}
		}
		if _, err := model.AcceptInitialMediaAttempt(scope, request); !errors.Is(err, model.ErrMediaSourceRevoked) {
			t.Fatalf("later initial acceptance after revocation was not rejected on race %d", n)
		}
	}
}

func TestAcceptInitialMediaAttemptPostgresTombstoneAndReadd(t *testing.T) {
	initPostgresModel(t)
	scope := model.MediaScope{DocumentID: "postgres-initial-tombstone", UserID: 72}
	const request = `{"url":"https://www.youtube.com/watch?v=abcdefghijk"}`

	first, err := model.AcceptInitialMediaAttempt(scope, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.RevokeMediaSource(first.Attempt.SourceID); err != nil {
		t.Fatal(err)
	}
	if _, err := model.AcceptInitialMediaAttempt(scope, request); !errors.Is(err, model.ErrMediaSourceRevoked) {
		t.Fatalf("acceptance revived a tombstoned PostgreSQL source: %v", err)
	}

	readded, err := model.EnsureMediaSource(scope.DocumentID, scope.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if readded.ID == first.Attempt.SourceID {
		t.Fatal("re-added PostgreSQL source reused the tombstoned incarnation")
	}
	second, err := model.AcceptInitialMediaAttempt(scope, request)
	if err != nil {
		t.Fatalf("acceptance for explicitly re-added source: %v", err)
	}
	if second.Replayed || second.Attempt.SourceID != readded.ID || second.Attempt.ID == first.Attempt.ID || second.Attempt.Generation != 1 {
		t.Fatalf("re-added source reused an old acceptance receipt: %#v", second)
	}
	if err := model.RevokeMediaSource(readded.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := model.AcceptInitialMediaAttempt(scope, request); !errors.Is(err, model.ErrMediaSourceRevoked) {
		t.Fatalf("acceptance revived re-added tombstone: %v", err)
	}
}
