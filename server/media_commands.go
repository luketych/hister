// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/server/indexer"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"

	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
)

const maxMediaCommandBytes int64 = 64 << 10

func mediaCommandBodyLimit(*config.Config) int64 { return maxMediaCommandBytes }

type mediaCommandRequest struct {
	DocumentID string `json:"document_id"`
	AttemptID  string `json:"attempt_id"`
}

type mediaCommandResponse struct {
	AttemptID string `json:"attempt_id"`
	State     string `json:"state"`
	Replayed  bool   `json:"replayed"`
}

type mediaCancelResponse struct {
	AttemptID string `json:"attempt_id"`
	State     string `json:"state"`
}

func decodeMediaCommand(c *webContext) (*mediaCommandRequest, bool) {
	if c.Request.Method != http.MethodPost {
		c.Response.Header().Set("Allow", http.MethodPost)
		http.Error(c.Response, "POST required", http.StatusMethodNotAllowed)
		return nil, false
	}
	if !canWrite(c) {
		serve403(c)
		return nil, false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response, c.Request.Body, maxMediaCommandBytes))
	decoder.DisallowUnknownFields()
	var request mediaCommandRequest
	if err := decoder.Decode(&request); err != nil {
		if !requestBodyTooLarge(c, err) {
			http.Error(c.Response, "invalid media command request", http.StatusBadRequest)
		}
		return nil, false
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		if !requestBodyTooLarge(c, err) {
			http.Error(c.Response, "expected one media command object", http.StatusBadRequest)
		}
		return nil, false
	}
	if strings.TrimSpace(request.DocumentID) == "" || strings.TrimSpace(request.AttemptID) == "" || len(request.AttemptID) > 128 {
		http.Error(c.Response, "document_id and attempt_id are required", http.StatusBadRequest)
		return nil, false
	}
	if !model.DatabaseMatches(c.Config) {
		http.NotFound(c.Response, c.Request)
		return nil, false
	}
	c.Response.Header().Set("Cache-Control", "no-store")
	return &request, true
}

func mediaCommandError(c *webContext, err error) {
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound), errors.Is(err, model.ErrMediaSourceRevoked):
		http.NotFound(c.Response, c.Request)
	case errors.Is(err, indexer.ErrMediaSourceSkipped):
		c.JSONStatus(http.StatusNotAcceptable, map[string]string{"code": "source_skipped", "error": "media source is excluded by current URL rules"})
	case errors.Is(err, indexer.ErrMediaDisabled):
		c.JSONStatus(http.StatusConflict, map[string]string{"code": "downloader_disabled", "error": "media downloader is disabled"})
	case errors.Is(err, model.ErrMediaConflict):
		c.JSONStatus(http.StatusConflict, map[string]string{"code": "attempt_conflict", "error": "attempt is no longer eligible or source is unavailable; check status"})
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		http.Error(c.Response, "media command request cancelled", http.StatusRequestTimeout)
	default:
		if !errors.Is(err, media.ErrDispatcherUnavailable) {
			log.Error().Err(err).Msg("media command failed")
		}
		c.JSONStatus(http.StatusServiceUnavailable, map[string]string{"code": "media_unavailable", "error": "media command is unavailable"})
	}
}

func serveMediaStart(c *webContext) {
	request, ok := decodeMediaStart(c)
	if !ok {
		return
	}
	result, err := c.Indexer.StartMedia(c.Request.Context(), request.DocumentID, indexer.MediaActor{UserID: c.UserID, IsAdmin: c.IsAdmin}, indexer.WithRules(c.effectiveRules()))
	if err != nil {
		mediaCommandError(c, err)
		return
	}
	status := http.StatusAccepted
	if result.Replayed {
		status = http.StatusOK
	}
	c.JSONStatus(status, mediaCommandResponse{AttemptID: result.Attempt.ID, State: result.Attempt.State, Replayed: result.Replayed})
}

type mediaStartRequest struct {
	DocumentID string `json:"document_id"`
}

func decodeMediaStart(c *webContext) (*mediaStartRequest, bool) {
	if c.Request.Method != http.MethodPost {
		c.Response.Header().Set("Allow", http.MethodPost)
		http.Error(c.Response, "POST required", http.StatusMethodNotAllowed)
		return nil, false
	}
	if !canWrite(c) {
		serve403(c)
		return nil, false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response, c.Request.Body, maxMediaCommandBytes))
	decoder.DisallowUnknownFields()
	var request mediaStartRequest
	if err := decoder.Decode(&request); err != nil {
		if !requestBodyTooLarge(c, err) {
			http.Error(c.Response, "invalid media command request", http.StatusBadRequest)
		}
		return nil, false
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		if !requestBodyTooLarge(c, err) {
			http.Error(c.Response, "expected one media command object", http.StatusBadRequest)
		}
		return nil, false
	}
	if strings.TrimSpace(request.DocumentID) == "" {
		http.Error(c.Response, "document_id is required", http.StatusBadRequest)
		return nil, false
	}
	if !model.DatabaseMatches(c.Config) {
		http.NotFound(c.Response, c.Request)
		return nil, false
	}
	c.Response.Header().Set("Cache-Control", "no-store")
	return &request, true
}

func serveMediaRetry(c *webContext) {
	request, ok := decodeMediaCommand(c)
	if !ok {
		return
	}
	result, err := c.Indexer.RetryMedia(c.Request.Context(), request.DocumentID, request.AttemptID, indexer.MediaActor{UserID: c.UserID, IsAdmin: c.IsAdmin}, indexer.WithRules(c.effectiveRules()))
	if err != nil {
		mediaCommandError(c, err)
		return
	}
	status := http.StatusAccepted
	if result.Replayed {
		status = http.StatusOK
	}
	c.JSONStatus(status, mediaCommandResponse{AttemptID: result.Attempt.ID, State: result.Attempt.State, Replayed: result.Replayed})
}

func serveMediaCancel(c *webContext) {
	request, ok := decodeMediaCommand(c)
	if !ok {
		return
	}
	attempt, err := c.Indexer.CancelMedia(c.Request.Context(), request.DocumentID, request.AttemptID, indexer.MediaActor{UserID: c.UserID, IsAdmin: c.IsAdmin})
	if err != nil {
		mediaCommandError(c, err)
		return
	}
	c.JSON(mediaCancelResponse{AttemptID: attempt.ID, State: attempt.State})
}
