// SPDX-License-Identifier: AGPL-3.0-or-later

package model

import (
	"errors"

	"gorm.io/gorm"
)

// SuspendMediaTransfer persists a fail-closed cross-store move intent before
// any Bleve changes. A crash/partial save leaves it unavailable, never silently
// re-authorized by a later re-add or by observing a destination document.
func SuspendMediaTransfer(id, documentID string, userID uint) error {
	if documentID == "" {
		return errors.New("media destination document ID must not be empty")
	}
	return withMediaSource(id, false, func(tx *gorm.DB, source *MediaSource) error {
		if err := cancelSourceMediaAttempts(tx, id, "ownership_changed"); err != nil {
			return err
		}
		return tx.Model(source).Updates(map[string]any{
			"suspended": true, "pending_document_id": documentID, "pending_user_id": userID,
		}).Error
	})
}

func GetMediaSource(documentID string) (*MediaSource, error) {
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	var source MediaSource
	if err := db.First(&source, "document_id = ? AND revoked_at IS NULL", documentID).Error; err != nil {
		return nil, err
	}
	return &source, nil
}

func GetMediaSourceByID(id string) (*MediaSource, error) {
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	var source MediaSource
	if err := db.First(&source, "id = ? AND revoked_at IS NULL", id).Error; err != nil {
		return nil, err
	}
	return &source, nil
}

