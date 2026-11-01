// SPDX-License-Identifier: AGPL-3.0-or-later

package testutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// WriteHistoryReports creates synthetic unsigned producer evidence, not registry
// authorization. Callers provide disposable test roots and explicit identities.
func WriteHistoryReports(t testing.TB, state, root, jobID, sourceURL, videoID, role string) string {
	t.Helper()
	jobType, extension := "scrape_videos", ".webm"
	if role == "audio" {
		jobType, extension = "scrape_audio_only", ".mp3"
	}
	output := filepath.Join(root, jobID)
	report := filepath.Join(state, "artifacts", jobID, "jobs", jobID)
	if err := os.MkdirAll(filepath.Join(output, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := WriteFile(t, output, "nested/clip"+extension, []byte("media"))
	writeJSON := func(path string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		WriteFile(t, filepath.Dir(path), filepath.Base(path), data)
	}
	provenance := filepath.Join(filepath.Dir(path), "provenance-"+videoID+"-"+jobType+".json")
	writeJSON(provenance, map[string]any{
		"schema_version": 1, "source": map[string]any{"url": sourceURL, "video_id": videoID},
		"retrieval": map[string]any{"job_type": jobType},
		"outputs":   []any{map[string]any{"kind": "media", "path": filepath.Base(path)}},
	})
	info, err := os.Stat(provenance)
	if err != nil {
		t.Fatal(err)
	}
	mediaFiles := []any{map[string]any{"path": path, "bytes": 5}}
	provenanceFiles := []any{map[string]any{"path": provenance, "bytes": info.Size()}}
	writeJSON(filepath.Join(report, "request.json"), map[string]any{
		"job_id": jobID, "output_dir": output, "artifacts_dir": report, "requested_urls": []string{sourceURL},
	})
	writeJSON(filepath.Join(report, "result.json"), map[string]any{
		"job_id": jobID, "output_dir": output, "requested_urls": []string{sourceURL}, "status": "success",
		"provenance_files": provenanceFiles,
		"items": []any{map[string]any{
			"url": sourceURL, "status": "success", "job_type": jobType, "video_id": videoID,
			"media_paths": []string{path}, "saved_files": mediaFiles, "provenance_files": provenanceFiles,
		}},
	})
	writeJSON(filepath.Join(report, "manifest.json"), map[string]any{
		"job_id": jobID, "output_dir": output, "delivered_files": mediaFiles, "provenance_files": provenanceFiles,
	})
	return path
}
