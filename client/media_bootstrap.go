// SPDX-License-Identifier: AGPL-3.0-or-later

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

const mediaSourceBootstrapConfirmationHeader = "X-Hister-Media-Source-Confirmation"

type MediaSourceBootstrapRequest struct {
	DocumentID string `json:"document_id"`
	OwnerID    uint   `json:"owner_id"`
	URL        string `json:"url"`
}

type MediaSourceBootstrapCheck struct {
	DocumentID          string `json:"document_id"`
	OwnerID             uint   `json:"owner_id"`
	URL                 string `json:"url"`
	ExistingSourceID    string `json:"existing_source_id,omitempty"`
	RevokedIncarnations int    `json:"revoked_incarnations"`
	ConfirmationSHA256  string `json:"confirmation_sha256"`
	Replayed            bool   `json:"replayed"`
}

type MediaSourceBootstrapResult struct {
	DocumentID string `json:"document_id"`
	OwnerID    uint   `json:"owner_id"`
	SourceID   string `json:"source_id"`
	Replayed   bool   `json:"replayed"`
}

func (c *Client) CheckMediaSourceBootstrap(ctx context.Context, request MediaSourceBootstrapRequest) (*MediaSourceBootstrapCheck, error) {
	var result MediaSourceBootstrapCheck
	if err := c.mediaSourceBootstrapRequest(ctx, "/api/yt_downloader/source/check", request, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) ApplyMediaSourceBootstrap(ctx context.Context, request MediaSourceBootstrapRequest, confirmation string) (*MediaSourceBootstrapResult, error) {
	var result MediaSourceBootstrapResult
	if err := c.mediaSourceBootstrapRequest(ctx, "/api/yt_downloader/source/apply", request, confirmation, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) mediaSourceBootstrapRequest(ctx context.Context, path string, payload MediaSourceBootstrapRequest, confirmation string, target any) (err error) {
	if payload.DocumentID == "" || payload.URL == "" || len(payload.DocumentID) > 4096 || len(payload.URL) > 4096 {
		return errors.New("media source bootstrap requires a document ID and URL")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := c.newRequest(http.MethodPost, path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if confirmation != "" {
		req.Header.Set(mediaSourceBootstrapConfirmationHeader, confirmation)
	}
	resp, err := c.httpClient.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer closeBody(resp, &err)
	if err = decodeResponse(resp, target); err != nil {
		return fmt.Errorf("media source bootstrap request: %w", err)
	}
	return nil
}
