// SPDX-License-Identifier: AGPL-3.0-or-later

package model

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	MediaPendingCommit = "pending_commit" // Ordering reservation, not accepted work.
	MediaQueued        = "queued"
	MediaStarting      = "starting" // Claimed, but no downloader process has started.
	MediaDownloading   = "downloading"
	MediaDownloaded    = "downloaded" // A mode finished; the pair is not published.
	MediaFinalizing    = "finalizing"
	MediaReady         = "ready"
	MediaFailed        = "failed"
	MediaCancelled     = "cancelled"
	MediaInterrupted   = "interrupted"

	MediaVideo = "video"
	MediaAudio = "audio"
)

var (
	ErrMediaConflict      = errors.New("media operation conflicts with current state")
	ErrMediaSourceRevoked = errors.New("media source is revoked")
)

// MediaSource is a server-owned incarnation of an indexed source document.
// DocumentID must be derived from the normalized URL and authoritative owner,
// never from a submitted DocumentID. Revocation retains history and frees the
// active key so re-adding the same document creates a different incarnation.
type MediaSource struct {
	ID                 string  `gorm:"primaryKey"`
	DocumentID         string  `gorm:"type:text;not null"`
	UserID             uint    `gorm:"not null"`
	ActiveKey          *string `gorm:"uniqueIndex" json:"-"`
	NextGeneration     uint64  `gorm:"not null;default:0"`
	AcceptedGeneration uint64  `gorm:"not null;default:0"`
	SelectedSetID      string
	Suspended          bool `gorm:"not null;default:false"`
	PendingDocumentID  string
	PendingUserID      uint
	RevokedAt          *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// MediaAttempt persists an ordering reservation or accepted scheduling intent.
// Only queued attempts are dispatchable. RequestID is allocated by the server
// for idempotent acceptance; RequestJSON is a frozen internal worker request,
// not imported document metadata. A dispatcher must be available before
// accepting work. Indexer integration must accept only after the source write
// commits; these SQL transactions do not include Bleve.
type MediaAttempt struct {
	ID         string `gorm:"primaryKey"`
	SourceID   string `gorm:"not null;uniqueIndex:idx_media_request;uniqueIndex:idx_media_generation"`
	RequestID  string `gorm:"not null;uniqueIndex:idx_media_request" json:"-"`
	Generation uint64 `gorm:"not null;uniqueIndex:idx_media_generation"`
	// PublicationGeneration accounts for later requests joining this actual
	// attempt. Its original identity/generation remain immutable.
	PublicationGeneration uint64  `gorm:"not null;default:0"`
	RequestJSON           string  `gorm:"type:text;not null" json:"-"`
	State                 string  `gorm:"not null;index"`
	VideoState            string  `gorm:"not null"`
	AudioState            string  `gorm:"not null"`
	ClaimToken            string  `json:"-"`
	VideoJobID            *string `gorm:"uniqueIndex"`
	AudioJobID            *string `gorm:"uniqueIndex"`
	JoinedAttemptID       string  `json:"-"`
	ErrorCode             string  // Safe, bounded code only; never raw process diagnostics.
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// MediaSet is one committed, validated pair. Staging/downloading never creates
// a set or artifacts. Historical sets are independent of the current selection.
type MediaSet struct {
	ID         string `gorm:"primaryKey"`
	SourceID   string `gorm:"not null;index"`
	AttemptID  string `gorm:"not null;uniqueIndex"`
	Generation uint64 `gorm:"not null"`
	CreatedAt  time.Time
}

// MediaArtifactInput is supplied only by the trusted manifest/file validator.
// Structural checks here do not replace validation of producer provenance or
// safely opening and checking actual regular files before publication/serving.
type MediaArtifactInput struct {
	Role         string `gorm:"not null;uniqueIndex:idx_media_set_role"`
	JobID        string `gorm:"not null;uniqueIndex"`
	StorageRoot  string `gorm:"not null" json:"-"`
	RelativePath string `gorm:"not null" json:"-"`
	Size         int64  `gorm:"not null"`
	ContentType  string `gorm:"not null"`
}

type MediaArtifact struct {
	ID    string `gorm:"primaryKey"`
	SetID string `gorm:"not null;uniqueIndex:idx_media_set_role"`
	MediaArtifactInput
	RetiredAt      *time.Time
	BytesRemovedAt *time.Time `gorm:"index" json:"-"`
	CreatedAt      time.Time
}

func mediaDB() (*gorm.DB, error) {
	if DB == nil {
		return nil, errors.New("database is not initialized")
	}
	return DB, nil
}

func mediaSourceKey(documentID string, userID uint) string {
	hash := sha256.Sum256([]byte(strconv.FormatUint(uint64(userID), 10) + "\x00" + documentID))
	return hex.EncodeToString(hash[:])
}

func EnsureMediaSource(documentID string, userID uint) (*MediaSource, error) {
	if documentID == "" {
		return nil, errors.New("media source document ID must not be empty")
	}
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	key := mediaSourceKey(documentID, userID)
	source := MediaSource{ID: rand.Text(), DocumentID: documentID, UserID: userID, ActiveKey: &key}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&source).Error; err != nil {
		return nil, err
	}
	var existing MediaSource
	if err := db.Where("active_key = ? AND document_id = ? AND user_id = ? AND revoked_at IS NULL", key, documentID, userID).First(&existing).Error; err != nil {
		return nil, err
	}
	return &existing, nil
}

// lockMediaSource takes a short write lock usable on SQLite and PostgreSQL.
// Always take this lock before reading mutable attempt/source state. It does
// not span extraction or a downloader process. The first statement is a write
// to avoid SQLite read-to-write transaction upgrades under concurrent writers.
func lockMediaSource(tx *gorm.DB, id string) (*MediaSource, error) {
	result := tx.Model(&MediaSource{}).Where("id = ?", id).Update("updated_at", time.Now().UTC())
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, gorm.ErrRecordNotFound
	}
	var source MediaSource
	if err := tx.First(&source, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &source, nil
}

func withMediaSource(id string, allowRevoked bool, fn func(*gorm.DB, *MediaSource) error) error {
	db, err := mediaDB()
	if err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		source, err := lockMediaSource(tx, id)
		if err != nil {
			return err
		}
		if source.RevokedAt != nil && !allowRevoked {
			return ErrMediaSourceRevoked
		}
		if source.Suspended && !allowRevoked {
			return ErrMediaConflict
		}
		return fn(tx, source)
	})
}

