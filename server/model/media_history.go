// SPDX-License-Identifier: AGPL-3.0-or-later

package model

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

const MediaHistorical = "historical"

// MediaHistoryInput describes a pair approved by the trusted history importer.
// Approval and evidence validation belong to the caller, not this model layer.
type MediaHistoryInput struct {
	ApprovalID, SourceID, DocumentID, ApprovalJSON string
	UserID, ApprovedBy                             uint
	Pair                                           []MediaArtifactInput
}

type MediaHistoryReceipt struct {
	ID                string `gorm:"primaryKey"`
	SourceID          string `gorm:"not null;index"`
	DocumentID        string `gorm:"not null"`
	UserID            uint   `gorm:"not null"`
	ApprovedBy        uint   `gorm:"not null"`
	ApprovalJSON      string `gorm:"type:text;not null" json:"-"`
	BindingSHA256     string `gorm:"not null"`
	FingerprintSHA256 string `gorm:"not null" json:"-"`
	SetID             string `gorm:"not null;uniqueIndex"`
	CreatedAt         time.Time
}

var ErrMediaHistoryConfirmation = errors.New("historical media confirmation does not match current plan")

type MediaHistoryResult struct {
	Receipt   MediaHistoryReceipt
	Artifacts []MediaArtifact
	Replayed  bool
}

type mediaHistoryArtifactFingerprint struct {
	Role, JobID, StorageRoot, RelativePath string
	Size                                   int64
	ContentType                            string
}

type mediaHistoryFingerprint struct {
	ApprovalJSON string
	SourceID     string
	DocumentID   string
	UserID       uint
	Pair         []mediaHistoryArtifactFingerprint
}

func mediaHistoryHashes(input MediaHistoryInput) ([]MediaArtifactInput, string, string, error) {
	if input.ApprovalID == "" || len(input.ApprovalID) > 120 || len(input.ApprovalJSON) > 64*1024 {
		return nil, "", "", errors.New("invalid media history approval")
	}
	var approval map[string]any
	if json.Unmarshal([]byte(input.ApprovalJSON), &approval) != nil || approval == nil {
		return nil, "", "", errors.New("media history approval must be a JSON object")
	}
	if input.SourceID == "" || input.DocumentID == "" || !validMediaPair(input.Pair) {
		return nil, "", "", errors.New("invalid media history binding or artifact pair")
	}
	pair := append([]MediaArtifactInput(nil), input.Pair...)
	sort.Slice(pair, func(i, j int) bool { return pair[i].Role < pair[j].Role })
	bindingBytes, err := json.Marshal(struct {
		SourceID, DocumentID string
		UserID               uint
	}{input.SourceID, input.DocumentID, input.UserID})
	if err != nil {
		return nil, "", "", err
	}
	bindingHash := sha256.Sum256(bindingBytes)
	fingerprintPair := make([]mediaHistoryArtifactFingerprint, len(pair))
	for i, artifact := range pair {
		fingerprintPair[i] = mediaHistoryArtifactFingerprint(artifact)
	}
	fingerprintBytes, err := json.Marshal(mediaHistoryFingerprint{
		ApprovalJSON: input.ApprovalJSON, SourceID: input.SourceID, DocumentID: input.DocumentID,
		UserID: input.UserID, Pair: fingerprintPair,
	})
	if err != nil {
		return nil, "", "", err
	}
	fingerprint := sha256.Sum256(fingerprintBytes)
	return pair, hex.EncodeToString(bindingHash[:]), hex.EncodeToString(fingerprint[:]), nil
}

