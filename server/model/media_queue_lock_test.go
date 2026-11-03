// SPDX-License-Identifier: AGPL-3.0-or-later

package model_test

import (
	"errors"
	"testing"
	"time"

	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"
)

func TestMediaQueueLockExpiredOwnerCannotReleaseSuccessor(t *testing.T) {
	testutil.InitModel(t)
	stale, err := model.AcquireMediaQueueLock()
	if err != nil {
		t.Fatal(err)
	}
	source, err := model.EnsureMediaSource("lease-test-document", 0)
	if err != nil {
		_ = stale.Close()
		t.Fatal(err)
	}
	reserved, err := model.ReserveMediaAttempt(source.ID, "lease-test-request", `{}`)
	if err != nil {
		_ = stale.Close()
		t.Fatal(err)
	}
	queued, err := model.AcceptReservedMediaAttempt(reserved.ID)
	if err != nil {
		_ = stale.Close()
		t.Fatal(err)
	}
	if err := model.DB.Model(&model.MediaQueueLease{}).Where("name = ?", "dispatcher").Update("expires_at", time.Unix(0, 0).UTC()).Error; err != nil {
		_ = stale.Close()
		t.Fatal(err)
	}
	successor, err := model.AcquireMediaQueueLock()
	if err != nil {
		_ = stale.Close()
		t.Fatalf("expired lease could not be acquired: %v", err)
	}
	if err := successor.InterruptMediaAttempts(); err != nil {
		_ = stale.Close()
		_ = successor.Close()
		t.Fatal(err)
	}
	if _, err := stale.ClaimMediaAttempt(queued.ID); !errors.Is(err, model.ErrMediaQueueLockLost) {
		_ = stale.Close()
		_ = successor.Close()
		t.Fatalf("expired owner claimed work after takeover: %v", err)
	}
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := model.AcquireMediaQueueLock(); !errors.Is(err, model.ErrMediaQueueAlreadyRunning) {
		_ = successor.Close()
		t.Fatalf("stale owner released successor lease: %v", err)
	}
	if err := successor.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMediaQueueLockExcludesConcurrentOwners(t *testing.T) {
	testutil.InitModel(t)

	first, err := model.AcquireMediaQueueLock()
	if err != nil {
		t.Fatal(err)
	}
	source, err := model.EnsureMediaSource("lock-exclusion-document", 0)
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	reserved, err := model.ReserveMediaAttempt(source.ID, "lock-exclusion-request", `{}`)
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	queued, err := model.AcceptReservedMediaAttempt(reserved.ID)
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	claimed, err := first.ClaimMediaAttempt(queued.ID)
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	if _, err := model.AcquireMediaQueueLock(); !errors.Is(err, model.ErrMediaQueueAlreadyRunning) {
		_ = first.Close()
		t.Fatalf("second owner acquired queue lock: %v", err)
	}
	var after model.MediaAttempt
	if err := model.DB.First(&after, "id = ?", claimed.ID).Error; err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	if after.State != model.MediaStarting || after.ClaimToken != claimed.ClaimToken {
		_ = first.Close()
		t.Fatalf("competing owner changed live attempt: %#v", after)
	}
	if err := first.InterruptMediaAttempts(); err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.InterruptMediaAttempts(); !errors.Is(err, model.ErrMediaQueueLockRequired) {
		t.Fatalf("recovery ran after lock release: %v", err)
	}

	second, err := model.AcquireMediaQueueLock()
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
