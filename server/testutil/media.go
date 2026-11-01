// SPDX-License-Identifier: AGPL-3.0-or-later

package testutil

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/asciimoo/hister/server/model"
)

// SeedMediaPair creates explicitly trusted test fixtures, not an import path.
// Tests must not use client-supplied metadata as evidence of ownership.
func SeedMediaPair(t testing.TB, documentID string, userID uint, videoPath, audioPath string) []model.MediaArtifact {
	t.Helper()
	source, err := model.EnsureMediaSource(documentID, userID)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := model.AcceptMediaAttempt(source.ID, "trusted-fixture-"+rand.Text(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = model.ClaimMediaAttempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pair []model.MediaArtifactInput
	for _, mode := range []struct{ role, path, contentType string }{
		{model.MediaVideo, videoPath, "video/webm"}, {model.MediaAudio, audioPath, "audio/mpeg"},
	} {
		path, err := filepath.EvalSymlinks(mode.path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := model.StartMediaMode(attempt.ID, attempt.ClaimToken, mode.role); err != nil {
			t.Fatal(err)
		}
		if err := model.CompleteMediaMode(attempt.ID, attempt.ClaimToken, mode.role); err != nil {
			t.Fatal(err)
		}
		pair = append(pair, model.MediaArtifactInput{
			Role: mode.role, JobID: attempt.ID + "-" + mode.role,
			StorageRoot: filepath.Dir(path), RelativePath: filepath.Base(path), Size: info.Size(), ContentType: mode.contentType,
		})
	}
	if _, err := model.PublishMediaPair(attempt.ID, attempt.ClaimToken, pair); err != nil {
		t.Fatal(err)
	}
	artifacts, err := model.CurrentMediaArtifacts(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	return artifacts
}
