// SPDX-License-Identifier: AGPL-3.0-or-later

package model_test

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// initPostgresModel uses a unique schema so the integration test never alters
// tables or rows belonging to another application in the configured database.
func initPostgresModel(t *testing.T) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("HISTER_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set HISTER_TEST_POSTGRES_DSN to run PostgreSQL media-bootstrap integration coverage")
	}
	if !strings.Contains(dsn, "=") || strings.Contains(dsn, "://") || strings.Contains(strings.ToLower(dsn), "search_path") {
		t.Fatal("HISTER_TEST_POSTGRES_DSN must be a PostgreSQL key/value DSN without search_path")
	}

	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	schema := "hister_media_bootstrap_test_" + hex.EncodeToString(random)
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open PostgreSQL test connection: %v", err)
	}
	schemaCreated := false
	t.Cleanup(func() {
		if schemaCreated {
			if err := admin.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`).Error; err != nil {
				t.Errorf("drop isolated PostgreSQL test schema: %v", err)
			}
		}
		if db, err := admin.DB(); err == nil {
			if err := db.Close(); err != nil {
				t.Errorf("close PostgreSQL test connection: %v", err)
			}
		}
	})
	if err := admin.Exec(`CREATE SCHEMA "` + schema + `"`).Error; err != nil {
		t.Fatalf("create isolated PostgreSQL test schema: %v", err)
	}
	schemaCreated = true

	cfg := testutil.Config(t)
	cfg.Server.Database = dsn + " search_path=" + schema
	if err := model.Init(cfg); err != nil {
		if model.DB != nil {
			if db, dbErr := model.DB.DB(); dbErr == nil {
				t.Cleanup(func() {
					_ = db.Close()
					model.DB = nil
				})
			}
		}
		t.Fatalf("initialize PostgreSQL test schema: %v", err)
	}
	db, err := model.DB.DB()
	if err != nil {
		t.Fatalf("get PostgreSQL test pool: %v", err)
	}
	db.SetMaxOpenConns(8)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close PostgreSQL model pool: %v", err)
		}
		model.DB = nil
	})
}

func TestMediaRetentionPostgresRetiresOnlyHistoricalArtifacts(t *testing.T) {
	initPostgresModel(t)

	root := t.TempDir()
	writePair := func(name string) (string, string) {
		t.Helper()
		video := filepath.Join(root, name+".webm")
		audio := filepath.Join(root, name+".mp3")
		if err := os.WriteFile(video, []byte(name+" video"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(audio, []byte(name+" audio"), 0o600); err != nil {
			t.Fatal(err)
		}
		return video, audio
	}
	oldVideo, oldAudio := writePair("old")
	oldPair := testutil.SeedMediaPair(t, "postgres-retention", 41, oldVideo, oldAudio)
	newVideo, newAudio := writePair("new")
	currentPair := testutil.SeedMediaPair(t, "postgres-retention", 41, newVideo, newAudio)

	before := time.Now().UTC().Add(-24 * time.Hour)
	oldSetCreatedAt := before.Add(-24 * time.Hour)
	if err := model.DB.Model(&model.MediaSet{}).Where("id = ?", oldPair[0].SetID).Update("created_at", oldSetCreatedAt).Error; err != nil {
		t.Fatal(err)
	}
	checked, err := model.CheckMediaRetention(before)
	if err != nil {
		t.Fatal(err)
	}
	if len(checked.Artifacts) != 2 || checked.SelectedArtifactsExcluded != 2 {
		t.Fatalf("PostgreSQL retention plan = %#v; want two old and two selected artifacts", checked)
	}
	if err := model.DB.Model(&model.MediaArtifact{}).Where("id = ?", checked.Artifacts[0].Artifact.ID).Update("content_type", "video/x-changed").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := model.RetireMediaForRetention(before, checked.RegistryConfirmationSHA256); !errors.Is(err, model.ErrMediaRetentionConfirmation) {
		t.Fatalf("retire after registry changed = %v, want stale confirmation", err)
	}
	checked, err = model.CheckMediaRetention(before)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := model.RetireMediaForRetention(before, checked.RegistryConfirmationSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if len(retired) != 2 {
		t.Fatalf("retired %d PostgreSQL artifacts, want two", len(retired))
	}
	for _, item := range retired {
		if item.Artifact.RetiredAt == nil || item.SelectedSetID == item.Artifact.SetID {
			t.Fatalf("retired artifact was not tombstoned or became selected: %#v", item)
		}
		path := filepath.Join(item.Artifact.StorageRoot, item.Artifact.RelativePath)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := model.MarkMediaArtifactBytesRemoved(item.Artifact.ID); err != nil {
			t.Fatal(err)
		}
	}
	remaining, err := model.CheckMediaRetention(before)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining.Artifacts) != 0 || remaining.SelectedArtifactsExcluded != 2 {
		t.Fatalf("PostgreSQL retention after cleanup = %#v; want only selected artifacts excluded", remaining)
	}
	if len(currentPair) != 2 {
		t.Fatalf("fixture current pair has %d artifacts, want two", len(currentPair))
	}
}

func TestMediaSourceBootstrapPostgresSerializesApplyAndRejectsStalePlans(t *testing.T) {
	initPostgresModel(t)

	staleInput := model.MediaSourceBootstrapInput{
		DocumentID: "postgres-bootstrap-stale",
		UserID:     31,
		URL:        "https://www.youtube.com/watch?v=abcdefghijk",
	}
	staleCheck, err := model.CheckMediaSourceBootstrap(staleInput)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.EnsureMediaSource(staleInput.DocumentID, staleInput.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := model.ApplyMediaSourceBootstrap(staleInput, staleCheck.ConfirmationSHA256); !errors.Is(err, model.ErrMediaSourceBootstrapConfirmation) {
		t.Fatalf("apply after registry changed = %v, want stale confirmation", err)
	}

	input := model.MediaSourceBootstrapInput{
		DocumentID: "postgres-bootstrap-concurrent",
		UserID:     32,
		URL:        "https://www.youtube.com/watch?v=klmnopqrst0",
	}
	checked, err := model.CheckMediaSourceBootstrap(input)
	if err != nil {
		t.Fatal(err)
	}

	type applyResult struct {
		result *model.MediaSourceBootstrapResult
		err    error
	}
	start := make(chan struct{})
	results := make(chan applyResult, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			result, err := model.ApplyMediaSourceBootstrap(input, checked.ConfirmationSHA256)
			results <- applyResult{result: result, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	created, stale := 0, 0
	for outcome := range results {
		switch {
		case outcome.err == nil && outcome.result != nil && !outcome.result.Replayed:
			created++
		case errors.Is(outcome.err, model.ErrMediaSourceBootstrapConfirmation):
			stale++
		default:
			t.Errorf("concurrent PostgreSQL apply = %#v, want one create and one stale-plan rejection", outcome)
		}
	}
	if created != 1 || stale != 1 {
		t.Fatalf("concurrent applies: created=%d stale=%d, want exactly one of each", created, stale)
	}
	var count int64
	if err := model.DB.Model(&model.MediaSource{}).Where("document_id = ?", input.DocumentID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("concurrent applies created %d source rows, want one", count)
	}

	current, err := model.CheckMediaSourceBootstrap(input)
	if err != nil || !current.Replayed || current.ExistingSourceID == "" {
		t.Fatalf("check after create = %#v, %v", current, err)
	}
	replayed, err := model.ApplyMediaSourceBootstrap(input, current.ConfirmationSHA256)
	if err != nil || !replayed.Replayed || replayed.Source.ID != current.ExistingSourceID {
		t.Fatalf("apply existing PostgreSQL source = %#v, %v", replayed, err)
	}
}
