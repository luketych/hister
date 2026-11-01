// SPDX-License-Identifier: AGPL-3.0-or-later

package yt_downloader

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/server/document"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"
)

// MediaWorkers keeps background dispatch bounded. The legacy zero/unlimited
// process-slot setting uses the default two workers for durable work.
func (e *YTDownloader) MediaWorkers() int {
	if count := cap(e.jobSlots); count > 0 {
		return count
	}
	return 2
}

// MediaRequest freezes current server settings for an explicitly authorized
// retry. The HTTP caller cannot supply executable paths, options or ownership.
func (e *YTDownloader) MediaRequest(d *document.Document) (media.Request, error) {
	if d == nil || !e.GetConfig().Enable {
		return media.Request{}, fmt.Errorf("downloader is disabled or source is missing")
	}
	if _, err := youtubeVideoID(d.URL); err != nil {
		return media.Request{}, err
	}
	options, err := e.downloaderOptions()
	if err != nil {
		return media.Request{}, err
	}
	return media.Request{
		URL: d.URL, UserID: d.UserID, Binary: options.binary,
		OutputRoot: options.outputRoot, Backend: options.backend, Transcribe: options.transcribe, Timeout: options.timeout,
	}, nil
}

// RunMediaAttempt reuses the strict v1 manifest and unique-job protocol. It
// neither writes document metadata nor authorizes/publishes an artifact itself.
func RunMediaAttempt(parent context.Context, request media.Request, events media.ModeEvents) ([]model.MediaArtifactInput, error) {
	e := &YTDownloader{}
	if err := e.SetConfig(&config.Extractor{Enable: true, Options: map[string]any{
		"binary": request.Binary, "output_dir": request.OutputRoot, "backend": request.Backend,
		"transcribe": request.Transcribe, "timeout": request.Timeout,
	}}); err != nil {
		return nil, fmt.Errorf("%w: frozen downloader configuration: %v", media.ErrInvalidOutput, err)
	}
	videoID, err := youtubeVideoID(request.URL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", media.ErrInvalidOutput, err)
	}
	options, err := e.downloaderOptions()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(request.Timeout)*time.Second)
	defer cancel()
	source := downloadSource{documentID: document.GetDocID(request.UserID, request.URL), userID: request.UserID, url: request.URL, videoID: videoID}
	pair := make([]model.MediaArtifactInput, 0, 2)
	for _, mode := range []struct{ role, command, jobType string }{
		{model.MediaVideo, "download", "scrape_videos"},
		{model.MediaAudio, "audio", "scrape_audio_only"},
	} {
		manifest, err := runDownloaderInvocationWithEvents(ctx, source, options, mode.command, mode.jobType,
			func(jobID string) error { return events.Prepared(mode.role, jobID) },
			func() error { return events.Started(mode.role) },
			func() error { return events.Completed(mode.role) })
		if err != nil {
			return nil, err
		}
		if len(manifest.MediaPaths) != 1 {
			return nil, fmt.Errorf("%w: expected one playback artifact for %s", media.ErrInvalidOutput, mode.role)
		}
		path := manifest.MediaPaths[0]
		root := filepath.Dir(manifest.OutputDir)
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", media.ErrInvalidOutput, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", media.ErrInvalidOutput, err)
		}
		contentType := playbackContentType(mode.role, path)
		if contentType == "" {
			return nil, fmt.Errorf("%w: unsupported playback artifact", media.ErrInvalidOutput)
		}
		candidate := model.MediaArtifactInput{
			Role: mode.role, JobID: manifest.JobID, StorageRoot: root,
			RelativePath: relative, Size: info.Size(), ContentType: contentType,
		}
		file, _, err := media.OpenArtifact(candidate)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", media.ErrInvalidOutput, err)
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		pair = append(pair, candidate)
	}
	return pair, nil
}

func playbackContentType(role, path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch role {
	case model.MediaVideo:
		switch ext {
		case ".mp4":
			return "video/mp4"
		case ".webm":
			return "video/webm"
		case ".mkv":
			return "video/x-matroska"
		case ".mov":
			return "video/quicktime"
		case ".ogv":
			return "video/ogg"
		}
	case model.MediaAudio:
		switch ext {
		case ".mp3":
			return "audio/mpeg"
		case ".m4a", ".mp4":
			return "audio/mp4"
		case ".webm":
			return "audio/webm"
		case ".mkv":
			return "audio/x-matroska"
		case ".ogg":
			return "audio/ogg"
		case ".opus":
			return "audio/opus"
		case ".wav":
			return "audio/wav"
		case ".flac":
			return "audio/flac"
		case ".aac":
			return "audio/aac"
		}
	}
	return ""
}
