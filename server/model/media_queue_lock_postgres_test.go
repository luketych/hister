// SPDX-License-Identifier: AGPL-3.0-or-later

package model_test

import (
	"errors"
	"testing"

	"github.com/asciimoo/hister/server/model"
)

func TestMediaQueuePostgresLockExcludesConcurrentOwners(t *testing.T) {
	initPostgresModel(t)

	first, err := model.AcquireMediaQueueLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.AcquireMediaQueueLock(); !errors.Is(err, model.ErrMediaQueueAlreadyRunning) {
		_ = first.Close()
		t.Fatalf("second owner acquired PostgreSQL queue lock: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := model.AcquireMediaQueueLock()
	if err != nil {
		t.Fatalf("PostgreSQL lock was not released: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
