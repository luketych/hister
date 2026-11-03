// SPDX-License-Identifier: AGPL-3.0-or-later

package model_test

import (
	"errors"
	"testing"
	"time"

	"github.com/asciimoo/hister/server/model"
)

func TestMediaRetentionPlansOnlyOldUnselectedArtifactsAndRetiresIdempotently(t *testing.T) {
	source := mediaSource(t)
	first := publishMedia(t, finalizeMedia(t, acceptMedia(t, source.ID, "retention-first")))
	second := publishMedia(t, finalizeMedia(t, acceptMedia(t, source.ID, "retention-second")))
	old := time.Now().UTC().Add(-48 * time.Hour)
	if err := model.DB.Model(&model.MediaSet{}).Where("id IN ?", []string{first.ID, second.ID}).Update("created_at", old).Error; err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(-24 * time.Hour)

	plan, err := model.CheckMediaRetention(before)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Artifacts) != 2 || plan.SelectedArtifactsExcluded != 2 || len(plan.RegistryConfirmationSHA256) != 64 {
		t.Fatalf("retention plan = %#v, want only the unselected pair", plan)
	}
	for _, item := range plan.Artifacts {
		if item.Artifact.SetID != first.ID || item.SelectedSetID != second.ID || item.Artifact.RetiredAt != nil {
			t.Fatalf("cleanup plan included selected or retired wrong artifact: %#v", item)
		}
	}
	var retired int64
	if err := model.DB.Model(&model.MediaArtifact{}).Where("retired_at IS NOT NULL").Count(&retired).Error; err != nil || retired != 0 {
		t.Fatalf("read-only retention check changed records: retired=%d err=%v", retired, err)
	}

	// A previously checked historical set becoming selected invalidates the plan.
	if err := model.DB.Model(&model.MediaSource{}).Where("id = ?", source.ID).Update("selected_set_id", first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := model.RetireMediaForRetention(before, plan.RegistryConfirmationSHA256); !errors.Is(err, model.ErrMediaRetentionConfirmation) {
		t.Fatalf("stale plan after selection change = %v, want confirmation mismatch", err)
	}
	if err := model.DB.Model(&model.MediaSource{}).Where("id = ?", source.ID).Update("selected_set_id", second.ID).Error; err != nil {
		t.Fatal(err)
	}

	plan, err = model.CheckMediaRetention(before)
	if err != nil {
		t.Fatal(err)
	}
	retiredArtifacts, err := model.RetireMediaForRetention(before, plan.RegistryConfirmationSHA256)
	if err != nil || len(retiredArtifacts) != 2 {
		t.Fatalf("retire old media = %#v, %v", retiredArtifacts, err)
	}
	for _, artifact := range retiredArtifacts {
		if artifact.Artifact.SetID != first.ID || artifact.Artifact.RetiredAt == nil {
			t.Fatalf("retirement changed selected media or omitted tombstone: %#v", artifact)
		}
	}
	current, err := model.CurrentMediaArtifacts(source.ID)
	if err != nil || len(current) != 2 || current[0].SetID != second.ID || current[0].RetiredAt != nil {
		t.Fatalf("current selection after cleanup = %#v, %v", current, err)
	}

	// Retired tombstones remain eligible so interrupted file removal can retry.
	retry, err := model.CheckMediaRetention(before)
	if err != nil || len(retry.Artifacts) != 2 {
		t.Fatalf("retry plan = %#v, %v", retry, err)
	}
	if _, err := model.RetireMediaForRetention(before, retry.RegistryConfirmationSHA256); err != nil {
		t.Fatalf("idempotent tombstone apply = %v", err)
	}
}

func TestMediaRetentionRejectsInvalidCutoffs(t *testing.T) {
	for _, before := range []time.Time{{}, time.Now().UTC().Add(time.Minute)} {
		if _, err := model.CheckMediaRetention(before); err == nil {
			t.Fatalf("accepted invalid cleanup cutoff %v", before)
		}
	}
}
