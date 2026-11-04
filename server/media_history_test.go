// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/server/document"
	yt "github.com/asciimoo/hister/server/extractor/extractors/yt_downloader"
	"github.com/asciimoo/hister/server/indexer"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"
)

func serverHistoryApproval(t *testing.T, fixture *mediaCommandFixture) (*yt.HistoryApproval, string) {
	t.Helper()
	output, ok := fixture.cfg.Extractors["yt_downloader"].Options["output_dir"].(string)
	if !ok {
		t.Fatal("fixture has no configured media root")
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := filepath.EvalSymlinks(output)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(cache, "hister", "yt_video_downloader")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err = filepath.EvalSymlinks(state)
	if err != nil {
		t.Fatal(err)
	}
	video := "hister-" + strings.Repeat("e", 32) + "-video"
	audio := "hister-" + strings.Repeat("f", 32) + "-audio"
	testutil.WriteHistoryReports(t, state, output, video, commandSourceURL, "abcdefghijk", "video")
	testutil.WriteHistoryReports(t, state, output, audio, commandSourceURL, "abcdefghijk", "audio")
	plan, err := yt.PlanHistory(context.Background(), state, output, []string{video, audio})
	if err != nil {
		t.Fatal(err)
	}
	owner := fixture.owner.ID
	approval := &yt.HistoryApproval{
		SchemaVersion: 1, ApprovalID: "http-reviewed-history", Action: "retain_history",
		Source: yt.HistoryApprovalSource{SourceID: fixture.failed.SourceID, DocumentID: fixture.documentID, OwnerID: &owner, URL: commandSourceURL},
	}
	for _, job := range plan.Jobs {
		if job.File == nil {
			t.Fatalf("history fixture job is not ready: %#v", job)
		}
		data, err := os.ReadFile(filepath.Join(output, job.File.RelativePath))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		approved := yt.HistoryApprovalJob{JobID: job.JobID, EvidenceSHA256: job.EvidenceSHA256, MediaSHA256: hex.EncodeToString(hash[:])}
		if job.Role == model.MediaVideo {
			approval.Video = approved
		} else {
			approval.Audio = approved
		}
	}
	return approval, output
}

func TestHistoryOperatorRequiresTrustworthyAdminAuthority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config config.App
		userID uint
		admin  bool
		auth   bool
		allow  bool
	}{
		{name: "multi-user admin", config: config.App{UserHandling: true}, userID: 9, admin: true, allow: true},
		{name: "multi-user ordinary user", config: config.App{UserHandling: true}, userID: 9},
		{name: "public without credentials", config: config.App{Public: true}},
		{name: "private single-user local authority", allow: true},
		{name: "public token operator", config: config.App{Public: true, AccessToken: "configured"}, auth: true, allow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &webContext{Request: httptest.NewRequest(http.MethodPost, "/", nil), Response: httptest.NewRecorder(), Config: &config.Config{App: tc.config}, UserID: tc.userID, IsAdmin: tc.admin, Authenticated: tc.auth}
			actor, ok := historyOperator(ctx)
			if ok != tc.allow || (ok && !actor.IsAdmin) {
				t.Fatalf("historyOperator() = %#v, %v; allow=%v", actor, ok, tc.allow)
			}
		})
	}
}

