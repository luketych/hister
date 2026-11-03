// SPDX-License-Identifier: AGPL-3.0-or-later

package media_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"
)

func TestOpenArtifactDistinguishesMissingFromInvalidFiles(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, root, "valid.webm", []byte("video"))
	if err := os.Symlink("valid.webm", filepath.Join(root, "symlink.webm")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(root, "symlink-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory.webm"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path string
		size int64
		want error
	}{
		{"missing.webm", 5, os.ErrNotExist},
		{"missing-dir/video.webm", 5, os.ErrNotExist},
		{"symlink.webm", 5, media.ErrInvalidOutput},
		{"symlink-dir/valid.webm", 5, media.ErrInvalidOutput},
		{"directory.webm", 5, media.ErrInvalidOutput},
		{"../valid.webm", 5, media.ErrInvalidOutput},
		{"valid.webm", 6, media.ErrInvalidOutput},
	} {
		t.Run(test.path, func(t *testing.T) {
			file, _, err := media.OpenArtifact(model.MediaArtifactInput{StorageRoot: root, RelativePath: test.path, Size: test.size})
			if file != nil {
				_ = file.Close()
				t.Fatal("unavailable file was opened for playback")
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("open error = %v, want %v", err, test.want)
			}
		})
	}
	file, _, err := media.OpenArtifact(model.MediaArtifactInput{StorageRoot: root, RelativePath: "valid.webm", Size: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveArtifactChecksTheValidatedFileIdentity(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "retained"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := testutil.WriteFile(t, root, filepath.Join("retained", "video.webm"), []byte("video"))
	artifact := model.MediaArtifactInput{StorageRoot: root, RelativePath: filepath.Join("retained", "video.webm"), Size: 5}
	file, expected, err := media.OpenArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := media.HashArtifact(context.Background(), file, artifact.Size)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := media.RemoveArtifact(artifact, expected, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed artifact stat = %v, want not-exist", err)
	}

	file, _, err = media.OpenArtifact(model.MediaArtifactInput{StorageRoot: root, RelativePath: filepath.Join("retained", "next.webm"), Size: 5})
	if file != nil || !errors.Is(err, os.ErrNotExist) {
		if file != nil {
			_ = file.Close()
		}
		t.Fatalf("missing artifact open = %v", err)
	}
	nextPath := testutil.WriteFile(t, root, filepath.Join("retained", "next.webm"), []byte("video"))
	next := model.MediaArtifactInput{StorageRoot: root, RelativePath: filepath.Join("retained", "next.webm"), Size: 5}
	file, expected, err = media.OpenArtifact(next)
	if err != nil {
		t.Fatal(err)
	}
	digest, err = media.HashArtifact(context.Background(), file, next.Size)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(nextPath, nextPath+".old"); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, root, filepath.Join("retained", "next.webm"), []byte("other"))
	if err := media.RemoveArtifact(next, expected, digest); !errors.Is(err, media.ErrInvalidOutput) {
		t.Fatalf("replacement removal error = %v, want invalid output", err)
	}
	if data, err := os.ReadFile(nextPath); err != nil || string(data) != "other" {
		t.Fatalf("replacement was removed or changed: %q, %v", data, err)
	}
}
