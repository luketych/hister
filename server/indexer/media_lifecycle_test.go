// SPDX-License-Identifier: AGPL-3.0-or-later

package indexer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/asciimoo/hister/server/document"
	"github.com/asciimoo/hister/server/model"
	"github.com/asciimoo/hister/server/testutil"
	servertypes "github.com/asciimoo/hister/server/types"

	"gorm.io/gorm"
)

func TestMediaOwnershipMovePreservesHistoryAndFailsClosedBetweenStores(t *testing.T) {
	for _, scenario := range []string{"success", "dry run", "index failure", "registry finalization failure"} {
		t.Run(scenario, func(t *testing.T) {
			idx, root := mediaIndexer(t, mediaSuccessScript, &mediaContentExtractor{})
			if err := idx.Add(&document.Document{URL: mediaSourceURL, Text: "original", Processed: true}); err != nil {
				t.Fatal(err)
			}
			artifacts := testutil.SeedMediaPair(t, mediaSourceURL, 0,
				testutil.WriteFile(t, root, "previous.webm", []byte("previous video")), testutil.WriteFile(t, root, "previous.mp3", []byte("previous audio")))
			source, err := model.GetMediaSource(mediaSourceURL)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "index failure" {
				for name, index := range idx.indexers {
					idx.indexers[name] = failingBlobCommitIndex{bleveIndex: index, fail: func() error { return errors.New("injected move failure") }}
				}
			}
			if scenario == "registry finalization failure" {
				if err := model.DB.Exec(`CREATE TRIGGER reject_move BEFORE UPDATE OF document_id ON media_sources WHEN NEW.document_id <> OLD.document_id BEGIN SELECT RAISE(ABORT, 'injected finalization failure'); END`).Error; err != nil {
					t.Fatal(err)
				}
			}
			const target = uint(22)
			_, err = idx.UpdateByQuery("*", new(uint(0)), servertypes.DocumentChanges{UserID: new(target)}, scenario == "dry run")
			if (scenario == "success" || scenario == "dry run") && err != nil {
				t.Fatal(err)
			}
			if (scenario == "index failure" || scenario == "registry finalization failure") && err == nil {
				t.Fatal("ownership move ignored an injected failure")
			}
			newID := document.GetDocID(target, mediaSourceURL)
			for _, artifact := range artifacts {
				_, oldErr := model.LookupMediaArtifact(artifact.ID, 0, mediaSourceURL)
				_, newErr := model.LookupMediaArtifact(artifact.ID, target, newID)
				switch scenario {
				case "dry run":
					if oldErr != nil || !errors.Is(newErr, gorm.ErrRecordNotFound) {
						t.Fatalf("dry run changed bindings: old=%v, new=%v", oldErr, newErr)
					}
				case "success":
					if !errors.Is(oldErr, gorm.ErrRecordNotFound) || newErr != nil {
						t.Fatalf("move did not transfer historical binding: old=%v, new=%v", oldErr, newErr)
					}
				default:
					if !errors.Is(oldErr, gorm.ErrRecordNotFound) || !errors.Is(newErr, gorm.ErrRecordNotFound) {
						t.Fatalf("partial move remained servable: old=%v, new=%v", oldErr, newErr)
					}
				}
				if _, err := os.Stat(filepath.Join(artifact.StorageRoot, artifact.RelativePath)); err != nil {
					t.Fatalf("ownership change removed retained bytes: %v", err)
				}
			}
			stored, err := model.GetMediaSourceByID(source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "index failure" || scenario == "registry finalization failure" {
				if !stored.Suspended || stored.PendingDocumentID != newID || stored.PendingUserID != target {
					t.Fatalf("failed move lost its durable unavailable state: %#v", stored)
				}
				if got, err := model.CurrentMediaArtifacts(source.ID); err != nil || len(got) != 0 {
					t.Fatalf("suspended source exposed current artifacts: %#v, %v", got, err)
				}
			} else if stored.Suspended {
				t.Fatal("completed/dry-run move left a suspended source")
			}
		})
	}
}

func TestMediaDatabaseOutageDoesNotSilentlyDisableRevocation(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.Server.Database = "bound-registry.sqlite3"
	testutil.InitModelWithConfig(t, cfg)
	idx := newTestIndexer(t, cfg)
	if err := idx.Add(&document.Document{URL: mediaSourceURL, Text: "source", Processed: true}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	artifacts := testutil.SeedMediaPair(t, mediaSourceURL, 0,
		testutil.WriteFile(t, root, "video.webm", []byte("video")), testutil.WriteFile(t, root, "audio.mp3", []byte("audio")))
	t.Cleanup(idx.Close)
	db, err := model.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if !idx.mediaRegistry {
		t.Fatal("database outage was mistaken for an unrelated/uninitialized registry")
	}
	if err := idx.Delete(mediaSourceURL); err == nil {
		t.Fatal("deletion skipped unavailable registry revocation")
	}
	if idx.GetByDocID(mediaSourceURL) == nil {
		t.Fatal("source deletion committed before revocation could succeed")
	}
	idx.Close()
	if opened, err := New(cfg); err == nil {
		opened.Close()
		t.Fatal("startup ignored an unavailable media registry")
	}
	testutil.InitModelWithConfig(t, cfg)
	idx = newTestIndexer(t, cfg)
	t.Cleanup(idx.Close)
	if err := idx.Delete(mediaSourceURL); err != nil {
		t.Fatal(err)
	}
	if _, err := model.LookupMediaArtifact(artifacts[0].ID, 0, mediaSourceURL); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("recovered deletion left a live binding: %v", err)
	}
}

func TestMediaDisabledDispatcherDoesNotReportPhantomQueuedWorkAfterRestart(t *testing.T) {
	cfg := testutil.Config(t)
	cfg.Server.Database = "disabled-dispatcher.sqlite3"
	testutil.InitModelWithConfig(t, cfg)
	source, err := model.EnsureMediaSource(mediaSourceURL, 0)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := model.AcceptMediaAttempt(source.ID, "previously-accepted", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	active, err := model.AcceptMediaAttempt(source.ID, "previously-active", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.ClaimMediaAttempt(active.ID); err != nil {
		t.Fatal(err)
	}
	idx := newTestIndexer(t, cfg)
	t.Cleanup(idx.Close)
	waitMedia(t, queued.Generation, model.MediaInterrupted)
	waitMedia(t, active.Generation, model.MediaInterrupted)
	if idx.mediaQueue != nil {
		t.Fatal("disabled extractor started a downloader dispatcher")
	}
	var stored model.MediaAttempt
	if err := model.DB.First(&stored, "id = ?", queued.ID).Error; err != nil || stored.ErrorCode != "dispatcher_disabled" {
		t.Fatalf("disabled dispatch path was not explained safely: %#v, %v", stored, err)
	}
}

func TestMediaPartialMultiIndexBatchFailureDoesNotDispatch(t *testing.T) {
	idx, _ := mediaIndexer(t, mediaSuccessScript, &mediaContentExtractor{})
	idx.getOrCreate("en")
	idx.getOrCreate("fr")
	commits := 0
	for name, index := range idx.indexers {
		idx.indexers[name] = failingBlobCommitIndex{bleveIndex: index, fail: func() error {
			commits++
			if commits == 2 {
				return errors.New("injected second-index failure")
			}
			return nil
		}}
	}
	batch := idx.NewMultiBatch()
	for _, language := range []string{"en", "fr"} {
		if err := batch.Add(&document.Document{URL: mediaSourceURL + "&language=" + language, HTML: "<p>source</p>", LanguageOverride: language}); err != nil {
			batch.Discard()
			t.Fatal(err)
		}
	}
	if err := batch.Save(); err == nil || commits != 2 {
		t.Fatalf("did not exercise partial multi-index save: commits=%d, error=%v", commits, err)
	}
	var attempts []model.MediaAttempt
	if err := model.DB.Find(&attempts).Error; err != nil || len(attempts) != 0 {
		t.Fatalf("indexing created media attempts: %#v, %v", attempts, err)
	}
}

func TestMediaProcessedImportsAndSaveCannotManufactureOwnershipOrWork(t *testing.T) {
	idx, _ := mediaIndexer(t, mediaSuccessScript, &mediaContentExtractor{})
	for _, operation := range []string{"add", "batch", "save"} {
		doc := &document.Document{
			URL: mediaSourceURL, Text: "imported content", Processed: true,
			DocumentID: "client-controlled-id", Metadata: map[string]any{
				"yt_video_downloader_media": `{"video":["file:///foreign/video.webm"]}`,
				"yt_downloader_media":       `{"audio":["file:///foreign/audio.mp3"]}`,
				"unrelated":                 "retained",
			},
		}
		switch operation {
		case "add":
			if err := idx.Add(doc); err != nil {
				t.Fatal(err)
			}
		case "batch":
			batch := idx.NewMultiBatch()
			if err := batch.Add(doc); err != nil {
				t.Fatal(err)
			}
			if err := batch.Save(); err != nil {
				t.Fatal(err)
			}
		case "save":
			if err := idx.Save(doc); err != nil {
				t.Fatal(err)
			}
		}
		stored := idx.GetByDocID(mediaSourceURL)
		if stored == nil || stored.Metadata["unrelated"] != "retained" || stored.Metadata["yt_video_downloader_media"] != nil || stored.Metadata["yt_downloader_media"] != nil {
			t.Fatalf("%s retained authorization claims or lost unrelated metadata: %#v", operation, stored)
		}
		var count int64
		if err := model.DB.Model(&model.MediaAttempt{}).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("%s manufactured work: %d, %v", operation, count, err)
		}
	}
}
