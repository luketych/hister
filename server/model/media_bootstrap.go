// SPDX-License-Identifier: AGPL-3.0-or-later

package model

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

var ErrMediaSourceBootstrapConfirmation = errors.New("media source bootstrap confirmation does not match current plan")

type MediaSourceBootstrapInput struct {
	DocumentID string
	UserID     uint
	URL        string
}

type MediaSourceBootstrapCheck struct {
	DocumentID          string
	UserID              uint
	URL                 string
	ExistingSourceID    string
	RevokedIncarnations int
	ConfirmationSHA256  string
	Replayed            bool
}

type MediaSourceBootstrapResult struct {
	Source   MediaSource
	Replayed bool
}

type mediaSourceBootstrapFingerprint struct {
	Input   MediaSourceBootstrapInput
	Sources []mediaSourceBootstrapState
}

type mediaSourceBootstrapState struct {
	ID                 string
	DocumentID         string
	UserID             uint
	ActiveKey          *string
	NextGeneration     uint64
	AcceptedGeneration uint64
	SelectedSetID      string
	Suspended          bool
	PendingDocumentID  string
	PendingUserID      uint
	RevokedAt          *time.Time
}

func validMediaSourceBootstrapInput(input MediaSourceBootstrapInput) error {
	if strings.TrimSpace(input.DocumentID) == "" || strings.TrimSpace(input.URL) == "" {
		return errors.New("media source bootstrap requires an indexed document ID and URL")
	}
	return nil
}

func mediaSourceBootstrapPlan(tx *gorm.DB, input MediaSourceBootstrapInput) (*MediaSourceBootstrapCheck, error) {
	if err := validMediaSourceBootstrapInput(input); err != nil {
		return nil, err
	}
	var sources []MediaSource
	if err := tx.Where("document_id = ?", input.DocumentID).Order("id ASC").Find(&sources).Error; err != nil {
		return nil, err
	}
	activeKey := mediaSourceKey(input.DocumentID, input.UserID)
	result := &MediaSourceBootstrapCheck{
		DocumentID: input.DocumentID,
		UserID:     input.UserID,
		URL:        input.URL,
	}
	states := make([]mediaSourceBootstrapState, 0, len(sources))
	activeCount := 0
	for _, source := range sources {
		if source.UserID != input.UserID {
			return nil, ErrMediaConflict
		}
		states = append(states, mediaSourceBootstrapState{
			ID: source.ID, DocumentID: source.DocumentID, UserID: source.UserID,
			ActiveKey: source.ActiveKey, NextGeneration: source.NextGeneration,
			AcceptedGeneration: source.AcceptedGeneration, SelectedSetID: source.SelectedSetID,
			Suspended: source.Suspended, PendingDocumentID: source.PendingDocumentID,
			PendingUserID: source.PendingUserID, RevokedAt: source.RevokedAt,
		})
		if source.RevokedAt != nil {
			result.RevokedIncarnations++
			if source.ActiveKey != nil {
				return nil, ErrMediaConflict
			}
			continue
		}
		activeCount++
		if activeCount > 1 || source.Suspended || source.ActiveKey == nil || *source.ActiveKey != activeKey {
			return nil, ErrMediaConflict
		}
		result.ExistingSourceID = source.ID
		result.Replayed = true
	}
	fingerprintBytes, err := json.Marshal(mediaSourceBootstrapFingerprint{Input: input, Sources: states})
	if err != nil {
		return nil, err
	}
	fingerprint := sha256.Sum256(fingerprintBytes)
	result.ConfirmationSHA256 = hex.EncodeToString(fingerprint[:])
	return result, nil
}

// CheckMediaSourceBootstrap returns a read-only plan for establishing a source
// incarnation for a document already verified by the indexer. It never creates
// rows or revives a revoked source.
func CheckMediaSourceBootstrap(input MediaSourceBootstrapInput) (*MediaSourceBootstrapCheck, error) {
	if err := validMediaSourceBootstrapInput(input); err != nil {
		return nil, err
	}
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	var result *MediaSourceBootstrapCheck
	err = db.Transaction(func(tx *gorm.DB) error {
		var err error
		result, err = mediaSourceBootstrapPlan(tx, input)
		return err
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ApplyMediaSourceBootstrap creates one fresh source incarnation only after a
// check fingerprint matches the current indexed binding and registry state.
// The source table lock serializes bootstraps even when no source row exists;
// the unique active key remains the final cross-process race defense.
func ApplyMediaSourceBootstrap(input MediaSourceBootstrapInput, confirmation string) (*MediaSourceBootstrapResult, error) {
	if err := validMediaSourceBootstrapInput(input); err != nil {
		return nil, err
	}
	if len(confirmation) != 64 || confirmation != strings.ToLower(confirmation) {
		return nil, ErrMediaSourceBootstrapConfirmation
	}
	if _, err := hex.DecodeString(confirmation); err != nil {
		return nil, ErrMediaSourceBootstrapConfirmation
	}
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	var result MediaSourceBootstrapResult
	err = db.Transaction(func(tx *gorm.DB) error {
		switch tx.Name() {
		case "postgres":
			if err := tx.Exec("LOCK TABLE media_sources IN SHARE ROW EXCLUSIVE MODE").Error; err != nil {
				return err
			}
		case "sqlite":
			if err := tx.Exec("UPDATE media_sources SET updated_at = updated_at WHERE 1 = 0").Error; err != nil {
				return err
			}
		default:
			return ErrDBType
		}
		current, err := mediaSourceBootstrapPlan(tx, input)
		if err != nil {
			return err
		}
		if current.ConfirmationSHA256 != confirmation {
			return ErrMediaSourceBootstrapConfirmation
		}
		if current.Replayed {
			if err := tx.First(&result.Source, "id = ?", current.ExistingSourceID).Error; err != nil {
				return err
			}
			result.Replayed = true
			return nil
		}
		activeKey := mediaSourceKey(input.DocumentID, input.UserID)
		source := MediaSource{
			ID: rand.Text(), DocumentID: input.DocumentID, UserID: input.UserID,
			ActiveKey: &activeKey, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := tx.Create(&source).Error; err != nil {
			return err
		}
		result.Source = source
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}
