// SPDX-License-Identifier: AGPL-3.0-or-later

package yt_downloader

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const historyTestURL = "https://www.youtube.com/watch?v=abcdefghijk"

func historyJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func historyFixture(t *testing.T, state, media, jobID string) (string, string) {
	t.Helper()
	jobType, ext := "scrape_videos", ".webm"
	if strings.HasSuffix(jobID, "-audio") {
		jobType, ext = "scrape_audio_only", ".mp3"
	}
	output := filepath.Join(media, jobID)
	report := filepath.Join(state, "artifacts", jobID, "jobs", jobID)
	path := filepath.Join(output, "nested", "clip"+ext)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	provenance := filepath.Join(filepath.Dir(path), "provenance-abcdefghijk-"+jobType+".json")
	historyJSON(t, provenance, map[string]any{
		"schema_version": 1, "source": map[string]any{"url": historyTestURL, "video_id": "abcdefghijk"},
		"retrieval": map[string]any{"job_type": jobType},
		"outputs":   []any{map[string]any{"kind": "media", "path": filepath.Base(path)}},
	})
	info, err := os.Stat(provenance)
	if err != nil {
		t.Fatal(err)
	}
	mediaFiles := []any{map[string]any{"path": path, "name": filepath.Base(path), "bytes": 5}}
	provenanceFiles := []any{map[string]any{"path": provenance, "name": filepath.Base(provenance), "bytes": info.Size()}}
	historyJSON(t, filepath.Join(report, "request.json"), map[string]any{
		"job_id": jobID, "output_dir": output, "artifacts_dir": report, "requested_urls": []string{historyTestURL},
	})
	historyJSON(t, filepath.Join(report, "result.json"), map[string]any{
		"job_id": jobID, "output_dir": output, "requested_urls": []string{historyTestURL}, "status": "success",
		"provenance_files": provenanceFiles,
		"items": []any{map[string]any{
			"url": historyTestURL, "status": "success", "job_type": jobType,
			"video_id": "abcdefghijk", "media_paths": []string{path}, "saved_files": mediaFiles, "provenance_files": provenanceFiles,
		}},
	})
	historyJSON(t, filepath.Join(report, "manifest.json"), map[string]any{
		"job_id": jobID, "output_dir": output, "delivered_files": mediaFiles, "provenance_files": provenanceFiles,
	})
	return path, report
}

func historyRoots(t *testing.T) (string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, media := filepath.Join(root, "state"), filepath.Join(root, "media")
	for _, dir := range []string{state, media} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return state, media
}

func historySnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			out[path] = "directory"
			return nil
		}
		data, err := os.ReadFile(path)
		out[path] = string(data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestHistoryPlanInspectsReportsWithoutAuthorizingOrWriting(t *testing.T) {
	state, media := historyRoots(t)
	video := "hister-" + strings.Repeat("a", 32) + "-video"
	audio := "hister-" + strings.Repeat("b", 32) + "-audio"
	historyFixture(t, state, media, video)
	historyFixture(t, state, media, audio)
	before := historySnapshot(t, filepath.Dir(state))
	first, err := PlanHistory(context.Background(), state, media, []string{video, audio})
	if err != nil {
		t.Fatal(err)
	}
	second, err := PlanHistory(context.Background(), state, media, []string{audio, video})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("report order changed the deterministic plan")
	}
	if !first.DryRun || first.RegistryChecked || first.OwnershipVerified || len(first.Jobs) != 2 || len(first.Sources) != 1 || first.Sources[0].State != "pair_approval_required" {
		t.Fatalf("plan claimed authorization/readiness: %#v", first)
	}
	for _, job := range first.Jobs {
		if job.State != "owner_approval_required" || job.VideoID != "abcdefghijk" || job.SourceURL != historyTestURL || len(job.EvidenceSHA256) != 64 || job.File == nil || job.File.Size != 5 {
			t.Fatalf("incomplete evidence: %#v", job)
		}
	}
	if got := historySnapshot(t, filepath.Dir(state)); !reflect.DeepEqual(got, before) {
		t.Fatal("inspection wrote or removed runtime/media files")
	}
}

