// SPDX-License-Identifier: AGPL-3.0-or-later

package model_test

import (
	"errors"
	"testing"

	"github.com/asciimoo/hister/server/model"
)

func TestCheckMediaHistoryIsReadOnlyAndApplyRequiresConfirmation(t *testing.T) {
	source := mediaSource(t)
	input := historyInput(source)
	before, err := model.GetMediaSourceByID(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"media_sources", "media_attempts", "media_sets", "media_artifacts", "media_history_receipts"} {
		if err := model.DB.Exec("CREATE TRIGGER reject_" + table + "_write BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT, 'write rejected'); END").Error; err != nil {
			t.Fatal(err)
		}
		if err := model.DB.Exec("CREATE TRIGGER reject_" + table + "_update BEFORE UPDATE ON " + table + " BEGIN SELECT RAISE(ABORT, 'write rejected'); END").Error; err != nil {
			t.Fatal(err)
		}
	}
	checked, err := model.CheckMediaHistory(input)
	if err != nil || checked.ApprovalID != input.ApprovalID || len(checked.ConfirmationSHA256) != 64 || checked.Replayed {
		t.Fatalf("check = %#v, %v", checked, err)
	}
	after, err := model.GetMediaSourceByID(source.ID)
	if err != nil || before.UpdatedAt != after.UpdatedAt || before.NextGeneration != after.NextGeneration || before.SelectedSetID != after.SelectedSetID {
		t.Fatalf("check changed source: before=%#v after=%#v err=%v", before, after, err)
	}
	for _, table := range []string{"media_attempts", "media_sets", "media_artifacts", "media_history_receipts"} {
		var count int64
		if err := model.DB.Table(table).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("check wrote %s: count=%d err=%v", table, count, err)
		}
	}
	if _, err := model.ApplyMediaHistory(input, ""); !errors.Is(err, model.ErrMediaHistoryConfirmation) {
		t.Fatalf("missing confirmation = %v", err)
	}
	if _, err := model.ApplyMediaHistory(input, "BAD"); !errors.Is(err, model.ErrMediaHistoryConfirmation) {
		t.Fatalf("invalid confirmation = %v", err)
	}
	// Remove write-rejecting triggers to permit the confirmed apply.
	for _, table := range []string{"media_sources", "media_attempts", "media_sets", "media_artifacts", "media_history_receipts"} {
		_ = model.DB.Exec("DROP TRIGGER reject_" + table + "_write").Error
		_ = model.DB.Exec("DROP TRIGGER reject_" + table + "_update").Error
	}
	applied, err := model.ApplyMediaHistory(input, checked.ConfirmationSHA256)
	if err != nil || applied.Replayed {
		t.Fatalf("confirmed apply = %#v, %v", applied, err)
	}
	replayCheck, err := model.CheckMediaHistory(input)
	if err != nil || !replayCheck.Replayed {
		t.Fatalf("replay check = %#v, %v", replayCheck, err)
	}
}

func TestCheckMediaHistoryDetectsRegistryConflicts(t *testing.T) {
	source := mediaSource(t)
	input := historyInput(source)
	if _, err := model.CheckMediaHistory(input); err != nil {
		t.Fatal(err)
	}
	occupied := acceptMedia(t, source.ID, "occupied")
	if err := model.DB.Model(&model.MediaAttempt{}).Where("id = ?", occupied.ID).Update("audio_job_id", input.Pair[0].JobID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := model.CheckMediaHistory(input); !errors.Is(err, model.ErrMediaConflict) {
		t.Fatalf("occupied producer job = %v", err)
	}
}
