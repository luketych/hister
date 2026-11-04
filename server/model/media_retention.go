// SPDX-License-Identifier: AGPL-3.0-or-later

package model

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

var (
	ErrMediaRetentionConfirmation = errors.New("media retention plan changed; check again")
	ErrMediaRetentionTooMany      = errors.New("media retention plan exceeds the per-operation limit")
)

const maxMediaRetentionArtifacts = 1000

type MediaRetentionArtifact struct {
	Artifact      MediaArtifact
	SourceID      string
	SelectedSetID string
	SetCreatedAt  time.Time
}

type MediaRetentionPlan struct {
	Before                     time.Time
	Artifacts                  []MediaRetentionArtifact
	SelectedArtifactsExcluded  int64
	RegistryConfirmationSHA256 string
}

type mediaRetentionRow struct {
	MediaArtifact
	SourceID      string    `gorm:"column:source_id"`
	SelectedSetID string    `gorm:"column:selected_set_id"`
	SetCreatedAt  time.Time `gorm:"column:set_created_at"`
}

type mediaRetentionFingerprint struct {
	Before                    time.Time
	SelectedArtifactsExcluded int64
	Artifacts                 []mediaRetentionState
}

type mediaRetentionState struct {
	ID             string
	SetID          string
	SourceID       string
	SelectedSetID  string
	Role           string
	JobID          string
	StorageRoot    string
	RelativePath   string
	Size           int64
	ContentType    string
	RetiredAt      *time.Time
	BytesRemovedAt *time.Time
	CreatedAt      time.Time
	SetCreatedAt   time.Time
}

func validateMediaRetentionCutoff(before time.Time) error {
	if before.IsZero() || !before.Before(time.Now().UTC()) {
		return errors.New("media retention cutoff must be a timestamp in the past")
	}
	return nil
}

func mediaRetentionPlan(tx *gorm.DB, before time.Time) (*MediaRetentionPlan, error) {
	before = before.UTC()
	var selected int64
	if err := tx.Table("media_artifacts").
		Joins("JOIN media_sets ON media_sets.id = media_artifacts.set_id").
		Joins("JOIN media_sources ON media_sources.id = media_sets.source_id").
		Where("media_artifacts.bytes_removed_at IS NULL AND media_sets.created_at < ? AND media_sources.selected_set_id = media_sets.id", before).
		Count(&selected).Error; err != nil {
		return nil, err
	}
	var rows []mediaRetentionRow
	if err := tx.Table("media_artifacts").
		Select("media_artifacts.*, media_sets.source_id AS source_id, media_sources.selected_set_id AS selected_set_id, media_sets.created_at AS set_created_at").
		Joins("JOIN media_sets ON media_sets.id = media_artifacts.set_id").
		Joins("JOIN media_sources ON media_sources.id = media_sets.source_id").
		Where("COALESCE(media_sources.selected_set_id, '') <> media_sets.id").
		Where("media_artifacts.bytes_removed_at IS NULL").
		Where("(media_artifacts.retired_at IS NOT NULL OR media_sets.created_at < ?)", before).
		Order("media_artifacts.id ASC").Limit(maxMediaRetentionArtifacts + 1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) > maxMediaRetentionArtifacts {
		return nil, ErrMediaRetentionTooMany
	}
	plan := &MediaRetentionPlan{
		Before: before, SelectedArtifactsExcluded: selected,
		Artifacts: make([]MediaRetentionArtifact, 0, len(rows)),
	}
	states := make([]mediaRetentionState, 0, len(rows))
	for _, row := range rows {
		plan.Artifacts = append(plan.Artifacts, MediaRetentionArtifact{
			Artifact: row.MediaArtifact, SourceID: row.SourceID, SelectedSetID: row.SelectedSetID,
			SetCreatedAt: row.SetCreatedAt,
		})
		states = append(states, mediaRetentionState{
			ID: row.ID, SetID: row.SetID, SourceID: row.SourceID, SelectedSetID: row.SelectedSetID,
			Role: row.Role, JobID: row.JobID, StorageRoot: row.StorageRoot,
			RelativePath: row.RelativePath, Size: row.Size, ContentType: row.ContentType,
			RetiredAt: row.RetiredAt, BytesRemovedAt: row.BytesRemovedAt,
			CreatedAt: row.CreatedAt, SetCreatedAt: row.SetCreatedAt,
		})
	}
	encoded, err := json.Marshal(mediaRetentionFingerprint{Before: before, SelectedArtifactsExcluded: selected, Artifacts: states})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	plan.RegistryConfirmationSHA256 = hex.EncodeToString(digest[:])
	return plan, nil
}

