// SPDX-License-Identifier: AGPL-3.0-or-later

package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/asciimoo/hister/server/document"
	yt "github.com/asciimoo/hister/server/extractor/extractors/yt_downloader"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"

	"gorm.io/gorm"
)

type historyImportFixture struct {
	indexer     *Indexer
	state, root string
	approval    *yt.HistoryApproval
	previous    []model.MediaArtifact
}

func historyImporter(t *testing.T, selected bool) historyImportFixture {
	t.Helper()
	cfg := testutil.Config(t)
	cfg.Server.Database = "history-import.sqlite3"
	testutil.InitModelWithConfig(t, cfg)
	idx := newTestIndexer(t, cfg)
	t.Cleanup(idx.Close)
	if err := idx.Add(&document.Document{URL: mediaSourceURL, Text: "preserve source content", Processed: true}); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixture := historyImportFixture{indexer: idx, state: filepath.Join(root, "state"), root: filepath.Join(root, "media")}
	if selected {
		fixture.previous = testutil.SeedMediaPair(t, mediaSourceURL, 0,
			testutil.WriteFile(t, root, "previous.webm", []byte("previous video")), testutil.WriteFile(t, root, "previous.mp3", []byte("previous audio")))
	}
	source, err := model.EnsureMediaSource(mediaSourceURL, 0)
	if err != nil {
		t.Fatal(err)
	}
	video, audio := "hister-"+strings.Repeat("a", 32)+"-video", "hister-"+strings.Repeat("b", 32)+"-audio"
	testutil.WriteHistoryReports(t, fixture.state, fixture.root, video, mediaSourceURL, "abcdefghijk", "video")
	testutil.WriteHistoryReports(t, fixture.state, fixture.root, audio, mediaSourceURL, "abcdefghijk", "audio")
	plan, err := yt.PlanHistory(context.Background(), fixture.state, fixture.root, []string{video, audio})
	if err != nil {
		t.Fatal(err)
	}
	owner := uint(0)
	fixture.approval = &yt.HistoryApproval{
		SchemaVersion: 1, ApprovalID: "reviewed-history-1", Action: "retain_history",
		Source: yt.HistoryApprovalSource{SourceID: source.ID, DocumentID: mediaSourceURL, OwnerID: &owner, URL: mediaSourceURL},
	}
	for _, job := range plan.Jobs {
		if job.File == nil {
			t.Fatalf("invalid fixture evidence: %#v", job)
		}
		data, err := os.ReadFile(filepath.Join(fixture.root, job.File.RelativePath))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		approved := yt.HistoryApprovalJob{JobID: job.JobID, EvidenceSHA256: job.EvidenceSHA256, MediaSHA256: hex.EncodeToString(hash[:])}
		if job.Role == model.MediaVideo {
			fixture.approval.Video = approved
		} else {
			fixture.approval.Audio = approved
		}
	}
	return fixture
}