func withMediaAttempt(id string, fn func(*gorm.DB, *MediaSource, *MediaAttempt) error) error {
	db, err := mediaDB()
	if err != nil {
		return err
	}
	// Resolve the immutable source ID outside the transaction, then reload all
	// mutable state after acquiring its write lock.
	var observed MediaAttempt
	if err := db.Select("source_id").First(&observed, "id = ?", id).Error; err != nil {
		return err
	}
	return withMediaSource(observed.SourceID, false, func(tx *gorm.DB, source *MediaSource) error {
		var attempt MediaAttempt
		if err := tx.First(&attempt, "id = ? AND source_id = ?", id, source.ID).Error; err != nil {
			return err
		}
		return fn(tx, source, &attempt)
	})
}

// ReserveMediaAttempt assigns ordering before extraction without claiming that
// work is queued. Accept the same RequestID only after the actual document
// commit. Failed/stale document writes must abandon their reservation.
func ReserveMediaAttempt(sourceID, requestID, requestJSON string) (*MediaAttempt, error) {
	return ensureMediaAttempt(sourceID, requestID, requestJSON, MediaPendingCommit)
}

func AcceptMediaAttempt(sourceID, requestID, requestJSON string) (*MediaAttempt, error) {
	return ensureMediaAttempt(sourceID, requestID, requestJSON, MediaQueued)
}