// AcceptReservedMediaAttempt is called only after the index commit. Joining
// accepted work is scoped to the exact source incarnation and frozen options.
// A coalesced reservation is not a second queued job or a new publication.
func AcceptReservedMediaAttempt(id string) (*MediaAttempt, error) {
	var accepted MediaAttempt
	err := withMediaAttempt(id, func(tx *gorm.DB, _ *MediaSource, attempt *MediaAttempt) error {
		var err error
		accepted, err = acceptMediaReservation(tx, attempt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &accepted, nil
}

func resolveJoinedMediaAttempt(tx *gorm.DB, attempt *MediaAttempt) (MediaAttempt, error) {
	if attempt.JoinedAttemptID == "" {
		return *attempt, nil
	}
	var joined MediaAttempt
	err := tx.First(&joined, "id = ? AND source_id = ?", attempt.JoinedAttemptID, attempt.SourceID).Error
	return joined, err
}

// The caller holds the live source lock and owns the SQL transaction. This is
// shared by post-index acceptance and explicit retry of a committed source.
func acceptMediaReservation(tx *gorm.DB, attempt *MediaAttempt) (MediaAttempt, error) {
	if attempt.State != MediaPendingCommit {
		if attempt.State == MediaCancelled && attempt.ErrorCode == "coalesced" {
			return resolveJoinedMediaAttempt(tx, attempt)
		}
		if mediaAttemptActive(attempt.State) || attempt.State == MediaReady || attempt.State == MediaFailed {
			return *attempt, nil
		}
		return MediaAttempt{}, ErrMediaConflict
	}
	var active MediaAttempt
	err := tx.Where("source_id = ? AND request_json = ? AND state IN ?", attempt.SourceID, attempt.RequestJSON,
		[]string{MediaQueued, MediaStarting, MediaDownloading, MediaFinalizing}).Order("generation DESC").First(&active).Error
	if err == nil {
		active.PublicationGeneration = max(active.Generation, active.PublicationGeneration, attempt.Generation)
		if err := tx.Model(&active).Update("publication_generation", active.PublicationGeneration).Error; err != nil {
			return MediaAttempt{}, err
		}
		attempt.JoinedAttemptID = active.ID
		if err := tx.Model(attempt).Update("joined_attempt_id", active.ID).Error; err != nil {
			return MediaAttempt{}, err
		}
		if err := endMediaAttempt(tx, attempt, MediaCancelled, "coalesced"); err != nil {
			return MediaAttempt{}, err
		}
		return active, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return MediaAttempt{}, err
	}
	attempt.State, attempt.VideoState, attempt.AudioState = MediaQueued, MediaQueued, MediaQueued
	if err := saveMediaAttempt(tx, attempt); err != nil {
		return MediaAttempt{}, err
	}
	return *attempt, nil
}

// AbandonMediaReservation never cancels already accepted durable work.
func AbandonMediaReservation(id string) error {
	return withMediaAttempt(id, func(tx *gorm.DB, _ *MediaSource, attempt *MediaAttempt) error {
		if attempt.State != MediaPendingCommit {
			return nil
		}
		return endMediaAttempt(tx, attempt, MediaInterrupted, "index_write_failed")
	})
}

// InterruptQueuedMediaAttempts is used only during exclusive initialization
// when the extractor is disabled: accepted work cannot claim to be in a queue
// that has no dispatcher. It may be explicitly requested again after enabling.
func InterruptQueuedMediaAttempts() error {
	db, err := mediaDB()
	if err != nil {
		return err
	}
	return db.Model(&MediaAttempt{}).Where("state = ?", MediaQueued).Updates(map[string]any{
		"state": MediaInterrupted, "video_state": MediaInterrupted, "audio_state": MediaInterrupted,
		"claim_token": "", "error_code": "dispatcher_disabled",
	}).Error
}

func NextQueuedMediaAttempt() (*MediaAttempt, error) {
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	var attempt MediaAttempt
	err = db.Where("state = ?", MediaQueued).Order("created_at").Order("id").First(&attempt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &attempt, err
}

func LatestMediaAttempt(sourceID string) (*MediaAttempt, error) {
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	var attempt MediaAttempt
	err = db.Where("source_id = ? AND state NOT IN ? AND error_code <> ?", sourceID, []string{MediaPendingCommit, MediaHistorical}, "coalesced").Order("CASE WHEN publication_generation > generation THEN publication_generation ELSE generation END DESC").First(&attempt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &attempt, err
}

func SetMediaModeJob(id, token, role, jobID string) error {
	if jobID == "" {
		return errors.New("media job ID must not be empty")
	}
	return withMediaAttempt(id, func(tx *gorm.DB, _ *MediaSource, attempt *MediaAttempt) error {
		if !ownsMediaClaim(attempt, token) {
			return ErrMediaConflict
		}
		var column, state string
		var previous *string
		switch role {
		case MediaVideo:
			column, state, previous = "video_job_id", attempt.VideoState, attempt.VideoJobID
		case MediaAudio:
			column, state, previous = "audio_job_id", attempt.AudioState, attempt.AudioJobID
		default:
			return errors.New("invalid media role")
		}
		if state != MediaQueued || previous != nil {
			return ErrMediaConflict
		}
		return tx.Model(attempt).Update(column, jobID).Error
	})
}

func InterruptClaimedMediaAttempt(id, token string) error {
	return withMediaAttempt(id, func(tx *gorm.DB, _ *MediaSource, attempt *MediaAttempt) error {
		if !ownsMediaClaim(attempt, token) {
			return ErrMediaConflict
		}
		return endMediaAttempt(tx, attempt, MediaInterrupted, "interrupted")
	})
}

// MediaArtifactsForDocument includes retired history for legacy-link resolution
// and diagnostics. It is not playback eligibility; recheck the exact record.
func MediaArtifactsForDocument(documentID string, userID uint) ([]MediaArtifact, error) {
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	var artifacts []MediaArtifact
	err = db.Model(&MediaArtifact{}).Select("media_artifacts.*").
		Joins("JOIN media_sets ON media_sets.id = media_artifacts.set_id").
		Joins("JOIN media_sources ON media_sources.id = media_sets.source_id").
		Where("media_sources.document_id = ? AND media_sources.user_id = ? AND media_sources.revoked_at IS NULL AND media_sources.suspended = ?", documentID, userID, false).
		Find(&artifacts).Error
	return artifacts, err
}

// MediaArtifactSource is an internal lookup, not an authorization decision.
// Includes retired records solely to route authorized diagnostics. Consumers
// must check source visibility and re-check the exact live binding/retirement.
func MediaArtifactSource(id string) (*MediaSource, error) {
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	var source MediaSource
	err = db.Model(&MediaSource{}).Select("media_sources.*").
		Joins("JOIN media_sets ON media_sets.source_id = media_sources.id").
		Joins("JOIN media_artifacts ON media_artifacts.set_id = media_sets.id").
		Where("media_artifacts.id = ? AND media_sources.revoked_at IS NULL AND media_sources.suspended = ?", id, false).
		First(&source).Error
	return &source, err
}
