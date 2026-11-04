// SPDX-License-Identifier: AGPL-3.0-or-later

package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
)

const mediaHistoryConfirmationHeader = "X-Hister-Media-Confirmation"

// MediaHistoryCheck is a read-only server preflight result. The confirmation
// hash detects stale plans; it is neither a reservation nor an authorization credential.
type MediaHistoryCheck struct {
	ApprovalID         string `json:"approval_id"`
	ConfirmationSHA256 string `json:"confirmation_sha256"`
	Replayed           bool   `json:"replayed"`
}

type MediaHistoryApplyResult struct {
	ApprovalID string `json:"approval_id"`
	ReceiptID  string `json:"receipt_id"`
	SetID      string `json:"set_id"`
	Replayed   bool   `json:"replayed"`
}

func (c *Client) CheckMediaHistory(ctx context.Context, approval []byte) (*MediaHistoryCheck, error) {
	var result MediaHistoryCheck
	if err := c.mediaHistoryRequest(ctx, "/api/yt_downloader/history/check", approval, "", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) ApplyMediaHistory(ctx context.Context, approval []byte, confirmation string) (*MediaHistoryApplyResult, error) {
	var result MediaHistoryApplyResult
	if err := c.mediaHistoryRequest(ctx, "/api/yt_downloader/history/apply", approval, confirmation, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) mediaHistoryRequest(ctx context.Context, path string, approval []byte, confirmation string, target any) (err error) {
	if len(approval) == 0 || len(approval) > 64<<10 {
		return errors.New("historical media approval size is invalid")
	}
	req, err := c.newRequest(http.MethodPost, path, bytes.NewReader(approval))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if confirmation != "" {
		req.Header.Set(mediaHistoryConfirmationHeader, confirmation)
	}
	resp, err := c.httpClient.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer closeBody(resp, &err)
	if err = decodeResponse(resp, target); err != nil {
		return fmt.Errorf("historical media request: %w", err)
	}
	return nil
}
