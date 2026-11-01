// SPDX-License-Identifier: AGPL-3.0-or-later

package model

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"math"

	"gorm.io/gorm"
)

// MediaScope is the source binding validated by an authorized server caller.
// It is not an authorization credential supplied by a client.
type MediaScope struct {
	DocumentID string
	UserID     uint
}

type MediaRetryResult struct {
	Attempt  MediaAttempt
	Replayed bool
}

// MediaAttemptInScope is a read-only preflight for command eligibility. Every
// actual transition still rechecks this binding under the source transaction.
func MediaAttemptInScope(scope MediaScope, id string) (*MediaAttempt, error) {
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	var attempt MediaAttempt
	err = db.Model(&MediaAttempt{}).Select("media_attempts.*").
		Joins("JOIN media_sources ON media_sources.id = media_attempts.source_id").
		Where("media_attempts.id = ? AND media_sources.document_id = ? AND media_sources.user_id = ? AND media_sources.revoked_at IS NULL", id, scope.DocumentID, scope.UserID).
		First(&attempt).Error
	if err != nil {
		return nil, err
	}
	return &attempt, nil
}

// Check scope BEFORE reporting revoked/suspended state. Using the unscoped
// worker helper here would leak a foreign attempt's state via 404 versus 409.
func withScopedMediaAttempt(scope MediaScope, id string, fn func(*gorm.DB, *MediaSource, *MediaAttempt) error) error {
	observed, err := MediaAttemptInScope(scope, id)
	if err != nil {
		return err
	}
	return withMediaSource(observed.SourceID, true, func(tx *gorm.DB, source *MediaSource) error {
		if source.DocumentID != scope.DocumentID || source.UserID != scope.UserID || source.RevokedAt != nil {
			return gorm.ErrRecordNotFound
		}
		if source.Suspended {
			return ErrMediaConflict
		}
		var attempt MediaAttempt
		if err := tx.First(&attempt, "id = ? AND source_id = ?", id, source.ID).Error; err != nil {
			return err
		}
		return fn(tx, source, &attempt)
	})
}

// RetryMediaAttempt retries a terminal failure/cancellation/interruption of an
// existing committed source, never a ready or unpublished attempt. The source
// indexer must hold its commit coordinator and verify its current document.
// requestJSON comes from CURRENT server config, never the previous job/client.
// The old attempt ID is the durable idempotency key: repeated POSTs return the
// same actual successor (including coalesced/terminal outcomes) without reruns.
func RetryMediaAttempt(scope MediaScope, id, requestJSON string) (*MediaRetryResult, error) {
	var request map[string]any
	if len(requestJSON) > 64*1024 || json.Unmarshal([]byte(requestJSON), &request) != nil || request == nil {
		return nil, errors.New("media retry requires a server-owned JSON request")
	}
	var result MediaRetryResult
	err := withScopedMediaAttempt(scope, id, func(tx *gorm.DB, source *MediaSource, previous *MediaAttempt) error {
		if previous.JoinedAttemptID != "" || (previous.State != MediaFailed && previous.State != MediaCancelled && previous.State != MediaInterrupted) {
			return ErrMediaConflict
		}
		requestID := "retry:" + previous.ID
		var successor MediaAttempt
		err := tx.First(&successor, "source_id = ? AND request_id = ?", source.ID, requestID).Error
		if err == nil {
			if successor.State == MediaPendingCommit {
				return ErrMediaConflict
			}
			result.Attempt, err = resolveJoinedMediaAttempt(tx, &successor)
			result.Replayed = true
			return err
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var latest MediaAttempt
		if err := tx.Where("source_id = ? AND state <> ? AND error_code <> ?", source.ID, MediaPendingCommit, "coalesced").Order("CASE WHEN publication_generation > generation THEN publication_generation ELSE generation END DESC").First(&latest).Error; err != nil {
			return err
		}
		if latest.ID != previous.ID {
			return ErrMediaConflict
		}
		if source.NextGeneration >= math.MaxInt64 {
			return errors.New("media generation exhausted")
		}
		generation := source.NextGeneration + 1
		successor = MediaAttempt{
			ID: rand.Text(), SourceID: source.ID, RequestID: requestID, RequestJSON: requestJSON,
			Generation: generation, PublicationGeneration: generation, State: MediaPendingCommit, VideoState: MediaPendingCommit, AudioState: MediaPendingCommit,
		}
		if err := tx.Create(&successor).Error; err != nil {
			return err
		}
		result.Attempt, err = acceptMediaReservation(tx, &successor)
		if err != nil {
			return err
		}
		return tx.Model(source).Update("next_generation", generation).Error
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// CancelScopedMediaAttempt atomically fences only the addressed accepted
// attempt, never "whatever is current". Completion wins as a conflict; an
// identical repeated cancellation is harmless and does not cancel a retry.
func CancelScopedMediaAttempt(scope MediaScope, id string) (*MediaAttempt, error) {
	var cancelled MediaAttempt
	err := withScopedMediaAttempt(scope, id, func(tx *gorm.DB, _ *MediaSource, attempt *MediaAttempt) error {
		if attempt.State == MediaCancelled && attempt.ErrorCode == "cancelled" {
			cancelled = *attempt
			return nil
		}
		if !mediaAttemptActive(attempt.State) {
			return ErrMediaConflict
		}
		if err := endMediaAttempt(tx, attempt, MediaCancelled, "cancelled"); err != nil {
			return err
		}
		cancelled = *attempt
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &cancelled, nil
}
