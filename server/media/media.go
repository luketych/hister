// SPDX-License-Identifier: AGPL-3.0-or-later

// Package media coordinates server-owned download work. No document metadata
// or serialized/imported document can create a scheduling intent or binding.
package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
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
	root, leaf, err := openArtifactParent(artifact)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = root.Close() }()
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

// RemoveArtifact removes only the exact regular file checked by OpenArtifact.
// expected is the descriptor info captured during the current retention apply
// preflight; a replaced or modified file is left in place for manual review.
func RemoveArtifact(artifact model.MediaArtifactInput, expected os.FileInfo, expectedSHA256 string) error {
	file, opened, err := OpenArtifact(artifact)
	if err != nil {
		return err
	}
	if expected != nil && (!os.SameFile(expected, opened) || expected.Size() != opened.Size() || expected.Mode() != opened.Mode() || !expected.ModTime().Equal(opened.ModTime())) {
		_ = file.Close()
		return ErrInvalidOutput
	}
	digest, err := HashArtifact(context.Background(), file, artifact.Size)
	if err != nil || digest != expectedSHA256 {
		_ = file.Close()
		return ErrInvalidOutput
	}
	if err := file.Close(); err != nil {
		return err
	}
	root, leaf, err := openArtifactParent(artifact)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	before, err := root.Lstat(leaf)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || !os.SameFile(opened, before) || before.Size() != artifact.Size || opened.Mode() != before.Mode() || !opened.ModTime().Equal(before.ModTime()) {
		return ErrInvalidOutput
	}
	return root.Remove(leaf)
}

// HashArtifact hashes a safely opened artifact descriptor and rejects changes
// observed during the read. It is useful for confirmation fingerprints on
// destructive operations; caller still owns and must close the descriptor.
func HashArtifact(ctx context.Context, file *os.File, expectedSize int64) (string, error) {
	if file == nil || expectedSize <= 0 {
		return "", ErrInvalidOutput
	}
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() != expectedSize {
		return "", ErrInvalidOutput
	}
	if _, err := file.Seek(0, 0); err != nil {
		return "", err
	}
	hash := sha256.New()
	buffer := make([]byte, 128*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			total += int64(n)
			if total > expectedSize {
				return "", ErrInvalidOutput
			}
			_, _ = hash.Write(buffer[:n])
		}
		if errors.Is(readErr, os.ErrClosed) {
			return "", readErr
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return "", readErr
		}
	}
	after, err := file.Stat()
	if err != nil || total != expectedSize || !os.SameFile(before, after) || after.Size() != before.Size() || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		return "", ErrInvalidOutput
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func openArtifactParent(artifact model.MediaArtifactInput) (*os.Root, string, error) {
	if !filepath.IsAbs(artifact.StorageRoot) || filepath.Clean(artifact.StorageRoot) != artifact.StorageRoot || !filepath.IsLocal(artifact.RelativePath) || artifact.RelativePath == "." || filepath.Clean(artifact.RelativePath) != artifact.RelativePath || artifact.Size <= 0 {
		return nil, "", ErrInvalidOutput
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
		return nil, "", err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = root.Close()
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		before, err := root.Lstat(part)
		if err != nil {
			return nil, "", err
		}
		if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			return nil, "", ErrInvalidOutput
		}
		next, err := root.OpenRoot(part)
		if err != nil {
			return nil, "", err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(before, opened) {
			_ = next.Close()
			return nil, "", ErrInvalidOutput
		}
		_ = root.Close()
		root = next
	}
	closeOnError = false
	return root, parts[len(parts)-1], nil
}