// CheckMediaRetention returns a read-only plan for an explicitly requested
// cleanup. It never prunes the currently selected set. Already-retired files
// remain eligible so interrupted file removal can be safely retried.
func CheckMediaRetention(before time.Time) (*MediaRetentionPlan, error) {
	if err := validateMediaRetentionCutoff(before); err != nil {
		return nil, err
	}
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	var plan *MediaRetentionPlan
	err = db.Transaction(func(tx *gorm.DB) error {
		var err error
		plan, err = mediaRetentionPlan(tx, before)
		return err
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	return plan, nil
}

// RetireMediaForRetention atomically tombstones the exact checked set before
// any filesystem removal. Source-row locks prevent an old set from becoming
// selected while it is being retired. Filesystem removal is a separate,
// retryable step performed only after this transaction commits.
func RetireMediaForRetention(before time.Time, confirmation string) ([]MediaRetentionArtifact, error) {
	if err := validateMediaRetentionCutoff(before); err != nil {
		return nil, err
	}
	if len(confirmation) != 64 || confirmation != strings.ToLower(confirmation) {
		return nil, ErrMediaRetentionConfirmation
	}
	if _, err := hex.DecodeString(confirmation); err != nil {
		return nil, ErrMediaRetentionConfirmation
	}
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	var retired []MediaRetentionArtifact
	err = db.Transaction(func(tx *gorm.DB) error {
		initial, err := mediaRetentionPlan(tx, before)
		if err != nil {
			return err
		}
		sourceIDs := make([]string, 0, len(initial.Artifacts))
		seen := make(map[string]struct{}, len(initial.Artifacts))
		for _, item := range initial.Artifacts {
			if _, exists := seen[item.SourceID]; exists {
				continue
			}
			seen[item.SourceID] = struct{}{}
			sourceIDs = append(sourceIDs, item.SourceID)
		}
		sort.Strings(sourceIDs)
		for _, sourceID := range sourceIDs {
			result := tx.Exec("UPDATE media_sources SET updated_at = updated_at WHERE id = ?", sourceID)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return gorm.ErrRecordNotFound
			}
		}
		current, err := mediaRetentionPlan(tx, before)
		if err != nil {
			return err
		}
		if current.RegistryConfirmationSHA256 != confirmation {
			return ErrMediaRetentionConfirmation
		}
		now := time.Now().UTC()
		for n := range current.Artifacts {
			item := &current.Artifacts[n]
			if item.Artifact.RetiredAt != nil {
				continue
			}
			updated := tx.Model(&MediaArtifact{}).Where("id = ? AND retired_at IS NULL", item.Artifact.ID).Update("retired_at", now)
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected != 1 {
				return ErrMediaRetentionConfirmation
			}
			item.Artifact.RetiredAt = &now
		}
		retired = current.Artifacts
		return nil
	})
	if err != nil {
		return nil, err
	}
	return retired, nil
}

// MarkMediaArtifactBytesRemoved records successful or already-absent file
// cleanup while retaining the permanent playback-denial tombstone. The caller
// must invoke it only after verifying that the registered path is absent.
func MarkMediaArtifactBytesRemoved(id string) error {
	db, err := mediaDB()
	if err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var item mediaRetentionRow
		if err := tx.Table("media_artifacts").
			Select("media_artifacts.*, media_sets.source_id AS source_id, media_sources.selected_set_id AS selected_set_id, media_sets.created_at AS set_created_at").
			Joins("JOIN media_sets ON media_sets.id = media_artifacts.set_id").
			Joins("JOIN media_sources ON media_sources.id = media_sets.source_id").
			Where("media_artifacts.id = ?", id).First(&item).Error; err != nil {
			return err
		}
		if item.BytesRemovedAt != nil {
			return nil
		}
		if item.RetiredAt == nil {
			return ErrMediaConflict
		}
		locked := tx.Exec("UPDATE media_sources SET updated_at = updated_at WHERE id = ?", item.SourceID)
		if locked.Error != nil {
			return locked.Error
		}
		if locked.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		var source MediaSource
		if err := tx.Select("id", "selected_set_id").First(&source, "id = ?", item.SourceID).Error; err != nil {
			return err
		}
		if source.SelectedSetID == item.SetID {
			return ErrMediaConflict
		}
		removedAt := time.Now().UTC()
		result := tx.Model(&MediaArtifact{}).Where("id = ? AND retired_at IS NOT NULL AND bytes_removed_at IS NULL", id).Update("bytes_removed_at", removedAt)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var current MediaArtifact
			if err := tx.First(&current, "id = ?", id).Error; err != nil {
				return err
			}
			if current.BytesRemovedAt != nil {
				return nil
			}
			return ErrMediaRetentionConfirmation
		}
		return nil
	})
}