func TestHistoryCheckAndConfirmedApply(t *testing.T) {
	f := historyImporter(t, true)
	actor := MediaActor{IsAdmin: true}
	before, err := model.GetMediaSourceByID(f.approval.Source.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	checked, err := f.indexer.CheckHistoricalMedia(context.Background(), f.state, f.root, f.approval, actor)
	if err != nil || checked.Replayed || len(checked.ConfirmationSHA256) != 64 {
		t.Fatalf("check = %#v, %v", checked, err)
	}
	after, err := model.GetMediaSourceByID(f.approval.Source.SourceID)
	if err != nil || before.UpdatedAt != after.UpdatedAt || before.NextGeneration != after.NextGeneration || before.SelectedSetID != after.SelectedSetID {
		t.Fatalf("check mutated source: %#v %#v %v", before, after, err)
	}
	for _, confirmation := range []string{"", "invalid"} {
		if _, err := f.indexer.ApplyHistoricalMedia(context.Background(), f.state, f.root, f.approval, confirmation, actor); !errors.Is(err, ErrMediaHistoryConfirmation) {
			t.Fatalf("confirmation %q = %v", confirmation, err)
		}
	}
	if _, err := f.indexer.ApplyHistoricalMedia(context.Background(), f.state, f.root, f.approval, strings.Repeat("0", 64), actor); !errors.Is(err, ErrMediaHistoryConfirmation) {
		t.Fatalf("stale confirmation = %v", err)
	}
	result, err := f.indexer.ApplyHistoricalMedia(context.Background(), f.state, f.root, f.approval, checked.ConfirmationSHA256, actor)
	if err != nil || result.Replayed {
		t.Fatalf("apply = %#v, %v", result, err)
	}
	replayed, err := f.indexer.CheckHistoricalMedia(context.Background(), f.state, f.root, f.approval, actor)
	if err != nil || !replayed.Replayed {
		t.Fatalf("replay check = %#v, %v", replayed, err)
	}
}

func TestHistoryCheckRequiresAdminAndLiveEvidence(t *testing.T) {
	f := historyImporter(t, false)
	if _, err := f.indexer.CheckHistoricalMedia(context.Background(), "unused", "unused", f.approval, MediaActor{}); !errors.Is(err, ErrMediaHistoryForbidden) {
		t.Fatalf("unauthorized check = %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.root, f.approval.Video.JobID, "nested", "clip.webm"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.indexer.CheckHistoricalMedia(context.Background(), f.state, f.root, f.approval, MediaActor{IsAdmin: true}); err == nil {
		t.Fatal("changed evidence passed check")
	}
}

func TestHistoryImportPreservesSelectionContentAndReplaysExactly(t *testing.T) {
	for _, selected := range []bool{false, true} {
		t.Run(map[bool]string{false: "no selection", true: "prior selection"}[selected], func(t *testing.T) {
			f := historyImporter(t, selected)
			before, err := model.ReadMediaSnapshot(mediaSourceURL, 0)
			if err != nil {
				t.Fatal(err)
			}
			first, err := f.indexer.ImportHistoricalMedia(context.Background(), f.state, f.root, f.approval, MediaActor{IsAdmin: true})
			if err != nil || first.Replayed || len(first.Artifacts) != 2 {
				t.Fatalf("import = %#v, %v", first, err)
			}
			replay, err := f.indexer.ImportHistoricalMedia(context.Background(), f.state, f.root, f.approval, MediaActor{IsAdmin: true})
			if err != nil || !replay.Replayed || replay.Receipt.ID != first.Receipt.ID || !reflect.DeepEqual(first.Artifacts, replay.Artifacts) {
				t.Fatalf("replay = %#v, %v", replay, err)
			}
			after, err := model.ReadMediaSnapshot(mediaSourceURL, 0)
			if err != nil || after.Source.SelectedSetID != before.Source.SelectedSetID || after.Source.AcceptedGeneration != before.Source.AcceptedGeneration || !reflect.DeepEqual(after.Artifacts, before.Artifacts) || !reflect.DeepEqual(after.Attempt, before.Attempt) {
				t.Fatalf("history import changed download selection/status: %#v, %v", after, err)
			}
			for _, artifact := range first.Artifacts {
				bound, err := model.LookupMediaArtifact(artifact.ID, 0, mediaSourceURL)
				if err != nil {
					t.Fatal(err)
				}
				file, _, err := media.OpenArtifact(bound.MediaArtifactInput)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if doc := f.indexer.GetByDocID(mediaSourceURL); doc == nil || doc.Text != "preserve source content" || doc.AddCount != 1 {
				t.Fatalf("history import re-indexed source: %#v", doc)
			}
			if first.Receipt.ApprovedBy != 0 || first.Receipt.ApprovalJSON == "" {
				t.Fatalf("local actor/approval audit missing: %#v", first.Receipt)
			}
		})
	}
}

func TestHistoryImportRejectsNonAdminWithoutTouchingEvidence(t *testing.T) {
	f := historyImporter(t, false)
	for _, actor := range []MediaActor{{}, {UserID: 11}, {UserID: 22}} {
		if _, err := f.indexer.ImportHistoricalMedia(context.Background(), "not-read", "not-read", f.approval, actor); !errors.Is(err, ErrMediaHistoryForbidden) {
			t.Fatalf("non-admin imported media: %v", err)
		}
	}
	f.indexer.userHandling = true
	if _, err := f.indexer.ImportHistoricalMedia(context.Background(), f.state, f.root, f.approval, MediaActor{IsAdmin: true}); !errors.Is(err, ErrMediaHistoryForbidden) {
		t.Fatalf("missing authenticated admin identity = %v", err)
	}
	result, err := f.indexer.ImportHistoricalMedia(context.Background(), f.state, f.root, f.approval, MediaActor{UserID: 91, IsAdmin: true})
	if err != nil || result.Receipt.ApprovedBy != 91 {
		t.Fatalf("verified admin import = %#v, %v", result, err)
	}
}

func TestHistoryImportRejectsStaleBindingEvidenceAndUnavailableRuntime(t *testing.T) {
	for _, kind := range []string{"revoked", "suspended", "readded", "wrong_owner", "wrong_source", "same_size_bytes", "closed", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f := historyImporter(t, true)
			ctx := context.Background()
			switch kind {
			case "revoked":
				if err := model.RevokeMediaSource(f.approval.Source.SourceID); err != nil {
					t.Fatal(err)
				}
			case "suspended":
				if err := model.SuspendMediaTransfer(f.approval.Source.SourceID, "destination", 22); err != nil {
					t.Fatal(err)
				}
			case "readded":
				if err := f.indexer.Delete(mediaSourceURL); err != nil {
					t.Fatal(err)
				}
				if err := f.indexer.Add(&document.Document{URL: mediaSourceURL, Text: "readded", Processed: true}); err != nil {
					t.Fatal(err)
				}
				if _, err := model.EnsureMediaSource(mediaSourceURL, 0); err != nil {
					t.Fatal(err)
				}
			case "wrong_owner":
				f.approval.Source.OwnerID = new(uint(22))
				f.approval.Source.DocumentID = document.GetDocID(22, mediaSourceURL)
			case "wrong_source":
				f.approval.Source.SourceID = "unknown-incarnation"
			case "same_size_bytes":
				if err := os.WriteFile(filepath.Join(f.root, f.approval.Video.JobID, "nested", "clip.webm"), []byte("other"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "closed":
				f.indexer.stopMediaQueue()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := f.indexer.ImportHistoricalMedia(ctx, f.state, f.root, f.approval, MediaActor{IsAdmin: true}); err == nil {
				t.Fatal("stale or unavailable import succeeded")
			}
			var count int64
			if err := model.DB.Model(&model.MediaHistoryReceipt{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("rejected import left an audit/registration: %d, %v", count, err)
			}
		})
	}
}

func TestHistoryImportDoesNotCreateMissingSourceIncarnation(t *testing.T) {
	f := historyImporter(t, false)
	if err := model.DB.Delete(&model.MediaSource{}, "id = ?", f.approval.Source.SourceID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.indexer.ImportHistoricalMedia(context.Background(), f.state, f.root, f.approval, MediaActor{IsAdmin: true}); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("unknown incarnation was established by an approval: %v", err)
	}
}
