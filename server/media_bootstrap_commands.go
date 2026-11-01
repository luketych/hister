// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/asciimoo/hister/server/indexer"
	"github.com/asciimoo/hister/server/model"

	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
)

const mediaSourceBootstrapConfirmationHeader = "X-Hister-Media-Source-Confirmation"

type mediaSourceBootstrapRequest struct {
	DocumentID string `json:"document_id"`
	OwnerID    uint   `json:"owner_id"`
	URL        string `json:"url"`
}

type mediaSourceBootstrapCheckResponse struct {
	DocumentID          string `json:"document_id"`
	OwnerID             uint   `json:"owner_id"`
	URL                 string `json:"url"`
	ExistingSourceID    string `json:"existing_source_id,omitempty"`
	RevokedIncarnations int    `json:"revoked_incarnations"`
	ConfirmationSHA256  string `json:"confirmation_sha256"`
	Replayed            bool   `json:"replayed"`
}

type mediaSourceBootstrapApplyResponse struct {
	DocumentID string `json:"document_id"`
	OwnerID    uint   `json:"owner_id"`
	SourceID   string `json:"source_id"`
	Replayed   bool   `json:"replayed"`
}

func consumeMediaBootstrapJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("invalid object key")
			}
			if _, duplicate := seen[name]; duplicate {
				return errors.New("duplicate object key")
			}
			seen[name] = struct{}{}
			if err := consumeMediaBootstrapJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeMediaBootstrapJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func decodeMediaSourceBootstrapRequest(c *webContext) (*mediaSourceBootstrapRequest, bool) {
	if c.Request.Method != http.MethodPost {
		c.Response.Header().Set("Allow", http.MethodPost)
		http.Error(c.Response, "POST required", http.StatusMethodNotAllowed)
		return nil, false
	}
	data, err := io.ReadAll(http.MaxBytesReader(c.Response, c.Request.Body, maxMediaCommandBytes))
	if err != nil || len(data) == 0 || int64(len(data)) > maxMediaCommandBytes {
		http.Error(c.Response, "invalid media source bootstrap request", http.StatusBadRequest)
		return nil, false
	}
	keyDecoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeMediaBootstrapJSONValue(keyDecoder); err != nil {
		http.Error(c.Response, "invalid media source bootstrap request", http.StatusBadRequest)
		return nil, false
	}
	if _, err := keyDecoder.Token(); err != io.EOF {
		http.Error(c.Response, "expected one media source bootstrap object", http.StatusBadRequest)
		return nil, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 3 {
		http.Error(c.Response, "invalid media source bootstrap request", http.StatusBadRequest)
		return nil, false
	}
	for _, name := range []string{"document_id", "owner_id", "url"} {
		if _, exists := fields[name]; !exists {
			http.Error(c.Response, "invalid media source bootstrap request", http.StatusBadRequest)
			return nil, false
		}
	}
	if string(bytes.TrimSpace(fields["owner_id"])) == "null" {
		http.Error(c.Response, "invalid media source bootstrap request", http.StatusBadRequest)
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request mediaSourceBootstrapRequest
	if decoder.Decode(&request) != nil || decoder.Decode(new(json.RawMessage)) != io.EOF ||
		strings.TrimSpace(request.DocumentID) == "" || strings.TrimSpace(request.URL) == "" ||
		len(request.DocumentID) > 4096 || len(request.URL) > 4096 {
		http.Error(c.Response, "invalid media source bootstrap request", http.StatusBadRequest)
		return nil, false
	}
	c.Response.Header().Set("Cache-Control", "no-store")
	return &request, true
}

func mediaSourceBootstrapCommandError(c *webContext, err error) {
	switch {
	case errors.Is(err, indexer.ErrMediaSourceBootstrapForbidden):
		serve403(c)
	case errors.Is(err, gorm.ErrRecordNotFound):
		http.NotFound(c.Response, c.Request)
	case errors.Is(err, model.ErrMediaConflict):
		c.JSONStatus(http.StatusConflict, map[string]string{"code": "source_conflict", "error": "document has a conflicting or unavailable media source binding"})
	case errors.Is(err, indexer.ErrMediaSourceBootstrapConfirmation):
		c.JSONStatus(http.StatusConflict, map[string]string{"code": "confirmation_mismatch", "error": "indexed document or source registry changed; check the bootstrap plan again"})
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		http.Error(c.Response, "media source bootstrap cancelled", http.StatusRequestTimeout)
	default:
		log.Error().Err(err).Msg("media source bootstrap failed")
		c.JSONStatus(http.StatusServiceUnavailable, map[string]string{"code": "source_bootstrap_unavailable", "error": "media source bootstrap is unavailable"})
	}
}

func mediaSourceBootstrapActor(c *webContext) (indexer.MediaActor, bool) {
	return historyOperator(c)
}

func serveMediaSourceBootstrapCheck(c *webContext) {
	actor, ok := mediaSourceBootstrapActor(c)
	if !ok {
		return
	}
	if c.Indexer == nil {
		http.Error(c.Response, "media source bootstrap unavailable", http.StatusServiceUnavailable)
		return
	}
	request, ok := decodeMediaSourceBootstrapRequest(c)
	if !ok {
		return
	}
	check, err := c.Indexer.CheckMediaSourceBootstrap(c.Request.Context(), request.DocumentID, request.URL, request.OwnerID, actor)
	if err != nil {
		mediaSourceBootstrapCommandError(c, err)
		return
	}
	c.JSON(mediaSourceBootstrapCheckResponse{
		DocumentID: check.DocumentID, OwnerID: check.UserID, URL: check.URL,
		ExistingSourceID: check.ExistingSourceID, RevokedIncarnations: check.RevokedIncarnations,
		ConfirmationSHA256: check.ConfirmationSHA256, Replayed: check.Replayed,
	})
}

func serveMediaSourceBootstrapApply(c *webContext) {
	actor, ok := mediaSourceBootstrapActor(c)
	if !ok {
		return
	}
	if c.Indexer == nil {
		http.Error(c.Response, "media source bootstrap unavailable", http.StatusServiceUnavailable)
		return
	}
	request, ok := decodeMediaSourceBootstrapRequest(c)
	if !ok {
		return
	}
	result, err := c.Indexer.ApplyMediaSourceBootstrap(c.Request.Context(), request.DocumentID, request.URL, request.OwnerID, c.Request.Header.Get(mediaSourceBootstrapConfirmationHeader), actor)
	if err != nil {
		mediaSourceBootstrapCommandError(c, err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	c.JSONStatus(status, mediaSourceBootstrapApplyResponse{
		DocumentID: result.Source.DocumentID, OwnerID: result.Source.UserID,
		SourceID: result.Source.ID, Replayed: result.Replayed,
	})
}
