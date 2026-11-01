// SPDX-License-Identifier: AGPL-3.0-or-later

package model

import (
	"database/sql"
	"errors"

	"gorm.io/gorm"
)

// MediaSnapshot reads selection, attempt state and retained records from one
// SQL snapshot. Otherwise publication between reads could appear terminal but
// omit its new artifacts, making a polling client stop before seeing them.
// The service must still check current file availability and source visibility.
type MediaSnapshot struct {
	Source    MediaSource
	Attempt   *MediaAttempt
	Artifacts []MediaArtifact
	Active    bool
}

func ReadMediaSnapshot(documentID string, userID uint) (*MediaSnapshot, error) {
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	snapshot := &MediaSnapshot{}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("document_id = ? AND user_id = ? AND revoked_at IS NULL", documentID, userID).First(&snapshot.Source).Error; err != nil {
			return err
		}
		if snapshot.Source.Suspended {
			return nil
		}
		var attempt MediaAttempt
		err := tx.Where("source_id = ? AND state <> ? AND error_code <> ?", snapshot.Source.ID, MediaPendingCommit, "coalesced").Order("CASE WHEN publication_generation > generation THEN publication_generation ELSE generation END DESC").First(&attempt).Error
		if err == nil {
			snapshot.Attempt = &attempt
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var count int64
		if err := tx.Model(&MediaAttempt{}).Where("source_id = ? AND state IN ?", snapshot.Source.ID,
			[]string{MediaQueued, MediaStarting, MediaDownloading, MediaFinalizing}).Count(&count).Error; err != nil {
			return err
		}
		snapshot.Active = count > 0
		if snapshot.Source.SelectedSetID == "" {
			return nil
		}
		return tx.Where("set_id = ?", snapshot.Source.SelectedSetID).Order("role").Find(&snapshot.Artifacts).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}
