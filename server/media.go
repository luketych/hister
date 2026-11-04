// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/asciimoo/hister/files"
	"github.com/asciimoo/hister/server/document"
	yt "github.com/asciimoo/hister/server/extractor/extractors/yt_downloader"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"

	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
)

func authorizedMediaDocument(c *webContext, id string) *document.Document {
	if !model.DatabaseMatches(c.Config) {
		return nil
	}
	doc := c.Indexer.GetByDocID(id)
	if doc == nil || doc.ID() != id || (doc.UserID != 0 && doc.UserID != c.UserID) || !(&yt.YTDownloader{}).Match(doc) {
		return nil
	}
	return doc
}

func serveMediaStatus(c *webContext) {
	id := c.Request.URL.Query().Get("document_id")
	if id == "" {
		http.Error(c.Response, "missing document_id", http.StatusBadRequest)
		return
	}
	doc := authorizedMediaDocument(c, id)
	if doc == nil {
		http.NotFound(c.Response, c.Request)
		return
	}
	status, err := media.ReadStatus(id, doc.UserID)
	if err != nil {
		http.Error(c.Response, "media status is unavailable", http.StatusServiceUnavailable)
		return
	}
	c.Response.Header().Set("Cache-Control", "no-store")
	c.JSON(status)
}

// Only call after source authorization. artifactID must be empty for registry
// failures or come from a verified record, never from an untrusted query. Log
// bounded codes and opaque IDs, not raw filesystem/SQL errors or legacy URIs.
func serveUnavailableMedia(c *webContext, artifactID string, status int, code, message string) {
	log.Warn().Str("code", code).Str("artifact_id", artifactID).Msg("media artifact unavailable")
	if c.Request.Method == http.MethodHead {
		c.Response.Header().Set("Content-Type", "application/json")
		c.Response.WriteHeader(status)
		return
	}
	c.JSONStatus(status, map[string]string{"code": code, "error": message})
}

func serveRegisteredMedia(c *webContext) {
	// Include failures: in particular, authorized retirement diagnostics must
	// not be cached and reused after the caller's source access changes.
	c.Response.Header().Set("Cache-Control", "no-store")
	c.Response.Header().Set("X-Content-Type-Options", "nosniff")
	query := c.Request.URL.Query()
	id, documentID := query.Get("id"), query.Get("document_id")
	if id != "" {
		source, err := model.MediaArtifactSource(id)
		if err != nil || (documentID != "" && documentID != source.DocumentID) {
			http.NotFound(c.Response, c.Request)
			return
		}
		documentID = source.DocumentID
	}
	if documentID == "" || (id == "" && query.Get("uri") == "") {
		http.Error(c.Response, "missing artifact ID or document_id/uri", http.StatusBadRequest)
		return
	}
	doc := authorizedMediaDocument(c, documentID)
	if doc == nil {
		http.NotFound(c.Response, c.Request)
		return
	}
	if id == "" {
		// Old links may name retained historical outputs, but the URI itself
		// never grants access. Resolve only within trusted owner/source records.
		uri := query.Get("uri")
		parsed, err := url.Parse(uri)
		if err != nil || parsed.Scheme != "file" || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			http.NotFound(c.Response, c.Request)
			return
		}
		path := filepath.Clean(files.FileURLToPath(uri))
		artifacts, err := model.MediaArtifactsForDocument(documentID, doc.UserID)
		if err != nil {
			serveUnavailableMedia(c, "", http.StatusServiceUnavailable, "media_registry_unavailable", "Media records are temporarily unavailable.")
			return
		}
		for _, artifact := range artifacts {
			if filepath.Join(artifact.StorageRoot, artifact.RelativePath) == path {
				id = artifact.ID
				break
			}
		}
	}
	artifact, err := model.LookupMediaArtifactRecord(id, doc.UserID, documentID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.NotFound(c.Response, c.Request)
		} else {
			serveUnavailableMedia(c, "", http.StatusServiceUnavailable, "media_registry_unavailable", "Media records are temporarily unavailable.")
		}
		return
	}
	if artifact.RetiredAt != nil {
		serveUnavailableMedia(c, artifact.ID, http.StatusGone, "media_retired", "This media artifact has been retired. This request does not schedule a download.")
		return
	}
	file, info, err := media.OpenArtifact(artifact.MediaArtifactInput)
	if err != nil {
		code, message := "media_unavailable", "This media file is unavailable or failed validation. This request does not schedule a download."
		if errors.Is(err, os.ErrNotExist) {
			code, message = "media_missing", "This media file is missing. This request does not schedule a download."
		}
		serveUnavailableMedia(c, artifact.ID, http.StatusNotFound, code, message)
		return
	}
	defer func() { _ = file.Close() }()
	name := filepath.Base(artifact.RelativePath)
	c.Response.Header().Set("Cache-Control", "no-store")
	c.Response.Header().Set("Content-Type", artifact.ContentType)
	c.Response.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	c.Response.Header().Set("X-Content-Type-Options", "nosniff")
	c.Response.Header().Set("Content-Security-Policy", "default-src 'none'; media-src 'self'")
	http.ServeContent(c.Response, c.Request, name, info.ModTime(), file)
}
