// SPDX-License-Identifier: AGPL-3.0-or-later

package model_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"
)

func TestMediaSourceBootstrapCheckIsReadOnlyAndApplyIsConfirmed(t *testing.T) {
	testutil.InitModel(t)
	input := model.MediaSourceBootstrapInput{DocumentID: "11:https://www.youtube.com/watch?v=abcdefghijk", UserID: 11, URL: "https://www.youtube.com/watch?v=abcdefghijk"}
	for _, table := range []string{"media_sources", "media_attempts", "media_sets", "media_artifacts", "media_history_receipts"} {
		if err := model.DB.Exec("CREATE TRIGGER reject_" + table + "_write BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(ABORT, 'write rejected'); END").Error; err != nil {
			t.Fatal(err)
		}
		if err := model.DB.Exec("CREATE TRIGGER reject_" + table + "_update BEFORE UPDATE ON " + table + " BEGIN SELECT RAISE(ABORT, 'write rejected'); END").Error; err != nil {
			t.Fatal(err)
		}
	}
	checked, err := model.CheckMediaSourceBootstrap(input)
	if err != nil || checked.Replayed || checked.ExistingSourceID != "" || len(checked.ConfirmationSHA256) != 64 {
		t.Fatalf("check = %#v, %v", checked, err)
	}
	var sources, attempts int64
	if err := model.DB.Model(&model.MediaSource{}).Count(&sources).Error; err != nil || sources != 0 {
		t.Fatalf("read-only check created source: count=%d err=%v", sources, err)
	}
	if err := model.DB.Model(&model.MediaAttempt{}).Count(&attempts).Error; err != nil || attempts != 0 {
		t.Fatalf("read-only check created attempt: count=%d err=%v", attempts, err)
	}
	for _, confirmation := range []string{"", "invalid", strings.Repeat("0", 64)} {
		if _, err := model.ApplyMediaSourceBootstrap(input, confirmation); !errors.Is(err, model.ErrMediaSourceBootstrapConfirmation) {
			t.Fatalf("confirmation %q = %v", confirmation, err)
		}
	}
	if err := model.DB.Model(&model.MediaSource{}).Count(&sources).Error; err != nil || sources != 0 {
		t.Fatalf("rejected apply created source: count=%d err=%v", sources, err)
	}
	for _, table := range []string{"media_sources", "media_attempts", "media_sets", "media_artifacts", "media_history_receipts"} {
		_ = model.DB.Exec("DROP TRIGGER reject_" + table + "_write").Error
		_ = model.DB.Exec("DROP TRIGGER reject_" + table + "_update").Error
	}
	result, err := model.ApplyMediaSourceBootstrap(input, checked.ConfirmationSHA256)
	if err != nil || result.Replayed || result.Source.ID == "" || result.Source.DocumentID != input.DocumentID || result.Source.UserID != input.UserID || result.Source.ActiveKey == nil {
		t.Fatalf("confirmed bootstrap = %#v, %v", result, err)
	}
	stored, err := model.GetMediaSourceByID(result.Source.ID)
	if err != nil || stored.ID != result.Source.ID || stored.NextGeneration != 0 || stored.SelectedSetID != "" {
		t.Fatalf("bootstrap source = %#v, %v", stored, err)
	}
}

func TestMediaSourceBootstrapIsIdempotentAndDoesNotReviveIncarnations(t *testing.T) {
	testutil.InitModel(t)
	input := model.MediaSourceBootstrapInput{DocumentID: "https://www.youtube.com/watch?v=abcdefghijk", URL: "https://www.youtube.com/watch?v=abcdefghijk"}
	old, err := model.EnsureMediaSource(input.DocumentID, input.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.RevokeMediaSource(old.ID); err != nil {
		t.Fatal(err)
	}
	checked, err := model.CheckMediaSourceBootstrap(input)
	if err != nil || checked.Replayed || checked.RevokedIncarnations != 1 {
		t.Fatalf("revoked-source check = %#v, %v", checked, err)
	}
	result, err := model.ApplyMediaSourceBootstrap(input, checked.ConfirmationSHA256)
	if err != nil || result.Replayed || result.Source.ID == old.ID || result.Source.RevokedAt != nil {
		t.Fatalf("new incarnation = %#v, %v", result, err)
	}
	if _, err := model.GetMediaSourceByID(old.ID); err == nil {
		t.Fatal("bootstrap revived the revoked incarnation")
	}
	currentCheck, err := model.CheckMediaSourceBootstrap(input)
	if err != nil || !currentCheck.Replayed || currentCheck.ExistingSourceID != result.Source.ID {
		t.Fatalf("existing-source check = %#v, %v", currentCheck, err)
	}
	replay, err := model.ApplyMediaSourceBootstrap(input, currentCheck.ConfirmationSHA256)
	if err != nil || !replay.Replayed || replay.Source.ID != result.Source.ID {
		t.Fatalf("existing-source replay = %#v, %v", replay, err)
	}
}

func TestMediaSourceBootstrapRejectsStalePlanAndConflictingBindings(t *testing.T) {
	testutil.InitModel(t)
	input := model.MediaSourceBootstrapInput{DocumentID: "11:https://www.youtube.com/watch?v=abcdefghijk", UserID: 11, URL: "https://www.youtube.com/watch?v=abcdefghijk"}
	checked, err := model.CheckMediaSourceBootstrap(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.EnsureMediaSource(input.DocumentID, input.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := model.ApplyMediaSourceBootstrap(input, checked.ConfirmationSHA256); !errors.Is(err, model.ErrMediaSourceBootstrapConfirmation) {
		t.Fatalf("stale empty-source plan = %v", err)
	}
	otherOwner := input
	otherOwner.UserID++
	if _, err := model.CheckMediaSourceBootstrap(otherOwner); !errors.Is(err, model.ErrMediaConflict) {
		t.Fatalf("same document under a different owner = %v", err)
	}
}
