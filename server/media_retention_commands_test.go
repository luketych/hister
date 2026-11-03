// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"
)

func TestMediaRetentionContinuesPastBlockedFilesWithoutDeletingThem(t *testing.T) {
	fixture := mediaCommandServer(t, "exit 1")
	previous, err := model.CurrentMediaArtifacts(fixture.failed.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	videoPath := testutil.WriteFile(t, root, "current.webm", []byte("current video"))
	audioPath := testutil.WriteFile(t, root, "current.mp3", []byte("current audio"))
	current := testutil.SeedMediaPair(t, fixture.documentID, fixture.owner.ID, videoPath, audioPath)
	before := time.Now().UTC().Add(-24 * time.Hour)
	if err := model.DB.Model(&model.MediaSet{}).Where("id = ?", previous[0].SetID).Update("created_at", before.Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	blockedPath := filepath.Join(previous[0].StorageRoot, previous[0].RelativePath)
	if err := os.Remove(blockedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(current[0].StorageRoot, current[0].RelativePath), blockedPath); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(mediaRetentionRequest{Before: before.Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	post := func(path, confirmation string) *httptest.ResponseRecorder {
		t.Helper()
		headers := map[string]string{"Content-Type": "application/json", "Origin": "hister://", "X-Access-Token": fixture.admin.Token}
		if confirmation != "" {
			headers[mediaRetentionConfirmationHeader] = confirmation
		}
		return testutil.ServeHTTP(t, fixture.handler, http.MethodPost, path, strings.NewReader(string(payload)), headers)
	}
	checkPath, applyPath := "/api/yt_downloader/retention/check", "/api/yt_downloader/retention/apply"
	checked := post(checkPath, "")
	var plan mediaRetentionCheckResponse
	if checked.Code != http.StatusOK || json.Unmarshal(checked.Body.Bytes(), &plan) != nil || plan.BlockedFiles != 1 || plan.FilesToRemove != 1 {
		t.Fatalf("blocked retention check = %d %s", checked.Code, checked.Body)
	}
	applied := post(applyPath, plan.ConfirmationSHA256)
	var result mediaRetentionApplyResponse
	if applied.Code != http.StatusOK || json.Unmarshal(applied.Body.Bytes(), &result) != nil || result.RetiredArtifacts != 2 || result.RemovedFiles != 1 || len(result.PendingFiles) != 1 {
		t.Fatalf("blocked retention apply = %d %s", applied.Code, applied.Body)
	}
	info, err := os.Lstat(blockedPath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("blocked symlink was removed or replaced: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(previous[1].StorageRoot, previous[1].RelativePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("safe historical file was not removed: %v", err)
	}
	for _, item := range current {
		if _, err := os.Stat(filepath.Join(item.StorageRoot, item.RelativePath)); err != nil {
			t.Fatalf("current selected file was removed: %v", err)
		}
	}
	for _, item := range previous {
		var artifact model.MediaArtifact
		if err := model.DB.First(&artifact, "id = ?", item.ID).Error; err != nil || artifact.RetiredAt == nil {
			t.Fatalf("blocked-plan artifact lacks tombstone: %#v %v", artifact, err)
		}
		if item.ID == previous[0].ID && artifact.BytesRemovedAt != nil {
			t.Fatalf("blocked file was recorded as removed: %#v", artifact)
		}
	}
}

func TestMediaRetentionHTTPChecksAndRetiresBeforeRemovingOnlyHistoricalFiles(t *testing.T) {
	fixture := mediaCommandServer(t, "exit 1")
	previous, err := model.CurrentMediaArtifacts(fixture.failed.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	videoPath := testutil.WriteFile(t, root, "older.webm", []byte("older video"))
	audioPath := testutil.WriteFile(t, root, "older.mp3", []byte("older audio"))
	current := testutil.SeedMediaPair(t, fixture.documentID, fixture.owner.ID, videoPath, audioPath)
	before := time.Now().UTC().Add(-24 * time.Hour)
	old := before.Add(-24 * time.Hour)
	setIDs := []string{previous[0].SetID, current[0].SetID}
	if err := model.DB.Model(&model.MediaSet{}).Where("id IN ?", setIDs).Update("created_at", old).Error; err != nil {
		t.Fatal(err)
	}

	payload, err := json.Marshal(mediaRetentionRequest{Before: before.Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	post := func(path string, user *model.User, body, confirmation string) *httptest.ResponseRecorder {
		t.Helper()
		headers := map[string]string{"Content-Type": "application/json", "Origin": "hister://"}
		if user != nil {
			headers["X-Access-Token"] = user.Token
		}
		if confirmation != "" {
			headers[mediaRetentionConfirmationHeader] = confirmation
		}
		return testutil.ServeHTTP(t, fixture.handler, http.MethodPost, path, strings.NewReader(body), headers)
	}
	checkPath, applyPath := "/api/yt_downloader/retention/check", "/api/yt_downloader/retention/apply"
	if rec := post(checkPath, fixture.owner, string(payload), ""); rec.Code != http.StatusForbidden {
		t.Fatalf("ordinary owner check = %d %s", rec.Code, rec.Body)
	}
	checked := post(checkPath, fixture.admin, string(payload), "")
	if checked.Code != http.StatusOK || checked.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("admin check = %d %s headers=%v", checked.Code, checked.Body, checked.Header())
	}
	var plan mediaRetentionCheckResponse
	if err := json.Unmarshal(checked.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.RetireArtifacts != 2 || plan.FilesToRemove != 2 || plan.SelectedArtifactsExcluded != 2 || plan.BlockedFiles != 0 || len(plan.ConfirmationSHA256) != 64 {
		t.Fatalf("retention plan = %#v", plan)
	}
	for _, item := range plan.Artifacts {
		if item.FileStatus != "ready" || item.Retired {
			t.Fatalf("unexpected candidate = %#v", item)
		}
	}
	changedPath := filepath.Join(previous[0].StorageRoot, previous[0].RelativePath)
	originalBytes, err := os.ReadFile(changedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changedPath, []byte(strings.Repeat("x", len(originalBytes))), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := post(applyPath, fixture.admin, string(payload), plan.ConfirmationSHA256); rec.Code != http.StatusConflict {
		t.Fatalf("apply after same-size file change = %d %s", rec.Code, rec.Body)
	}
	if err := os.WriteFile(changedPath, originalBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	checked = post(checkPath, fixture.admin, string(payload), "")
	if checked.Code != http.StatusOK || json.Unmarshal(checked.Body.Bytes(), &plan) != nil {
		t.Fatalf("recheck after restoring file = %d %s", checked.Code, checked.Body)
	}
	if rec := post(applyPath, fixture.admin, string(payload), ""); rec.Code != http.StatusConflict {
		t.Fatalf("apply without confirmation = %d %s", rec.Code, rec.Body)
	}
	if rec := post(applyPath, fixture.admin, string(payload), strings.Repeat("0", 64)); rec.Code != http.StatusConflict {
		t.Fatalf("apply with stale confirmation = %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{videoPath, audioPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("rejected apply removed %s: %v", path, err)
		}
	}
	for _, item := range previous {
		var artifact model.MediaArtifact
		if err := model.DB.First(&artifact, "id = ?", item.ID).Error; err != nil || artifact.RetiredAt != nil {
			t.Fatalf("rejected apply retired artifact %#v: %#v %v", item, artifact, err)
		}
	}
	if err := model.DB.Exec("CREATE TRIGGER reject_media_retirement BEFORE UPDATE OF retired_at ON media_artifacts BEGIN SELECT RAISE(ABORT, 'retirement rejected'); END").Error; err != nil {
		t.Fatal(err)
	}
	if rec := post(applyPath, fixture.admin, string(payload), plan.ConfirmationSHA256); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("database-failed apply = %d %s", rec.Code, rec.Body)
	}
	for _, item := range previous {
		path := filepath.Join(item.StorageRoot, item.RelativePath)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("file was removed before retirement committed: %s: %v", path, err)
		}
	}
	if err := model.DB.Exec("DROP TRIGGER reject_media_retirement").Error; err != nil {
		t.Fatal(err)
	}

	applied := post(applyPath, fixture.admin, string(payload), plan.ConfirmationSHA256)
	if applied.Code != http.StatusOK {
		t.Fatalf("confirmed retention apply = %d %s", applied.Code, applied.Body)
	}
	var result mediaRetentionApplyResponse
	if err := json.Unmarshal(applied.Body.Bytes(), &result); err != nil || result.RetiredArtifacts != 2 || result.RemovedFiles != 2 || result.MissingFiles != 0 || len(result.PendingFiles) != 0 {
		t.Fatalf("retention apply result = %#v, %v", result, err)
	}
	for _, item := range previous {
		path := filepath.Join(item.StorageRoot, item.RelativePath)
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("pruned file remains or stat failed: %s: %v", path, err)
		}
	}
	for _, path := range []string{videoPath, audioPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("current selected file was removed: %s: %v", path, err)
		}
	}
	for _, item := range previous {
		var artifact model.MediaArtifact
		if err := model.DB.First(&artifact, "id = ?", item.ID).Error; err != nil || artifact.RetiredAt == nil || artifact.BytesRemovedAt == nil {
			t.Fatalf("pruned artifact lacks tombstone/removal record: %#v %v", artifact, err)
		}
	}
	for _, item := range current {
		var artifact model.MediaArtifact
		if err := model.DB.First(&artifact, "id = ?", item.ID).Error; err != nil || artifact.RetiredAt != nil {
			t.Fatalf("current artifact was retired: %#v %v", artifact, err)
		}
	}
	if active, err := model.CurrentMediaArtifacts(fixture.failed.SourceID); err != nil || len(active) != 2 || active[0].SetID != current[0].SetID {
		t.Fatalf("current selection after retention = %#v, %v", active, err)
	}
	remaining := post(checkPath, fixture.admin, string(payload), "")
	var remainingPlan mediaRetentionCheckResponse
	if remaining.Code != http.StatusOK || json.Unmarshal(remaining.Body.Bytes(), &remainingPlan) != nil || len(remainingPlan.Artifacts) != 0 {
		t.Fatalf("completed cleanup remained eligible: %d %s", remaining.Code, remaining.Body)
	}
	for _, malformed := range []string{
		`{"before":"2020-01-01T00:00:00Z","before":"2021-01-01T00:00:00Z"}`,
		`{"Before":"2020-01-01T00:00:00Z"}`,
		`{"before":"2020-01-01T00:00:00Z","extra":true}`,
	} {
		if rec := post(checkPath, fixture.admin, malformed, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("malformed retention request = %d %s for %s", rec.Code, rec.Body, malformed)
		}
	}
}
