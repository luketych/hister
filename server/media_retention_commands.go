// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"

	"github.com/rs/zerolog/log"
)

const mediaRetentionConfirmationHeader = "X-Hister-Media-Retention-Confirmation"

type mediaRetentionRequest struct {
	Before string `json:"before"`
}

type mediaRetentionArtifactResponse struct {
	ArtifactID string    `json:"artifact_id"`
	SourceID   string    `json:"source_id"`
	JobID      string    `json:"job_id"`
	Role       string    `json:"role"`
	Size       int64     `json:"size"`
	CreatedAt  time.Time `json:"created_at"`
	Retired    bool      `json:"retired"`
	FileStatus string    `json:"file_status"`
}

type mediaRetentionCheckResponse struct {
	Before                    time.Time                        `json:"before"`
	Artifacts                 []mediaRetentionArtifactResponse `json:"artifacts"`
	SelectedArtifactsExcluded int64                            `json:"selected_artifacts_excluded"`
	RetireArtifacts           int                              `json:"retire_artifacts"`
	FilesToRemove             int                              `json:"files_to_remove"`
	MissingFiles              int                              `json:"missing_files"`
	BytesToRemove             int64                            `json:"bytes_to_remove"`
	BlockedFiles              int                              `json:"blocked_files"`
	ConfirmationSHA256        string                           `json:"confirmation_sha256"`
}

type mediaRetentionPendingFile struct {
	ArtifactID string `json:"artifact_id"`
	Code       string `json:"code"`
}

type mediaRetentionApplyResponse struct {
	Before           time.Time                   `json:"before"`
	RetiredArtifacts int                         `json:"retired_artifacts"`
	RemovedFiles     int                         `json:"removed_files"`
	MissingFiles     int                         `json:"missing_files"`
	PendingFiles     []mediaRetentionPendingFile `json:"pending_files"`
}

type mediaRetentionFile struct {
	artifact model.MediaRetentionArtifact
	status   string
	info     os.FileInfo
	sha256   string
}

type mediaRetentionFileFingerprint struct {
	ArtifactID string
	Status     string
	Size       int64
	ModTime    int64
	Mode       uint32
	SHA256     string
}

type mediaRetentionConfirmation struct {
	Registry string
	Files    []mediaRetentionFileFingerprint
}

type preparedMediaRetention struct {
	registry *model.MediaRetentionPlan
	files    []mediaRetentionFile
	response mediaRetentionCheckResponse
}