func TestHistoryPlanFailsClosedOnIncompleteOrConflictingEvidence(t *testing.T) {
	for _, kind := range []string{"missing_report", "duplicate_key", "job_mismatch", "failed", "source_mismatch", "mode_mismatch", "unknown_video", "foreign_path", "missing_file", "changed_size", "symlink", "symlink_report", "symlink_ancestor", "missing_provenance", "wrong_provenance", "unsupported_provenance", "duplicate_media", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			state, media := historyRoots(t)
			id := "hister-" + strings.Repeat("a", 32) + "-video"
			path, report := historyFixture(t, state, media, id)
			resultPath := filepath.Join(report, "result.json")
			data, err := os.ReadFile(resultPath)
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			item := result["items"].([]any)[0].(map[string]any)
			switch kind {
			case "missing_report":
				if err := os.Remove(resultPath); err != nil {
					t.Fatal(err)
				}
			case "duplicate_key":
				if err := os.WriteFile(resultPath, append([]byte(`{"status":"failed",`), data[1:]...), 0o600); err != nil {
					t.Fatal(err)
				}
			case "job_mismatch":
				result["job_id"] = "foreign"
				historyJSON(t, resultPath, result)
			case "failed":
				result["status"] = "failed"
				historyJSON(t, resultPath, result)
			case "source_mismatch":
				item["url"] = "https://youtu.be/lmnopqrstuv"
				historyJSON(t, resultPath, result)
			case "mode_mismatch":
				item["job_type"] = "scrape_audio_only"
				historyJSON(t, resultPath, result)
			case "unknown_video":
				delete(item, "video_id")
				historyJSON(t, resultPath, result)
			case "foreign_path":
				item["media_paths"] = []string{filepath.Join(media, "foreign.webm")}
				historyJSON(t, resultPath, result)
			case "missing_file":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "changed_size":
				if err := os.WriteFile(path, []byte("different size"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(resultPath, path); err != nil {
					t.Fatal(err)
				}
			case "symlink_report":
				if err := os.Remove(resultPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(report, "request.json"), resultPath); err != nil {
					t.Fatal(err)
				}
			case "symlink_ancestor":
				if err := os.Rename(filepath.Join(state, "artifacts"), filepath.Join(state, "elsewhere")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("elsewhere", filepath.Join(state, "artifacts")); err != nil {
					t.Fatal(err)
				}
			case "missing_provenance", "wrong_provenance", "unsupported_provenance":
				provenance := result["provenance_files"].([]any)[0].(map[string]any)["path"].(string)
				if kind == "missing_provenance" {
					if err := os.Remove(provenance); err != nil {
						t.Fatal(err)
					}
				} else {
					data, err := os.ReadFile(provenance)
					if err != nil {
						t.Fatal(err)
					}
					value := strings.ReplaceAll(string(data), "abcdefghijk", "lmnopqrstuv")
					if kind == "unsupported_provenance" {
						value = strings.Replace(string(data), `"schema_version":1`, `"schema_version":2`, 1)
					}
					if err := os.WriteFile(provenance, []byte(value), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			case "duplicate_media":
				item["media_paths"] = []string{path, path}
				historyJSON(t, resultPath, result)
			case "oversize":
				if err := os.WriteFile(resultPath, []byte(strings.Repeat(" ", maxManifestSize+1)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := PlanHistory(context.Background(), state, media, []string{id})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Jobs) != 1 || plan.Jobs[0].State != "blocked" || plan.Jobs[0].Code == "" || plan.Jobs[0].File != nil || len(plan.Sources) != 0 {
				t.Fatalf("invalid evidence became a candidate: %#v", plan)
			}
		})
	}
}

func TestHistoryPlanDoesNotInferOwnersPairsOrLeakDiagnostics(t *testing.T) {
	state, media := historyRoots(t)
	ids := []string{"hister-" + strings.Repeat("a", 32) + "-video", "hister-" + strings.Repeat("b", 32) + "-video", "hister-" + strings.Repeat("c", 32) + "-audio"}
	for _, id := range ids {
		_, report := historyFixture(t, state, media, id)
		for _, name := range []string{"request.json", "result.json"} {
			path := filepath.Join(report, name)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err := json.Unmarshal(data, &value); err != nil {
				t.Fatal(err)
			}
			value["requested_urls"] = []string{historyTestURL + "&token=secret-token"}
			value["user_id"], value["message"] = 22, "private backend diagnostic"
			if name == "result.json" {
				value["items"].([]any)[0].(map[string]any)["url"] = historyTestURL + "&token=secret-token"
			}
			historyJSON(t, path, value)
		}
	}
	plan, err := PlanHistory(context.Background(), state, media, ids)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != 1 || plan.OwnershipVerified || plan.RegistryChecked || len(plan.Sources) != 1 || plan.Sources[0].State != "ambiguous_pair" {
		t.Fatalf("unapproved jobs acquired an owner/pair: %#v", plan)
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-token", "private backend diagnostic", state, media, `"user_id"`} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("history inventory leaked %q: %s", secret, data)
		}
	}
	for _, job := range plan.Jobs {
		if job.State != "owner_approval_required" || job.SourceURL != historyTestURL {
			t.Fatalf("untrusted ownership/query accepted: %#v", job)
		}
	}
}

func TestHistoryPlanRequiresExplicitSafeInputsAndPairApproval(t *testing.T) {
	state, media := historyRoots(t)
	id := "hister-" + strings.Repeat("a", 32) + "-video"
	historyFixture(t, state, media, id)
	for _, ids := range [][]string{nil, {"../escape"}, {"unknown"}, {id, id}} {
		if _, err := PlanHistory(context.Background(), state, media, ids); err == nil {
			t.Fatalf("accepted unsafe/ambiguous job selection: %v", ids)
		}
	}
	plan, err := PlanHistory(context.Background(), state, media, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sources) != 1 || plan.Sources[0].State != "incomplete_pair" {
		t.Fatalf("single mode appeared pair-ready: %#v", plan)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PlanHistory(ctx, state, media, []string{id}); err != context.Canceled {
		t.Fatalf("cancelled inspection = %v", err)
	}
	missing := filepath.Join(state, "must-not-create")
	if _, err := PlanHistory(context.Background(), missing, media, []string{id}); err == nil {
		t.Fatal("missing state root was accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("inspection created its state root")
	}
}
