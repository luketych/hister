// SPDX-License-Identifier: AGPL-3.0-or-later

package media

import (
	"errors"

	"github.com/asciimoo/hister/server/model"

	"gorm.io/gorm"
)

// Status is a read-only public projection, never a scheduling or authorization
// input. It contains no filesystem/configuration paths, claim tokens or logs.
type Status struct {
	DocumentID  string           `json:"document_id"`
	Available   []ArtifactStatus `json:"available"`
	Attempt     *AttemptStatus   `json:"attempt"`
	Active      bool             `json:"active"`
	Unavailable string           `json:"unavailable,omitempty"`
}

type ArtifactStatus struct {
	ID    string `json:"id"`
	Role  string `json:"role"`
	State string `json:"state"`
}

type AttemptStatus struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
	State      string `json:"state"`
	Video      string `json:"video"`
	Audio      string `json:"audio"`
	ErrorCode  string `json:"error_code,omitempty"`
}

// ReadStatus requires prior source-document authorization. Merely reading
// status cannot accept work, touch a reservation, or activate discovered files.
func ReadStatus(documentID string, userID uint) (*Status, error) {
	status := &Status{DocumentID: documentID, Available: []ArtifactStatus{}}
	snapshot, err := model.ReadMediaSnapshot(documentID, userID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return status, nil
	}
	if err != nil {
		return nil, err
	}
	if snapshot.Source.Suspended {
		status.Unavailable = "ownership_change_incomplete"
		return status, nil
	}
	status.Active = snapshot.Active
	if attempt := snapshot.Attempt; attempt != nil {
		status.Attempt = &AttemptStatus{
			ID: attempt.ID, Generation: max(attempt.Generation, attempt.PublicationGeneration),
			State: attempt.State, Video: attempt.VideoState, Audio: attempt.AudioState,
		}
		switch attempt.ErrorCode {
		case "start_failed", "download_failed", "validation_failed", "publication_failed", "cancelled", "interrupted", "index_write_failed", "source_deleted", "ownership_changed", "dispatcher_disabled":
			status.Attempt.ErrorCode = attempt.ErrorCode
		}
	}
	for _, artifact := range snapshot.Artifacts {
		state := "missing"
		if artifact.RetiredAt != nil {
			state = "retired"
		} else {
			file, _, err := OpenArtifact(artifact.MediaArtifactInput)
			if err == nil {
				state = model.MediaReady
				_ = file.Close()
			}
		}
		status.Available = append(status.Available, ArtifactStatus{ID: artifact.ID, Role: artifact.Role, State: state})
	}
	return status, nil
}
