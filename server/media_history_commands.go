// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/files"
	yt "github.com/asciimoo/hister/server/extractor/extractors/yt_downloader"
	"github.com/asciimoo/hister/server/indexer"
	"github.com/asciimoo/hister/server/model"

	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
)

func mediaHistoryApprovalSchema() []*JSONSchemaField {
	return []*JSONSchemaField{
		{Name: "schema_version", Type: "integer", Required: true, Description: "Approval schema version; currently 1"},
		{Name: "approval_id", Type: "string", Required: true, Description: "Operator-assigned idempotent approval identity"},
		{Name: "action", Type: "string", Required: true, Description: "Must be retain_history"},
		{Name: "source", Type: "object", Required: true, Description: "Pinned existing source incarnation and owner", Fields: []*JSONSchemaField{
			{Name: "source_id", Type: "string", Required: true, Description: "Existing source incarnation ID"},
			{Name: "document_id", Type: "string", Required: true, Description: "Exact committed document ID"},
			{Name: "owner_id", Type: "integer", Required: true, Description: "Explicit owner user ID (zero for single-user mode)"},
			{Name: "url", Type: "string", Required: true, Description: "Exact indexed source URL"},
		}},
		{Name: "video", Type: "object", Required: true, Description: "Approved video job", Fields: []*JSONSchemaField{
			{Name: "job_id", Type: "string", Required: true, Description: "Exact historical video job ID"},
			{Name: "evidence_sha256", Type: "string", Required: true, Description: "Digest of the reviewed reports and provenance"},
			{Name: "media_sha256", Type: "string", Required: true, Description: "Digest of the exact reviewed media bytes"},
		}},
		{Name: "audio", Type: "object", Required: true, Description: "Approved audio job", Fields: []*JSONSchemaField{
			{Name: "job_id", Type: "string", Required: true, Description: "Exact historical audio job ID"},
			{Name: "evidence_sha256", Type: "string", Required: true, Description: "Digest of the reviewed reports and provenance"},
			{Name: "media_sha256", Type: "string", Required: true, Description: "Digest of the exact reviewed media bytes"},
		}},
	}
}

type mediaHistoryCheckResponse struct {
	ApprovalID         string `json:"approval_id"`
	ConfirmationSHA256 string `json:"confirmation_sha256"`
	Replayed           bool   `json:"replayed"`
}

type mediaHistoryApplyResponse struct {
	ApprovalID string `json:"approval_id"`
	ReceiptID  string `json:"receipt_id"`
	SetID      string `json:"set_id"`
	Replayed   bool   `json:"replayed"`
}

func historyOperator(c *webContext) (indexer.MediaActor, bool) {
	if c.Config.App.UserHandling {
		if c.UserID == 0 || !c.IsAdmin {
			serve403(c)
			return indexer.MediaActor{}, false
		}
		return indexer.MediaActor{UserID: c.UserID, IsAdmin: true}, true
	}
	// Public unauthenticated deployments do not have a trustworthy local
	// administrator identity. A configured access token is the operator
	// credential in single-user deployments.
	if c.Config.App.Public && c.Config.App.AccessToken == "" {
		serve403(c)
		return indexer.MediaActor{}, false
	}
	if c.Config.App.AccessToken != "" && !c.Authenticated {
		serve403(c)
		return indexer.MediaActor{}, false
	}
	return indexer.MediaActor{UserID: c.UserID, IsAdmin: true}, true
}

func historyRoots(c *config.Config) (string, string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", "", err
	}
	state := filepath.Join(cache, "hister", "yt_video_downloader")

	output := ""
	if extractor := c.Extractors[config.YTDownloaderName]; extractor != nil {
		if configured, ok := extractor.Options["output_dir"]; ok {
			var valid bool
			output, valid = configured.(string)
			if !valid || output == "" {
				return "", "", errors.New("invalid configured media output directory")
			}
		}
	}
	if output == "" {
		defaults := (&yt.YTDownloader{}).GetConfig()
		output, _ = defaults.Options["output_dir"].(string)
	}
	if output == "" {
		return "", "", errors.New("media output directory is not configured")
	}
	return state, files.ExpandHome(output), nil
}