func ensureMediaAttempt(sourceID, requestID, requestJSON, state string) (*MediaAttempt, error) {
	var request map[string]any
	if requestID == "" || len(requestID) > 128 || len(requestJSON) > 64*1024 || json.Unmarshal([]byte(requestJSON), &request) != nil || request == nil {
		return nil, errors.New("media attempt requires a server request ID and a JSON object")
	}
	var attempt MediaAttempt
	err := withMediaSource(sourceID, false, func(tx *gorm.DB, source *MediaSource) error {
		err := tx.Where("source_id = ? AND request_id = ?", sourceID, requestID).First(&attempt).Error
		if err == nil {
			if attempt.RequestJSON != requestJSON {
				return ErrMediaConflict
			}
			if state == MediaQueued && attempt.State == MediaPendingCommit {
				attempt.State, attempt.VideoState, attempt.AudioState = MediaQueued, MediaQueued, MediaQueued
				return saveMediaAttempt(tx, &attempt)
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if source.NextGeneration >= math.MaxInt64 {
			return errors.New("media generation exhausted")
		}
		attempt = MediaAttempt{
			ID: rand.Text(), SourceID: sourceID, RequestID: requestID,
			RequestJSON: requestJSON, Generation: source.NextGeneration + 1,
			PublicationGeneration: source.NextGeneration + 1,
			State:                 state, VideoState: state, AudioState: state,
		}
		if err := tx.Create(&attempt).Error; err != nil {
			return err
		}
		return tx.Model(source).Update("next_generation", attempt.Generation).Error
	})
	if err != nil {
		return nil, err
	}
	return &attempt, nil
}

func saveMediaAttempt(tx *gorm.DB, attempt *MediaAttempt) error {
	attempt.UpdatedAt = time.Now().UTC()
	return tx.Model(&MediaAttempt{}).Where("id = ?", attempt.ID).Updates(map[string]any{
		"state": attempt.State, "video_state": attempt.VideoState, "audio_state": attempt.AudioState,
		"claim_token": attempt.ClaimToken, "error_code": attempt.ErrorCode, "updated_at": attempt.UpdatedAt,
	}).Error
}

func ClaimMediaAttempt(id string) (*MediaAttempt, error) {
	var claimed MediaAttempt
	err := withMediaAttempt(id, func(tx *gorm.DB, _ *MediaSource, attempt *MediaAttempt) error {
		if attempt.State != MediaQueued {
			return ErrMediaConflict
		}
		attempt.State = MediaStarting
		attempt.ClaimToken = rand.Text()
		if err := saveMediaAttempt(tx, attempt); err != nil {
			return err
		}
		claimed = *attempt
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &claimed, nil
}

func mediaAttemptActive(state string) bool {
	return state == MediaQueued || state == MediaStarting || state == MediaDownloading || state == MediaFinalizing
}

func ownsMediaClaim(attempt *MediaAttempt, token string) bool {
	return token != "" && token == attempt.ClaimToken && attempt.State != MediaQueued && mediaAttemptActive(attempt.State)
}

func advanceMediaMode(id, token, role, from, to string) error {
	return withMediaAttempt(id, func(tx *gorm.DB, _ *MediaSource, attempt *MediaAttempt) error {
		if !ownsMediaClaim(attempt, token) {
			return ErrMediaConflict
		}
		var mode *string
		switch role {
		case MediaVideo:
			mode = &attempt.VideoState
		case MediaAudio:
			mode = &attempt.AudioState
		default:
			return errors.New("invalid media role")
		}
		if *mode != from {
			return ErrMediaConflict
		}
		*mode = to
		attempt.State = MediaDownloading
		if attempt.VideoState == MediaDownloaded && attempt.AudioState == MediaDownloaded {
			attempt.State = MediaFinalizing
		}
		return saveMediaAttempt(tx, attempt)
	})
}

// StartMediaMode is called only after the actual producer process starts.
func StartMediaMode(id, token, role string) error {
	return advanceMediaMode(id, token, role, MediaQueued, MediaDownloading)
}

// CompleteMediaMode records process completion, not readiness or authorization.
func CompleteMediaMode(id, token, role string) error {
	return advanceMediaMode(id, token, role, MediaDownloading, MediaDownloaded)
}

func endMediaAttempt(tx *gorm.DB, attempt *MediaAttempt, state, code string) error {
	attempt.State, attempt.ErrorCode, attempt.ClaimToken = state, code, ""
	if attempt.VideoState != MediaDownloaded {
		attempt.VideoState = state
	}
	if attempt.AudioState != MediaDownloaded {
		attempt.AudioState = state
	}
	return saveMediaAttempt(tx, attempt)
}

func FailMediaAttempt(id, token, code string) error {
	switch code {
	case "start_failed", "download_failed", "validation_failed", "publication_failed":
	default:
		return errors.New("invalid public media failure code")
	}
	return withMediaAttempt(id, func(tx *gorm.DB, _ *MediaSource, attempt *MediaAttempt) error {
		if !ownsMediaClaim(attempt, token) {
			return ErrMediaConflict
		}
		return endMediaAttempt(tx, attempt, MediaFailed, code)
	})
}

// CancelMediaAttempt requires prior source authorization by the caller.
func CancelMediaAttempt(id string) error {
	return withMediaAttempt(id, func(tx *gorm.DB, _ *MediaSource, attempt *MediaAttempt) error {
		if attempt.State != MediaPendingCommit && !mediaAttemptActive(attempt.State) {
			return ErrMediaConflict
		}
		return endMediaAttempt(tx, attempt, MediaCancelled, "cancelled")
	})
}

// InterruptMediaAttempts must run during exclusive startup recovery with old
// workers stopped. Queued work remains dispatchable; uncommitted reservations
// and claimed/unpublished work are interrupted, never promoted by discovering
// files on disk.
func InterruptMediaAttempts() error {
	db, err := mediaDB()
	if err != nil {
		return err
	}
	return db.Model(&MediaAttempt{}).Where("state IN ?", []string{MediaPendingCommit, MediaStarting, MediaDownloading, MediaFinalizing}).Updates(map[string]any{
		"state": MediaInterrupted, "claim_token": "", "error_code": "interrupted",
		"video_state": gorm.Expr("CASE WHEN video_state = ? THEN video_state ELSE ? END", MediaDownloaded, MediaInterrupted),
		"audio_state": gorm.Expr("CASE WHEN audio_state = ? THEN audio_state ELSE ? END", MediaDownloaded, MediaInterrupted),
	}).Error
}

func validMediaPair(pair []MediaArtifactInput) bool {
	if len(pair) != 2 || pair[0].Role == pair[1].Role || pair[0].JobID == pair[1].JobID {
		return false
	}
	for _, artifact := range pair {
		if artifact.JobID == "" || artifact.Size <= 0 || artifact.RelativePath == "." || !filepath.IsAbs(artifact.StorageRoot) || filepath.Clean(artifact.StorageRoot) != artifact.StorageRoot || !filepath.IsLocal(artifact.RelativePath) || filepath.Clean(artifact.RelativePath) != artifact.RelativePath {
			return false
		}
		switch artifact.ContentType {
		case "video/mp4", "video/webm", "video/ogg", "video/quicktime", "video/x-matroska":
			if artifact.Role != MediaVideo {
				return false
			}
		case "audio/mpeg", "audio/mp4", "audio/webm", "audio/ogg", "audio/wav", "audio/flac", "audio/aac", "audio/opus", "audio/x-m4a", "audio/x-matroska":
			if artifact.Role != MediaAudio {
				return false
			}
		default:
			return false
		}
		// Operational reports are never media, regardless of their claimed MIME.
		switch strings.ToLower(filepath.Ext(artifact.RelativePath)) {
		case ".json", ".html", ".htm", ".part", ".tmp":
			return false
		}
	}
	return true
}

// PublishMediaPair atomically records the verified pair, readiness and the
// conditional current selection. The caller must validate provenance and actual
// files first. A late successful generation is retained as history but cannot
// replace a newer success. Newer failures do not block older valid successes.
func PublishMediaPair(id, token string, pair []MediaArtifactInput) (*MediaSet, error) {
	if !validMediaPair(pair) {
		return nil, errors.New("invalid media artifact pair")
	}
	var set MediaSet
	err := withMediaAttempt(id, func(tx *gorm.DB, source *MediaSource, attempt *MediaAttempt) error {
		if token == "" || token != attempt.ClaimToken {
			return ErrMediaConflict
		}
		if attempt.State == MediaReady {
			if err := tx.First(&set, "attempt_id = ?", id).Error; err != nil {
				return err
			}
			var artifacts []MediaArtifact
			if err := tx.Where("set_id = ?", set.ID).Find(&artifacts).Error; err != nil {
				return err
			}
			if len(artifacts) != len(pair) {
				return ErrMediaConflict
			}
			for _, expected := range pair {
				matched := false
				for _, artifact := range artifacts {
					if artifact.MediaArtifactInput == expected {
						matched = true
					}
				}
				if !matched {
					return ErrMediaConflict
				}
			}
			return nil
		}
		if attempt.State != MediaFinalizing || attempt.VideoState != MediaDownloaded || attempt.AudioState != MediaDownloaded {
			return ErrMediaConflict
		}
		generation := max(attempt.Generation, attempt.PublicationGeneration)
		set = MediaSet{ID: rand.Text(), SourceID: source.ID, AttemptID: id, Generation: generation}
		if err := tx.Create(&set).Error; err != nil {
			return err
		}
		for _, candidate := range pair {
			jobID := attempt.VideoJobID
			if candidate.Role == MediaAudio {
				jobID = attempt.AudioJobID
			}
			if jobID != nil && *jobID != candidate.JobID {
				return ErrMediaConflict
			}
			artifact := MediaArtifact{ID: rand.Text(), SetID: set.ID, MediaArtifactInput: candidate}
			if err := tx.Create(&artifact).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&MediaSource{}).Where("id = ? AND revoked_at IS NULL AND accepted_generation < ?", source.ID, generation).Updates(map[string]any{
			"accepted_generation": generation, "selected_set_id": set.ID,
		}).Error; err != nil {
			return err
		}
		attempt.State, attempt.VideoState, attempt.AudioState = MediaReady, MediaReady, MediaReady
		return saveMediaAttempt(tx, attempt)
	})
	if err != nil {
		return nil, err
	}
	return &set, nil
}

// LookupMediaArtifact checks the exact live source/owner binding, not possession
// of an opaque ID or membership in submitted metadata. The HTTP layer must also
// enforce source-document visibility and safely open/check the retained file.
func LookupMediaArtifact(id string, userID uint, documentID string) (*MediaArtifact, error) {
	artifact, err := LookupMediaArtifactRecord(id, userID, documentID)
	if err != nil {
		return nil, err
	}
	if artifact.RetiredAt != nil {
		return nil, gorm.ErrRecordNotFound
	}
	return artifact, nil
}

// LookupMediaArtifactRecord includes retired records for authorized diagnostics,
// but still requires the exact live, unsuspended source/owner binding. A returned
// record is NOT playback eligibility: callers must reject retirement and safely
// open the file. LookupMediaArtifact remains the playback-only lookup.
func LookupMediaArtifactRecord(id string, userID uint, documentID string) (*MediaArtifact, error) {
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	var artifact MediaArtifact
	err = db.Model(&MediaArtifact{}).Select("media_artifacts.*").
		Joins("JOIN media_sets ON media_sets.id = media_artifacts.set_id").
		Joins("JOIN media_sources ON media_sources.id = media_sets.source_id").
		Where("media_artifacts.id = ? AND media_sources.revoked_at IS NULL AND media_sources.user_id = ? AND media_sources.document_id = ? AND media_sources.suspended = ?", id, userID, documentID, false).
		First(&artifact).Error
	if err != nil {
		return nil, err
	}
	return &artifact, nil
}

func CurrentMediaArtifacts(sourceID string) ([]MediaArtifact, error) {
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	var artifacts []MediaArtifact
	err = db.Model(&MediaArtifact{}).Select("media_artifacts.*").
		Joins("JOIN media_sources ON media_sources.selected_set_id = media_artifacts.set_id").
		Where("media_sources.id = ? AND media_sources.revoked_at IS NULL AND media_artifacts.retired_at IS NULL AND media_sources.suspended = ?", sourceID, false).
		Order("media_artifacts.role").Find(&artifacts).Error
	return artifacts, err
}

func cancelSourceMediaAttempts(tx *gorm.DB, sourceID, code string) error {
	return tx.Model(&MediaAttempt{}).Where("source_id = ? AND state IN ?", sourceID, []string{MediaPendingCommit, MediaQueued, MediaStarting, MediaDownloading, MediaFinalizing}).Updates(map[string]any{
		"state": MediaCancelled, "claim_token": "", "error_code": code,
		"video_state": gorm.Expr("CASE WHEN video_state = ? THEN video_state ELSE ? END", MediaDownloaded, MediaCancelled),
		"audio_state": gorm.Expr("CASE WHEN audio_state = ? THEN audio_state ELSE ? END", MediaDownloaded, MediaCancelled),
	}).Error
}

// RevokeMediaSource invalidates bindings and pending publication, retaining all
// records and bytes. It does not stop processes; the dispatcher must cancel them.
func RevokeMediaSource(id string) error {
	return withMediaSource(id, true, func(tx *gorm.DB, source *MediaSource) error {
		if source.RevokedAt != nil {
			return nil
		}
		if err := cancelSourceMediaAttempts(tx, id, "source_deleted"); err != nil {
			return err
		}
		return tx.Model(source).Updates(map[string]any{"revoked_at": time.Now().UTC(), "active_key": nil}).Error
	})
}

// TransferMediaSource requires an authorized ownership move coordinated with
// the actual index write. History follows the new owner; incompatible pending
// work is cancelled. A conflicting live target fails without changing bindings.
func TransferMediaSource(id, documentID string, userID uint) error {
	if documentID == "" {
		return errors.New("media source document ID must not be empty")
	}
	return withMediaSource(id, true, func(tx *gorm.DB, source *MediaSource) error {
		if source.RevokedAt != nil {
			return ErrMediaSourceRevoked
		}
		if source.Suspended && (source.PendingDocumentID != documentID || source.PendingUserID != userID) {
			return ErrMediaConflict
		}
		if source.DocumentID == documentID && source.UserID == userID && !source.Suspended {
			return nil
		}
		if err := cancelSourceMediaAttempts(tx, id, "ownership_changed"); err != nil {
			return err
		}
		return tx.Model(source).Updates(map[string]any{
			"document_id": documentID, "user_id": userID, "active_key": mediaSourceKey(documentID, userID),
			"suspended": false, "pending_document_id": "", "pending_user_id": 0,
		}).Error
	})
}
