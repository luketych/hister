// SPDX-License-Identifier: AGPL-3.0-or-later

package model_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/asciimoo/hister/server/model"
)

func historyInput(source *model.MediaSource) model.MediaHistoryInput {
	root := filepath.Join(os.TempDir(), "hister-media-history-test")
	return model.MediaHistoryInput{
		ApprovalID: "approval-1", SourceID: source.ID, DocumentID: source.DocumentID,
		UserID: source.UserID, ApprovedBy: 91, ApprovalJSON: `{"version":1}`,
		Pair: []model.MediaArtifactInput{
			{Role: model.MediaVideo, JobID: "history-video", StorageRoot: root, RelativePath: filepath.Join("old", "video.webm"), Size: 100, ContentType: "video/webm"},
			{Role: model.MediaAudio, JobID: "history-audio", StorageRoot: root, RelativePath: filepath.Join("old", "audio.mp3"), Size: 50, ContentType: "audio/mpeg"},
		},
	}
}

func TestMediaHistoryImportPreservesSelectionAndReplays(t *testing.T) {
	source := mediaSource(t)
	input := historyInput(source)
	first, err := model.ImportMediaHistory(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Receipt.ApprovedBy != input.ApprovedBy || len(first.Artifacts) != 2 {
		t.Fatalf("initial import = %#v", first)
	}
	storedSource, err := model.GetMediaSourceByID(source.ID)
	if err != nil || storedSource.SelectedSetID != "" || storedSource.AcceptedGeneration != 0 || storedSource.NextGeneration != 1 {
		t.Fatalf("history affected selection/ordering: %#v, %v", storedSource, err)
	}
	attempt, err := model.LatestMediaAttempt(source.ID)
	if err != nil || attempt != nil {
		t.Fatalf("history surfaced as latest: %#v, %v", attempt, err)
	}
	input.Pair[0], input.Pair[1] = input.Pair[1], input.Pair[0]
	input.ApprovedBy++
	replay, err := model.ImportMediaHistory(input)
	if err != nil || !replay.Replayed || replay.Receipt.ID != first.Receipt.ID || replay.Receipt.ApprovedBy != first.Receipt.ApprovedBy || replay.Receipt.SetID != first.Receipt.SetID {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	for i := range first.Artifacts {
		if replay.Artifacts[i].ID != first.Artifacts[i].ID {
			t.Fatalf("replay changed artifact identities: %#v", replay.Artifacts)
		}
	}
	if err := model.DB.Model(&model.MediaArtifact{}).Where("id = ?", first.Artifacts[0].ID).Update("retired_at", time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	replay, err = model.ImportMediaHistory(input)
	if err != nil || !replay.Replayed || replay.Artifacts[0].RetiredAt == nil {
		t.Fatalf("retired replay revived or lost record: %#v, %v", replay, err)
	}
	storedSource, _ = model.GetMediaSourceByID(source.ID)
	if storedSource.SelectedSetID != "" || storedSource.AcceptedGeneration != 0 || storedSource.NextGeneration != 1 {
		t.Fatalf("replay modified source: %#v", storedSource)
	}
}

func TestMediaHistoryImportPreservesExistingWorkAndSelection(t *testing.T) {
	source := mediaSource(t)
	published := publishMedia(t, finalizeMedia(t, acceptMedia(t, source.ID, "already-ready")))
	queued := acceptMedia(t, source.ID, "still-queued")
	before := currentMedia(t, source.ID)
	if _, err := model.ImportMediaHistory(historyInput(source)); err != nil {
		t.Fatal(err)
	}
	stored, err := model.GetMediaSourceByID(source.ID)
	if err != nil || stored.SelectedSetID != published.ID || stored.AcceptedGeneration != 1 || stored.NextGeneration != 3 {
		t.Fatalf("history changed selected state: %#v, %v", stored, err)
	}
	if got := currentMedia(t, source.ID); len(got) != len(before) || got[0].ID != before[0].ID || got[1].ID != before[1].ID {
		t.Fatalf("history changed existing artifacts: %#v", got)
	}
	if got := storedMediaAttempt(t, queued.ID); got.State != model.MediaQueued {
		t.Fatalf("history changed queued work: %#v", got)
	}
}

func TestMediaHistoryChangedInputAndBindingConflicts(t *testing.T) {
	source := mediaSource(t)
	if _, err := model.ImportMediaHistory(historyInput(source)); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*model.MediaHistoryInput){
		func(in *model.MediaHistoryInput) { in.ApprovalJSON = `{"version": 1}` },
		func(in *model.MediaHistoryInput) { in.Pair[0].StorageRoot = filepath.Join(os.TempDir(), "other") },
		func(in *model.MediaHistoryInput) { in.Pair[0].RelativePath = filepath.Join("else", "video.webm") },
		func(in *model.MediaHistoryInput) { in.Pair[0].Size++ },
		func(in *model.MediaHistoryInput) { in.Pair[0].JobID = "else" },
		func(in *model.MediaHistoryInput) {
			in.Pair[0].Role, in.Pair[1].Role = model.MediaAudio, model.MediaVideo
			in.Pair[0].ContentType, in.Pair[1].ContentType = "audio/mpeg", "video/webm"
		},
		func(in *model.MediaHistoryInput) { in.DocumentID = "moved" },
		func(in *model.MediaHistoryInput) { in.UserID++ },
	}
	for _, mutate := range mutations {
		changed := historyInput(source)
		mutate(&changed)
		if _, err := model.ImportMediaHistory(changed); !errors.Is(err, model.ErrMediaConflict) {
			t.Fatalf("changed approval reused receipt: %v", err)
		}
	}
	invalid := historyInput(source)
	invalid.ApprovalJSON = `[]`
	if _, err := model.ImportMediaHistory(invalid); err == nil {
		t.Fatal("non-object approval accepted")
	}
	invalid = historyInput(source)
	invalid.ApprovalID = string(make([]byte, 121))
	if _, err := model.ImportMediaHistory(invalid); err == nil {
		t.Fatal("unbounded approval ID accepted")
	}
}

func TestMediaHistoryLiveSourceScopeAndRollback(t *testing.T) {
	for _, phase := range []string{"suspended", "revoked", "moved"} {
		t.Run(phase, func(t *testing.T) {
			source := mediaSource(t)
			input := historyInput(source)
			switch phase {
			case "suspended":
				if err := model.SuspendMediaTransfer(source.ID, "destination", source.UserID+1); err != nil {
					t.Fatal(err)
				}
			case "revoked":
				if err := model.RevokeMediaSource(source.ID); err != nil {
					t.Fatal(err)
				}
			case "moved":
				if err := model.DB.Model(&model.MediaSource{}).Where("id = ?", source.ID).Updates(map[string]any{"document_id": "destination", "user_id": source.UserID + 1}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if _, err := model.ImportMediaHistory(input); err == nil {
				t.Fatal("import accepted non-live/moved source")
			}
		})
	}
	source := mediaSource(t)
	input := historyInput(source)
	if err := model.DB.Exec(`CREATE TRIGGER reject_history BEFORE INSERT ON media_history_receipts BEGIN SELECT RAISE(ABORT, 'injected receipt failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := model.ImportMediaHistory(input); err == nil {
		t.Fatal("ignored SQL failure")
	}
	var attempts, sets, artifacts, receipts int64
	for table, count := range map[string]*int64{"media_attempts": &attempts, "media_sets": &sets, "media_artifacts": &artifacts, "media_history_receipts": &receipts} {
		if err := model.DB.Table(table).Count(count).Error; err != nil {
			t.Fatal(err)
		}
	}
	stored, err := model.GetMediaSourceByID(source.ID)
	if err != nil || attempts != 0 || sets != 0 || artifacts != 0 || receipts != 0 || stored.NextGeneration != 0 {
		t.Fatalf("failed transaction left rows/allocation: counts=%d/%d/%d/%d source=%#v err=%v", attempts, sets, artifacts, receipts, stored, err)
	}
}

func TestMediaHistoryJobUniquenessAndConcurrentReplay(t *testing.T) {
	source := mediaSource(t)
	registered := acceptMedia(t, source.ID, "registered")
	if err := model.DB.Model(&model.MediaAttempt{}).Where("id = ?", registered.ID).Update("video_job_id", "history-video").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := model.ImportMediaHistory(historyInput(source)); err == nil {
		t.Fatal("job registered on another attempt was adopted")
	}
	source = mediaSource(t)
	input := historyInput(source)
	if _, err := model.ImportMediaHistory(input); err != nil {
		t.Fatal(err)
	}
	foreign := historyInput(source)
	foreign.ApprovalID = "another-approval"
	if _, err := model.ImportMediaHistory(foreign); err == nil {
		t.Fatal("foreign artifact jobs were adopted")
	}

	source = mediaSource(t)
	input = historyInput(source)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			<-start
			if _, err := model.ImportMediaHistory(input); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	var receipts, attempts, sets, artifacts int64
	for table, count := range map[string]*int64{"media_history_receipts": &receipts, "media_attempts": &attempts, "media_sets": &sets, "media_artifacts": &artifacts} {
		if err := model.DB.Table(table).Count(count).Error; err != nil {
			t.Fatal(err)
		}
	}
	if receipts != 1 || attempts != 1 || sets != 1 || artifacts != 2 {
		t.Fatalf("concurrent imports duplicated data: %d/%d/%d/%d", receipts, attempts, sets, artifacts)
	}
}

func TestMediaHistoryDoesNotChangeLatestOrRetryEligibility(t *testing.T) {
	source := mediaSource(t)
	failed := failedMediaCommand(t, source.ID, "failed-before-history")
	if _, err := model.ImportMediaHistory(historyInput(source)); err != nil {
		t.Fatal(err)
	}
	latest, err := model.LatestMediaAttempt(source.ID)
	if err != nil || latest == nil || latest.ID != failed.ID {
		t.Fatalf("latest changed: %#v, %v", latest, err)
	}
	snapshot, err := model.ReadMediaSnapshot(source.DocumentID, source.UserID)
	if err != nil || snapshot.Attempt == nil || snapshot.Attempt.ID != failed.ID || snapshot.Source.SelectedSetID != "" {
		t.Fatalf("snapshot reported history as download: %#v, %v", snapshot, err)
	}
	retry, err := model.RetryMediaAttempt(model.MediaScope{DocumentID: source.DocumentID, UserID: source.UserID}, failed.ID, `{}`)
	if err != nil || retry.Attempt.State != model.MediaQueued {
		t.Fatalf("history blocked retry: %#v, %v", retry, err)
	}
}