func decodeMediaRetentionRequest(c *webContext) (time.Time, bool) {
	if c.Request.Method != http.MethodPost {
		c.Response.Header().Set("Allow", http.MethodPost)
		http.Error(c.Response, "POST required", http.StatusMethodNotAllowed)
		return time.Time{}, false
	}
	data, err := io.ReadAll(http.MaxBytesReader(c.Response, c.Request.Body, maxMediaCommandBytes))
	if err != nil || len(data) == 0 || int64(len(data)) > maxMediaCommandBytes {
		http.Error(c.Response, "invalid media retention request", http.StatusBadRequest)
		return time.Time{}, false
	}
	keyDecoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeMediaBootstrapJSONValue(keyDecoder); err != nil {
		http.Error(c.Response, "invalid media retention request", http.StatusBadRequest)
		return time.Time{}, false
	}
	if _, err := keyDecoder.Token(); err != io.EOF {
		http.Error(c.Response, "expected one media retention object", http.StatusBadRequest)
		return time.Time{}, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 1 || fields["before"] == nil {
		http.Error(c.Response, "invalid media retention request", http.StatusBadRequest)
		return time.Time{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request mediaRetentionRequest
	if decoder.Decode(&request) != nil || decoder.Decode(new(json.RawMessage)) != io.EOF || strings.TrimSpace(request.Before) == "" {
		http.Error(c.Response, "invalid media retention request", http.StatusBadRequest)
		return time.Time{}, false
	}
	before, err := time.Parse(time.RFC3339Nano, request.Before)
	if err != nil || !before.Before(time.Now().UTC()) {
		http.Error(c.Response, "before must be an RFC3339 timestamp in the past", http.StatusBadRequest)
		return time.Time{}, false
	}
	c.Response.Header().Set("Cache-Control", "no-store")
	return before.UTC(), true
}

func prepareMediaRetention(ctx context.Context, before time.Time) (*preparedMediaRetention, error) {
	registry, err := model.CheckMediaRetention(before)
	if err != nil {
		return nil, err
	}
	prepared := &preparedMediaRetention{
		registry: registry,
		files:    make([]mediaRetentionFile, 0, len(registry.Artifacts)),
		response: mediaRetentionCheckResponse{
			Before: registry.Before, Artifacts: make([]mediaRetentionArtifactResponse, 0, len(registry.Artifacts)),
			SelectedArtifactsExcluded: registry.SelectedArtifactsExcluded,
		},
	}
	fingerprint := mediaRetentionConfirmation{Registry: registry.RegistryConfirmationSHA256}
	for _, item := range registry.Artifacts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if item.Artifact.RetiredAt == nil {
			prepared.response.RetireArtifacts++
		}
		candidate := mediaRetentionFile{artifact: item}
		file, info, err := media.OpenArtifact(item.Artifact.MediaArtifactInput)
		switch {
		case err == nil:
			digest, hashErr := media.HashArtifact(ctx, file, item.Artifact.Size)
			closeErr := file.Close()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if hashErr != nil || closeErr != nil {
				candidate.status = "blocked"
			} else {
				candidate.status, candidate.info, candidate.sha256 = "ready", info, digest
			}
		case errors.Is(err, os.ErrNotExist):
			candidate.status = "missing"
		default:
			candidate.status = "blocked"
		}
		prepared.files = append(prepared.files, candidate)
		fileState := mediaRetentionFileFingerprint{ArtifactID: item.Artifact.ID, Status: candidate.status}
		if candidate.info != nil {
			fileState.Size = candidate.info.Size()
			fileState.ModTime = candidate.info.ModTime().UnixNano()
			fileState.Mode = uint32(candidate.info.Mode())
			fileState.SHA256 = candidate.sha256
			prepared.response.FilesToRemove++
			prepared.response.BytesToRemove += candidate.info.Size()
		}
		fingerprint.Files = append(fingerprint.Files, fileState)
		if candidate.status == "missing" {
			prepared.response.MissingFiles++
		}
		if candidate.status == "blocked" {
			prepared.response.BlockedFiles++
		}
		prepared.response.Artifacts = append(prepared.response.Artifacts, mediaRetentionArtifactResponse{
			ArtifactID: item.Artifact.ID, SourceID: item.SourceID, JobID: item.Artifact.JobID,
			Role: item.Artifact.Role, Size: item.Artifact.Size, CreatedAt: item.SetCreatedAt,
			Retired: item.Artifact.RetiredAt != nil, FileStatus: candidate.status,
		})
	}
	encoded, err := json.Marshal(fingerprint)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	prepared.response.ConfirmationSHA256 = hex.EncodeToString(digest[:])
	return prepared, nil
}

func mediaRetentionCommandError(c *webContext, err error) {
	switch {
	case errors.Is(err, model.ErrMediaRetentionConfirmation):
		c.JSONStatus(http.StatusConflict, map[string]string{"code": "confirmation_mismatch", "error": "media retention plan changed; run the check again"})
	case errors.Is(err, model.ErrMediaRetentionTooMany):
		c.JSONStatus(http.StatusRequestEntityTooLarge, map[string]string{"code": "retention_plan_too_large", "error": "media retention plan exceeds the per-operation limit; use a narrower cutoff"})
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		http.Error(c.Response, "media retention request cancelled", http.StatusRequestTimeout)
	default:
		log.Error().Err(err).Msg("media retention operation failed")
		c.JSONStatus(http.StatusServiceUnavailable, map[string]string{"code": "retention_unavailable", "error": "media retention is unavailable"})
	}
}

func serveMediaRetentionCheck(c *webContext) {
	if _, ok := historyOperator(c); !ok {
		return
	}
	before, ok := decodeMediaRetentionRequest(c)
	if !ok {
		return
	}
	prepared, err := prepareMediaRetention(c.Request.Context(), before)
	if err != nil {
		mediaRetentionCommandError(c, err)
		return
	}
	c.JSON(prepared.response)
}

func serveMediaRetentionApply(c *webContext) {
	if _, ok := historyOperator(c); !ok {
		return
	}
	before, ok := decodeMediaRetentionRequest(c)
	if !ok {
		return
	}
	prepared, err := prepareMediaRetention(c.Request.Context(), before)
	if err != nil {
		mediaRetentionCommandError(c, err)
		return
	}
	if prepared.response.ConfirmationSHA256 != c.Request.Header.Get(mediaRetentionConfirmationHeader) {
		mediaRetentionCommandError(c, model.ErrMediaRetentionConfirmation)
		return
	}
	retired, err := model.RetireMediaForRetention(before, prepared.registry.RegistryConfirmationSHA256)
	if err != nil {
		mediaRetentionCommandError(c, err)
		return
	}
	filesByID := make(map[string]mediaRetentionFile, len(prepared.files))
	for _, candidate := range prepared.files {
		filesByID[candidate.artifact.Artifact.ID] = candidate
	}
	response := mediaRetentionApplyResponse{
		Before:       prepared.registry.Before,
		PendingFiles: make([]mediaRetentionPendingFile, 0),
	}
	for _, item := range retired {
		candidate, ok := filesByID[item.Artifact.ID]
		if !ok {
			response.PendingFiles = append(response.PendingFiles, mediaRetentionPendingFile{ArtifactID: item.Artifact.ID, Code: "plan_changed"})
			continue
		}
		if candidate.artifact.Artifact.RetiredAt == nil {
			response.RetiredArtifacts++
		}
		if candidate.status == "blocked" {
			response.PendingFiles = append(response.PendingFiles, mediaRetentionPendingFile{ArtifactID: item.Artifact.ID, Code: "file_changed_or_unavailable"})
			log.Warn().Str("artifact_id", item.Artifact.ID).Msg("retired media file awaits safe cleanup")
			continue
		}
		if candidate.status == "missing" {
			response.MissingFiles++
			if err := model.MarkMediaArtifactBytesRemoved(item.Artifact.ID); err != nil {
				response.PendingFiles = append(response.PendingFiles, mediaRetentionPendingFile{ArtifactID: item.Artifact.ID, Code: "removal_state_unrecorded"})
			}
			continue
		}
		err := media.RemoveArtifact(item.Artifact.MediaArtifactInput, candidate.info, candidate.sha256)
		switch {
		case err == nil:
			response.RemovedFiles++
			if err := model.MarkMediaArtifactBytesRemoved(item.Artifact.ID); err != nil {
				response.PendingFiles = append(response.PendingFiles, mediaRetentionPendingFile{ArtifactID: item.Artifact.ID, Code: "removal_state_unrecorded"})
			}
		case errors.Is(err, os.ErrNotExist):
			response.MissingFiles++
			if err := model.MarkMediaArtifactBytesRemoved(item.Artifact.ID); err != nil {
				response.PendingFiles = append(response.PendingFiles, mediaRetentionPendingFile{ArtifactID: item.Artifact.ID, Code: "removal_state_unrecorded"})
			}
		default:
			response.PendingFiles = append(response.PendingFiles, mediaRetentionPendingFile{ArtifactID: item.Artifact.ID, Code: "file_changed_or_unavailable"})
			log.Warn().Str("artifact_id", item.Artifact.ID).Msg("retired media file awaits safe cleanup")
		}
	}
	c.JSON(response)
}