func CheckMediaHistory(input MediaHistoryInput) (*MediaHistoryCheck, error) {
	pair, bindingHash, fingerprint, err := mediaHistoryHashes(input)
	if err != nil {
		return nil, err
	}
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	result := &MediaHistoryCheck{ApprovalID: input.ApprovalID, ConfirmationSHA256: fingerprint}
	err = db.Transaction(func(tx *gorm.DB) error {
		var source MediaSource
		if err := tx.First(&source, "id = ?", input.SourceID).Error; err != nil {
			return err
		}
		if source.RevokedAt != nil || source.Suspended || source.DocumentID != input.DocumentID || source.UserID != input.UserID || source.ActiveKey == nil || *source.ActiveKey != mediaSourceKey(source.DocumentID, source.UserID) {
			return ErrMediaConflict
		}
		return checkMediaHistoryRegistry(tx, source, input, pair, bindingHash, fingerprint, result)
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type MediaHistoryCheck struct {
	ApprovalID         string
	ConfirmationSHA256 string
	Replayed           bool
}

func checkMediaHistoryRegistry(tx *gorm.DB, source MediaSource, input MediaHistoryInput, pair []MediaArtifactInput, bindingHash, fingerprint string, result *MediaHistoryCheck) error {
	var receipt MediaHistoryReceipt
	err := tx.First(&receipt, "id = ?", input.ApprovalID).Error
	if err == nil {
		if receipt.FingerprintSHA256 != fingerprint || receipt.BindingSHA256 != bindingHash || receipt.SourceID != source.ID || receipt.DocumentID != input.DocumentID || receipt.UserID != input.UserID {
			return ErrMediaConflict
		}
		var set MediaSet
		if err := tx.First(&set, "id = ? AND source_id = ?", receipt.SetID, source.ID).Error; err != nil {
			return ErrMediaConflict
		}
		var artifacts []MediaArtifact
		if err := tx.Where("set_id = ?", set.ID).Order("role").Find(&artifacts).Error; err != nil {
			return err
		}
		if len(artifacts) != len(pair) {
			return ErrMediaConflict
		}
		for i := range pair {
			if artifacts[i].MediaArtifactInput != pair[i] {
				return ErrMediaConflict
			}
		}
		result.Replayed = true
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if source.NextGeneration >= math.MaxInt64 {
		return errors.New("media generation exhausted")
	}
	for _, artifact := range pair {
		var attempts int64
		if err := tx.Model(&MediaAttempt{}).Where("video_job_id = ? OR audio_job_id = ?", artifact.JobID, artifact.JobID).Count(&attempts).Error; err != nil {
			return err
		}
		var artifacts int64
		if err := tx.Model(&MediaArtifact{}).Where("job_id = ?", artifact.JobID).Count(&artifacts).Error; err != nil {
			return err
		}
		if attempts != 0 || artifacts != 0 {
			return ErrMediaConflict
		}
	}
	return nil
}

func ImportMediaHistory(input MediaHistoryInput) (*MediaHistoryResult, error) {
	return applyMediaHistory(input, "", false)
}

// ApplyMediaHistory atomically checks the caller's stale-plan confirmation and registers history.
func ApplyMediaHistory(input MediaHistoryInput, confirmation string) (*MediaHistoryResult, error) {
	return applyMediaHistory(input, confirmation, true)
}

func applyMediaHistory(input MediaHistoryInput, confirmation string, requireConfirmation bool) (*MediaHistoryResult, error) {
	input.Pair = append([]MediaArtifactInput(nil), input.Pair...)
	pair, bindingHash, fingerprint, err := mediaHistoryHashes(input)
	if err != nil {
		return nil, err
	}
	if requireConfirmation && (len(confirmation) != 64 || confirmation != strings.ToLower(confirmation) || confirmation != fingerprint) {
		return nil, ErrMediaHistoryConfirmation
	}
	var result MediaHistoryResult
	err = withMediaSource(input.SourceID, false, func(tx *gorm.DB, source *MediaSource) error {
		if requireConfirmation {
			_, _, currentFingerprint, err := mediaHistoryHashes(input)
			if err != nil || confirmation != currentFingerprint {
				return ErrMediaHistoryConfirmation
			}
		}
		if source.DocumentID != input.DocumentID || source.UserID != input.UserID || source.ActiveKey == nil ||
			*source.ActiveKey != mediaSourceKey(source.DocumentID, source.UserID) {
			return ErrMediaConflict
		}
		var receipt MediaHistoryReceipt
		err := tx.First(&receipt, "id = ?", input.ApprovalID).Error
		if err == nil {
			if receipt.FingerprintSHA256 != fingerprint || receipt.BindingSHA256 != bindingHash {
				return ErrMediaConflict
			}
			var set MediaSet
			if err := tx.First(&set, "id = ? AND source_id = ?", receipt.SetID, source.ID).Error; err != nil {
				return ErrMediaConflict
			}
			var artifacts []MediaArtifact
			if err := tx.Where("set_id = ?", set.ID).Order("role").Find(&artifacts).Error; err != nil {
				return err
			}
			if len(artifacts) != len(pair) {
				return ErrMediaConflict
			}
			for i := range pair {
				if artifacts[i].MediaArtifactInput != pair[i] {
					return ErrMediaConflict
				}
			}
			result = MediaHistoryResult{Receipt: receipt, Artifacts: artifacts, Replayed: true}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if requireConfirmation && confirmation != fingerprint {
			return ErrMediaHistoryConfirmation
		}
		if err := checkMediaHistoryRegistry(tx, *source, input, pair, bindingHash, fingerprint, &MediaHistoryCheck{}); err != nil {
			return err
		}
		if source.NextGeneration >= math.MaxInt64 {
			return errors.New("media generation exhausted")
		}
		generation := source.NextGeneration + 1
		attemptID := rand.Text()
		var videoJobID, audioJobID *string
		for i := range pair {
			jobID := pair[i].JobID
			if pair[i].Role == MediaVideo {
				videoJobID = &jobID
			} else {
				audioJobID = &jobID
			}
		}
		attempt := MediaAttempt{
			ID: attemptID, SourceID: source.ID, RequestID: "history:" + input.ApprovalID,
			RequestJSON: `{"historical":true}`, Generation: generation, PublicationGeneration: generation,
			State: MediaHistorical, VideoState: MediaHistorical, AudioState: MediaHistorical,
			VideoJobID: videoJobID, AudioJobID: audioJobID,
		}
		if err := tx.Create(&attempt).Error; err != nil {
			return err
		}
		set := MediaSet{ID: rand.Text(), SourceID: source.ID, AttemptID: attempt.ID, Generation: generation}
		if err := tx.Create(&set).Error; err != nil {
			return err
		}
		artifacts := make([]MediaArtifact, 0, len(pair))
		for _, candidate := range pair {
			artifact := MediaArtifact{ID: rand.Text(), SetID: set.ID, MediaArtifactInput: candidate}
			if err := tx.Create(&artifact).Error; err != nil {
				return err
			}
			artifacts = append(artifacts, artifact)
		}
		receipt = MediaHistoryReceipt{
			ID: input.ApprovalID, SourceID: source.ID, DocumentID: source.DocumentID,
			UserID: source.UserID, ApprovedBy: input.ApprovedBy, ApprovalJSON: input.ApprovalJSON,
			BindingSHA256: bindingHash, FingerprintSHA256: fingerprint, SetID: set.ID,
		}
		if err := tx.Create(&receipt).Error; err != nil {
			return err
		}
		if err := tx.Model(source).Update("next_generation", generation).Error; err != nil {
			return err
		}
		result = MediaHistoryResult{Receipt: receipt, Artifacts: artifacts}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}