func decodeMediaHistoryApproval(c *webContext) (*yt.HistoryApproval, bool) {
	if c.Request.Method != http.MethodPost {
		c.Response.Header().Set("Allow", http.MethodPost)
		http.Error(c.Response, "POST required", http.StatusMethodNotAllowed)
		return nil, false
	}
	approval, err := yt.DecodeHistoryApproval(http.MaxBytesReader(c.Response, c.Request.Body, maxMediaCommandBytes))
	if err != nil {
		http.Error(c.Response, "invalid historical media approval", http.StatusBadRequest)
		return nil, false
	}
	c.Response.Header().Set("Cache-Control", "no-store")
	return approval, true
}

func mediaHistoryCommandError(c *webContext, err error) {
	switch {
	case errors.Is(err, indexer.ErrMediaHistoryForbidden):
		serve403(c)
	case errors.Is(err, gorm.ErrRecordNotFound), errors.Is(err, model.ErrMediaSourceRevoked):
		http.NotFound(c.Response, c.Request)
	case errors.Is(err, model.ErrMediaConflict):
		c.JSONStatus(http.StatusConflict, map[string]string{"code": "history_conflict", "error": "historical media approval no longer matches the live source or registry"})
	case errors.Is(err, indexer.ErrMediaHistoryConfirmation):
		c.JSONStatus(http.StatusConflict, map[string]string{"code": "confirmation_mismatch", "error": "historical media plan changed; check the approval again"})
	case errors.Is(err, yt.ErrHistoryApproval):
		http.Error(c.Response, "invalid historical media approval", http.StatusBadRequest)
	case errors.Is(err, yt.ErrHistoryEvidence):
		c.JSONStatus(http.StatusConflict, map[string]string{"code": "evidence_changed", "error": "historical media evidence changed or is unavailable; review a fresh inventory"})
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		http.Error(c.Response, "historical media request cancelled", http.StatusRequestTimeout)
	default:
		log.Error().Err(err).Msg("historical media command failed")
		c.JSONStatus(http.StatusServiceUnavailable, map[string]string{"code": "history_unavailable", "error": "historical media command is unavailable"})
	}
}

func serveMediaHistoryCheck(c *webContext) {
	actor, ok := historyOperator(c)
	if !ok {
		return
	}
	if c.Indexer == nil {
		http.Error(c.Response, "historical media command unavailable", http.StatusServiceUnavailable)
		return
	}
	approval, ok := decodeMediaHistoryApproval(c)
	if !ok {
		return
	}
	state, root, err := historyRoots(c.Config)
	if err != nil {
		mediaHistoryCommandError(c, err)
		return
	}
	check, err := c.Indexer.CheckHistoricalMedia(c.Request.Context(), state, root, approval, actor)
	if err != nil {
		mediaHistoryCommandError(c, err)
		return
	}
	c.JSON(mediaHistoryCheckResponse{ApprovalID: check.ApprovalID, ConfirmationSHA256: check.ConfirmationSHA256, Replayed: check.Replayed})
}

func serveMediaHistoryApply(c *webContext) {
	actor, ok := historyOperator(c)
	if !ok {
		return
	}
	if c.Indexer == nil {
		http.Error(c.Response, "historical media command unavailable", http.StatusServiceUnavailable)
		return
	}
	approval, ok := decodeMediaHistoryApproval(c)
	if !ok {
		return
	}
	state, root, err := historyRoots(c.Config)
	if err != nil {
		mediaHistoryCommandError(c, err)
		return
	}
	result, err := c.Indexer.ApplyHistoricalMedia(c.Request.Context(), state, root, approval, c.Request.Header.Get("X-Hister-Media-Confirmation"), actor)
	if err != nil {
		mediaHistoryCommandError(c, err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	c.JSONStatus(status, mediaHistoryApplyResponse{ApprovalID: result.Receipt.ID, ReceiptID: result.Receipt.ID, SetID: result.Receipt.SetID, Replayed: result.Replayed})
}
