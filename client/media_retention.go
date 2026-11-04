// SPDX-License-Identifier: AGPL-3.0-or-later

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const mediaRetentionConfirmationHeader = "X-Hister-Media-Retention-Confirmation"

type MediaRetentionArtifact struct {
	ArtifactID string    `json:"artifact_id"`
	SourceID   string    `json:"source_id"`
	JobID      string    `json:"job_id"`
	Role       string    `json:"role"`
	Size       int64     `json:"size"`
	CreatedAt  time.Time `json:"created_at"`
	Retired    bool      `json:"retired"`
	FileStatus string    `json:"file_status"`
}

type MediaRetentionCheck struct {
	Before                    time.Time                `json:"before"`
	Artifacts                 []MediaRetentionArtifact `json:"artifacts"`
	SelectedArtifactsExcluded int64                    `json:"selected_artifacts_excluded"`
	RetireArtifacts           int                      `json:"retire_artifacts"`
	FilesToRemove             int                      `json:"files_to_remove"`
	MissingFiles              int                      `json:"missing_files"`
	BytesToRemove             int64                    `json:"bytes_to_remove"`
	BlockedFiles              int                      `json:"blocked_files"`
	ConfirmationSHA256        string                   `json:"confirmation_sha256"`
}

type MediaRetentionPendingFile struct {
	ArtifactID string `json:"artifact_id"`
	Code       string `json:"code"`
}

type MediaRetentionApplyResult struct {
	Before           time.Time                   `json:"before"`
	RetiredArtifacts int                         `json:"retired_artifacts"`
	RemovedFiles     int                         `json:"removed_files"`
	MissingFiles     int                         `json:"missing_files"`
	PendingFiles     []MediaRetentionPendingFile `json:"pending_files"`
}

type mediaRetentionRequest struct {
	Before time.Time `json:"before"`
}

func (c *Client) CheckMediaRetention(ctx context.Context, before time.Time) (*MediaRetentionCheck, error) {
	var result MediaRetentionCheck
	if err := c.mediaRetentionRequest(ctx, "/api/yt_downloader/retention/check", before, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) ApplyMediaRetention(ctx context.Context, before time.Time, confirmation string) (*MediaRetentionApplyResult, error) {
	var result MediaRetentionApplyResult
	if err := c.mediaRetentionRequest(ctx, "/api/yt_downloader/retention/apply", before, confirmation, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) mediaRetentionRequest(ctx context.Context, path string, before time.Time, confirmation string, target any) (err error) {
	if before.IsZero() || !before.Before(time.Now()) {
		return errors.New("media retention cutoff must be a timestamp in the past")
	}
	data, err := json.Marshal(mediaRetentionRequest{Before: before.UTC()})
	if err != nil {
		return err
	}
	req, err := c.newRequest(http.MethodPost, path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if confirmation != "" {
		req.Header.Set(mediaRetentionConfirmationHeader, confirmation)
	}
	resp, err := c.httpClient.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer closeBody(resp, &err)
	if err = decodeResponse(resp, target); err != nil {
		return fmt.Errorf("media retention request: %w", err)
	}
	return nil
}