func TestMediaSourceBootstrapHTTPIsAdminOnlyCheckedAndNonDestructive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := testutil.Config(t)
	cfg.App.UserHandling, cfg.App.Public = true, true
	cfg.Server.Database = "media-source-bootstrap.sqlite3"
	cfg.Indexer.DetectLanguages = false
	if err := cfg.UpdateBaseURL("http://127.0.0.1:4433"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveRules(); err != nil {
		t.Fatal(err)
	}
	testutil.InitModelWithConfig(t, cfg)
	previousStore := sessionStore
	sessionStore = newSessionStore([]byte(strings.Repeat("x", 32)), cfg.BaseURL(""), sessionMaxAge)
	t.Cleanup(func() { sessionStore = previousStore })
	owner := testutil.CreateUser(t, "bootstrap-owner")
	admin, err := model.CreateUser("bootstrap-admin", "password123", true)
	if err != nil {
		t.Fatal(err)
	}
	idx := newServerTestIndexer(t, cfg)
	url := "https://www.youtube.com/watch?v=bootstrap01"
	documentID := document.GetDocID(owner.ID, url)
	if err := idx.Add(&document.Document{URL: url, UserID: owner.ID, Text: "pre-registry YouTube source", Processed: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := model.GetMediaSource(documentID); err == nil {
		t.Fatal("fixture unexpectedly created a media source")
	}
	handler := registerEndpoints(cfg, idx)
	body, err := json.Marshal(map[string]any{"document_id": documentID, "owner_id": owner.ID, "url": url})
	if err != nil {
		t.Fatal(err)
	}
	post := func(path string, user *model.User, payload, confirmation string) *httptest.ResponseRecorder {
		t.Helper()
		headers := map[string]string{"Content-Type": "application/json", "Origin": "hister://"}
		if user != nil {
			headers["X-Access-Token"] = user.Token
		}
		if confirmation != "" {
			headers[mediaSourceBootstrapConfirmationHeader] = confirmation
		}
		return testutil.ServeHTTP(t, handler, http.MethodPost, path, strings.NewReader(payload), headers)
	}
	checkPath := "/api/yt_downloader/source/check"
	applyPath := "/api/yt_downloader/source/apply"
	if rec := post(checkPath, owner, string(body), ""); rec.Code != http.StatusForbidden {
		t.Fatalf("ordinary owner check = %d %s", rec.Code, rec.Body)
	}
	checked := post(checkPath, admin, string(body), "")
	if checked.Code != http.StatusOK {
		t.Fatalf("admin check = %d %s", checked.Code, checked.Body)
	}
	var check mediaSourceBootstrapCheckResponse
	if err := json.Unmarshal(checked.Body.Bytes(), &check); err != nil || check.DocumentID != documentID || check.OwnerID != owner.ID || check.Replayed || check.ExistingSourceID != "" || len(check.ConfirmationSHA256) != 64 {
		t.Fatalf("bootstrap check response = %#v, %v", check, err)
	}
	if _, err := model.GetMediaSource(documentID); err == nil {
		t.Fatal("read-only check created a source")
	}
	if rec := post(applyPath, admin, string(body), ""); rec.Code != http.StatusConflict {
		t.Fatalf("apply without confirmation = %d %s", rec.Code, rec.Body)
	}
	if _, err := model.GetMediaSource(documentID); err == nil {
		t.Fatal("rejected apply created a source")
	}
	if rec := post(applyPath, admin, string(body), strings.Repeat("0", 64)); rec.Code != http.StatusConflict {
		t.Fatalf("apply with stale confirmation = %d %s", rec.Code, rec.Body)
	}
	applied := post(applyPath, admin, string(body), check.ConfirmationSHA256)
	if applied.Code != http.StatusCreated {
		t.Fatalf("confirmed apply = %d %s", applied.Code, applied.Body)
	}
	var result mediaSourceBootstrapApplyResponse
	if err := json.Unmarshal(applied.Body.Bytes(), &result); err != nil || result.DocumentID != documentID || result.OwnerID != owner.ID || result.SourceID == "" || result.Replayed {
		t.Fatalf("bootstrap apply response = %#v, %v", result, err)
	}
	source, err := model.GetMediaSource(documentID)
	if err != nil || source.ID != result.SourceID || source.UserID != owner.ID || source.SelectedSetID != "" {
		t.Fatalf("created source = %#v, %v", source, err)
	}
	for _, malformed := range []string{
		`{"document_id":"a","document_id":"b","owner_id":1,"url":"https://www.youtube.com/watch?v=abcdefghijk"}`,
		`{"Document_ID":"a","owner_id":1,"url":"https://www.youtube.com/watch?v=abcdefghijk"}`,
		`{"document_id":"a","owner_id":1,"url":"https://www.youtube.com/watch?v=abcdefghijk","extra":true}`,
		`{"document_id":"a","owner_id":null,"url":"https://www.youtube.com/watch?v=abcdefghijk"}`,
	} {
		if rec := post(checkPath, admin, malformed, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("malformed bootstrap request = %d %s for %s", rec.Code, rec.Body, malformed)
		}
	}
	if rec := post(checkPath, admin, strings.Replace(string(body), url, "https://www.youtube.com/watch?v=otherVideo1", 1), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("URL mismatch check = %d %s", rec.Code, rec.Body)
	}
	if count := idx.GetByDocID(documentID); count == nil || count.Text != "pre-registry YouTube source" || count.AddCount != 1 {
		t.Fatalf("bootstrap changed indexed document: %#v", count)
	}
	var attempts, receipts int64
	if err := model.DB.Model(&model.MediaAttempt{}).Count(&attempts).Error; err != nil || attempts != 0 {
		t.Fatalf("bootstrap created attempts: %d %v", attempts, err)
	}
	if err := model.DB.Model(&model.MediaHistoryReceipt{}).Count(&receipts).Error; err != nil || receipts != 0 {
		t.Fatalf("bootstrap created history receipts: %d %v", receipts, err)
	}
}

func TestMediaHistoryHTTPCheckAndConfirmationGuardedApply(t *testing.T) {
	fixture := mediaCommandServer(t, "exit 1")
	approval, output := serverHistoryApproval(t, fixture)
	data, err := json.Marshal(approval)
	if err != nil {
		t.Fatal(err)
	}
	post := func(path string, user *model.User, confirmation string) *httptest.ResponseRecorder {
		t.Helper()
		headers := map[string]string{"Content-Type": "application/json", "Origin": "hister://"}
		if user != nil {
			headers["X-Access-Token"] = user.Token
		}
		if confirmation != "" {
			headers["X-Hister-Media-Confirmation"] = confirmation
		}
		return testutil.ServeHTTP(t, fixture.handler, http.MethodPost, path, strings.NewReader(string(data)), headers)
	}
	if rec := post("/api/yt_downloader/history/check", fixture.owner, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("owner check = %d %s", rec.Code, rec.Body)
	}
	before, err := model.GetMediaSourceByID(fixture.failed.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	checked := post("/api/yt_downloader/history/check", fixture.admin, "")
	if checked.Code != http.StatusOK {
		t.Fatalf("admin check = %d %s", checked.Code, checked.Body)
	}
	var check struct {
		ApprovalID         string `json:"approval_id"`
		ConfirmationSHA256 string `json:"confirmation_sha256"`
		Replayed           bool   `json:"replayed"`
	}
	if err := json.Unmarshal(checked.Body.Bytes(), &check); err != nil || check.ApprovalID != approval.ApprovalID || len(check.ConfirmationSHA256) != 64 || check.Replayed {
		t.Fatalf("check response = %#v, %v", check, err)
	}
	afterCheck, err := model.GetMediaSourceByID(fixture.failed.SourceID)
	if err != nil || before.UpdatedAt != afterCheck.UpdatedAt || before.NextGeneration != afterCheck.NextGeneration || before.SelectedSetID != afterCheck.SelectedSetID {
		t.Fatalf("check changed source: %#v %#v %v", before, afterCheck, err)
	}
	var receipts int64
	if err := model.DB.Model(&model.MediaHistoryReceipt{}).Count(&receipts).Error; err != nil || receipts != 0 {
		t.Fatalf("check wrote receipts: %d %v", receipts, err)
	}
	if rec := post("/api/yt_downloader/history/apply", fixture.admin, ""); rec.Code != http.StatusConflict {
		t.Fatalf("apply without confirmation = %d %s", rec.Code, rec.Body)
	}
	if err := model.DB.Model(&model.MediaHistoryReceipt{}).Count(&receipts).Error; err != nil || receipts != 0 {
		t.Fatalf("rejected apply wrote receipts: %d %v", receipts, err)
	}
	applied := post("/api/yt_downloader/history/apply", fixture.admin, check.ConfirmationSHA256)
	if applied.Code != http.StatusCreated {
		t.Fatalf("confirmed apply = %d %s", applied.Code, applied.Body)
	}
	var result struct {
		ApprovalID string `json:"approval_id"`
		ReceiptID  string `json:"receipt_id"`
		SetID      string `json:"set_id"`
		Replayed   bool   `json:"replayed"`
	}
	if err := json.Unmarshal(applied.Body.Bytes(), &result); err != nil || result.ApprovalID != approval.ApprovalID || result.ReceiptID == "" || result.SetID == "" || result.Replayed {
		t.Fatalf("apply response = %#v, %v", result, err)
	}
	replay := post("/api/yt_downloader/history/apply", fixture.admin, check.ConfirmationSHA256)
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), `"replayed":true`) {
		t.Fatalf("exact apply replay = %d %s", replay.Code, replay.Body)
	}
	if _, err := os.Stat(filepath.Join(output, approval.Video.JobID)); err != nil {
		t.Fatalf("HTTP apply moved retained media: %v", err)
	}
}

func TestApprovedHistoryRestoresOnlyScopedLinksWithoutChangingReadySelection(t *testing.T) {
	fixture := mediaCommandServer(t, "exit 1")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, output := filepath.Join(root, "state"), filepath.Join(root, "media")
	video, audio := "hister-"+strings.Repeat("c", 32)+"-video", "hister-"+strings.Repeat("d", 32)+"-audio"
	videoPath := testutil.WriteHistoryReports(t, state, output, video, commandSourceURL, "abcdefghijk", "video")
	testutil.WriteHistoryReports(t, state, output, audio, commandSourceURL, "abcdefghijk", "audio")
	plan, err := yt.PlanHistory(context.Background(), state, output, []string{video, audio})
	if err != nil {
		t.Fatal(err)
	}
	owner := fixture.owner.ID
	approval := &yt.HistoryApproval{
		SchemaVersion: 1, ApprovalID: "reviewed-server-history", Action: "retain_history",
		Source: yt.HistoryApprovalSource{SourceID: fixture.failed.SourceID, DocumentID: fixture.documentID, OwnerID: &owner, URL: commandSourceURL},
	}
	for _, job := range plan.Jobs {
		data, err := os.ReadFile(filepath.Join(output, job.File.RelativePath))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		approved := yt.HistoryApprovalJob{JobID: job.JobID, EvidenceSHA256: job.EvidenceSHA256, MediaSHA256: hex.EncodeToString(hash[:])}
		if job.Role == model.MediaVideo {
			approval.Video = approved
		} else {
			approval.Audio = approved
		}
	}
	before, err := media.ReadStatus(fixture.documentID, fixture.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A plausible report/URI does not grant playback before actual approved
	// registration, even when the bytes exist and the caller owns the source.
	unregistered := model.MediaArtifact{MediaArtifactInput: model.MediaArtifactInput{StorageRoot: filepath.Dir(videoPath), RelativePath: filepath.Base(videoPath)}}
	oldLink := legacyArtifactURL(fixture.documentID, unregistered)
	rec := testutil.ServeHTTP(t, fixture.handler, http.MethodGet, oldLink, nil, map[string]string{"X-Access-Token": fixture.owner.Token})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reports authorized a historical URI: %d %s", rec.Code, rec.Body)
	}
	result, err := fixture.idx.ImportHistoricalMedia(context.Background(), state, output, approval, indexer.MediaActor{UserID: fixture.admin.ID, IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Receipt.ApprovedBy != fixture.admin.ID {
		t.Fatal("approval body replaced authenticated audit actor")
	}
	for _, artifact := range result.Artifacts {
		for _, path := range []string{artifactURL(artifact), legacyArtifactURL(fixture.documentID, artifact)} {
			for _, user := range []*model.User{fixture.owner, fixture.other, fixture.admin, nil} {
				headers := map[string]string{}
				if user != nil {
					headers["X-Access-Token"] = user.Token
				}
				rec := testutil.ServeHTTP(t, fixture.handler, http.MethodGet, path, nil, headers)
				if user == fixture.owner {
					if rec.Code != http.StatusOK || rec.Body.String() != "media" {
						t.Fatalf("owner historical link = %d %s", rec.Code, rec.Body)
					}
				} else if rec.Code != http.StatusNotFound {
					t.Fatalf("historical registration weakened source visibility: %d", rec.Code)
				}
			}
		}
	}
	after, err := media.ReadStatus(fixture.documentID, fixture.owner.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("history changed the selected pair/latest download: %#v, %v", after, err)
	}
	artifact := result.Artifacts[0]
	if err := model.DB.Model(&artifact).Update("retired_at", time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	replay, err := fixture.idx.ImportHistoricalMedia(context.Background(), state, output, approval, indexer.MediaActor{UserID: fixture.admin.ID, IsAdmin: true})
	if err != nil || !replay.Replayed || replay.Artifacts[0].RetiredAt == nil {
		t.Fatalf("replay revived retirement: %#v, %v", replay, err)
	}
	rec = testutil.ServeHTTP(t, fixture.handler, http.MethodGet, artifactURL(artifact), nil, map[string]string{"X-Access-Token": fixture.owner.Token})
	if rec.Code != http.StatusGone {
		t.Fatalf("retirement no longer applies after replay: %d", rec.Code)
	}
	if _, err := os.Stat(videoPath); err != nil {
		t.Fatalf("registration/replay changed retained files: %v", err)
	}
}
