// SPDX-License-Identifier: AGPL-3.0-or-later

// Package media coordinates server-owned download work. No document metadata
// or serialized/imported document can create a scheduling intent or binding.
package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/asciimoo/hister/server/model"
)

var ErrInvalidOutput = errors.New("invalid downloaded media")

// Request is frozen from server configuration, not a client payload.
// UserID and URL are checked against the authoritative indexed identity.
type Request struct {
	URL        string `json:"url"`
	UserID     uint   `json:"user_id"`
	Binary     string `json:"binary"`
	OutputRoot string `json:"output_root"`
	Backend    string `json:"backend"`
	Transcribe bool   `json:"transcribe"`
	Timeout    int    `json:"timeout"`
}

type Intent struct {
	AttemptID  string
	SourceID   string
	DocumentID string
	UserID     uint
}

type stagerKey struct{}

type Stager func(Request) error

func WithStager(ctx context.Context, stage Stager) context.Context {
	return context.WithValue(ctx, stagerKey{}, stage)
}

func HasStager(ctx context.Context) bool {
	_, ok := ctx.Value(stagerKey{}).(Stager)
	return ok
}

func Stage(ctx context.Context, request Request) (bool, error) {
	stage, ok := ctx.Value(stagerKey{}).(Stager)
	if !ok {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	return true, stage(request)
}

// StripMetadata applies at every index-write boundary, including processed
// submissions, imports and Save. Metadata is never authorization or intent.
func StripMetadata(metadata map[string]any) {
	delete(metadata, "yt_video_downloader_media")
	delete(metadata, "yt_downloader_media")
}

// OpenArtifact authorizes no caller. After checking the registry/source access,
// callers use this anchored open to prevent relative-path/symlink escapes and
// validate the actual opened regular file, not a checked-then-reopened pathname.
func OpenArtifact(artifact model.MediaArtifactInput) (*os.File, os.FileInfo, error) {
	if !filepath.IsAbs(artifact.StorageRoot) || filepath.Clean(artifact.StorageRoot) != artifact.StorageRoot || !filepath.IsLocal(artifact.RelativePath) || artifact.RelativePath == "." || filepath.Clean(artifact.RelativePath) != artifact.RelativePath || artifact.Size <= 0 {
		return nil, nil, ErrInvalidOutput
	}
	// Pin each directory descriptor separately. Root.Open alone permits
	// symlinks to other jobs inside the shared root, which could cross owners.
	// Lstat + descriptor identity checks reject both those links and swaps
	// between the check and open, including in the storage-root ancestors.
	path := filepath.Join(artifact.StorageRoot, artifact.RelativePath)
	base := filepath.VolumeName(path) + string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(path, base), string(filepath.Separator))
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = root.Close() }()
	for _, part := range parts[:len(parts)-1] {
		before, err := root.Lstat(part)
		if err != nil {
			return nil, nil, err
		}
		if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			return nil, nil, ErrInvalidOutput
		}
		next, err := root.OpenRoot(part)
		if err != nil {
			return nil, nil, err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(before, opened) {
			_ = next.Close()
			return nil, nil, ErrInvalidOutput
		}
		_ = root.Close()
		root = next
	}
	leaf := parts[len(parts)-1]
	before, err := root.Lstat(leaf)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, ErrInvalidOutput
	}
	file, err := root.Open(leaf)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() != artifact.Size {
		_ = file.Close()
		return nil, nil, ErrInvalidOutput
	}
	return file, opened, nil
}
