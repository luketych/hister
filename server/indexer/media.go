// SPDX-License-Identifier: AGPL-3.0-or-later

package indexer

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"

	"github.com/asciimoo/hister/config"
	"github.com/asciimoo/hister/server/document"
	yt "github.com/asciimoo/hister/server/extractor/extractors/yt_downloader"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"

	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
)

// The coordinator covers short actual commit/lifecycle operations, never
// extraction or downloads. Sorted stripe acquisition also handles multi-source
// batches and owner moves without lock-order deadlocks.
func (i *Indexer) lockMediaDocuments(ids ...string) func() {
	stripes := make([]int, 0, len(ids))
	for _, id := range ids {
		hash := fnv.New32a()
		_, _ = hash.Write([]byte(id))
		stripes = append(stripes, int(hash.Sum32()%uint32(len(i.mediaCommits))))
	}
	slices.Sort(stripes)
	stripes = slices.Compact(stripes)
	for _, stripe := range stripes {
		i.mediaCommits[stripe].Lock()
	}
	return func() {
		for _, stripe := range slices.Backward(stripes) {
			i.mediaCommits[stripe].Unlock()
		}
	}
}

func (i *Indexer) initMedia(cfg *config.Config) error {
	cfgs, err := config.NormalizeExtractorConfigs(cfg.Extractors)
	if err != nil {
		return err
	}
	i.mediaRegistry = model.DatabaseMatches(cfg)
	if configured := cfgs[config.YTDownloaderName]; configured != nil && configured.Enabled(false) {
		e := &yt.YTDownloader{}
		if err := e.SetConfig(configured); err != nil {
			return err
		}
		i.mediaRetryConfig = e.GetConfig().Clone()
		_, err := i.ensureMediaQueue(e.MediaWorkers())
		return err
	}
	if i.mediaRegistry {
		lock, err := model.AcquireMediaQueueLock()
		if errors.Is(err, model.ErrMediaQueueAlreadyRunning) {
			log.Debug().Msg("media queue already active; skipped startup recovery")
			return nil
		}
		if err != nil {
			return err
		}
		defer func() {
			if err := lock.Close(); err != nil {
				log.Error().Err(err).Msg("could not release media queue startup lock")
			}
		}()
		if err := lock.InterruptMediaAttempts(); err != nil {
			return err
		}
		return lock.InterruptQueuedMediaAttempts()
	}
	return nil
}

func (i *Indexer) ensureMediaQueue(workers int) (*media.Queue, error) {
	i.mediaMu.Lock()
	defer i.mediaMu.Unlock()
	if i.mediaClosed || !i.mediaRegistry {
		return nil, errors.New("media dispatcher is unavailable")
	}
	if i.mediaQueue != nil {
		return i.mediaQueue, nil
	}
	queue, err := media.NewQueue(workers, yt.RunMediaAttempt, func(source *model.MediaSource, request media.Request) bool {
		if source == nil || source.Suspended || source.UserID != request.UserID || source.DocumentID != document.GetDocID(request.UserID, request.URL) {
			return false
		}
		unlock := i.lockMediaDocuments(source.DocumentID)
		defer unlock()
		doc := i.GetByDocID(source.DocumentID)
		return doc != nil && doc.ID() == source.DocumentID && doc.UserID == source.UserID
	})
	if err != nil {
		return nil, err
	}
	i.mediaQueue = queue
	return queue, nil
}

func (i *Indexer) stopMediaQueue() {
	i.mediaMu.Lock()
	i.mediaClosed = true
	queue := i.mediaQueue
	i.mediaMu.Unlock()
	if queue != nil {
		queue.Close()
	}
}

func (i *Indexer) stageMedia(d *document.Document, owner uint, intent **media.Intent) media.Stager {
	return func(request media.Request) error {
		if *intent != nil || request.URL != d.URL || d.UserID != owner {
			return errors.New("media scheduling identity changed or was already staged")
		}
		if _, err := i.ensureMediaQueue(2); err != nil {
			return err
		}
		request.UserID = owner
		encoded, err := json.Marshal(request)
		if err != nil {
			return err
		}
		id := document.GetDocID(owner, d.URL)
		unlock := i.lockMediaDocuments(id)
		defer unlock()
		source, err := model.EnsureMediaSource(id, owner)
		if err != nil {
			return err
		}
		attempt, err := model.ReserveMediaAttempt(source.ID, rand.Text(), string(encoded))
		if err != nil {
			return err
		}
		*intent = &media.Intent{AttemptID: attempt.ID, SourceID: source.ID, DocumentID: id, UserID: owner}
		return nil
	}
}

func abandonMedia(intent *media.Intent) {
	if intent == nil {
		return
	}
	if err := model.AbandonMediaReservation(intent.AttemptID); err != nil && !errors.Is(err, model.ErrMediaSourceRevoked) {
		log.Error().Err(err).Str("attempt_id", intent.AttemptID).Msg("could not abandon uncommitted media intent")
	}
}

func validateMediaIntent(intent *media.Intent) error {
	if intent == nil {
		return nil
	}
	source, err := model.GetMediaSourceByID(intent.SourceID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.ErrMediaSourceRevoked
	}
	if err != nil {
		return err
	}
	if source.Suspended || source.DocumentID != intent.DocumentID || source.UserID != intent.UserID {
		return model.ErrMediaConflict
	}
	return nil
}

func (i *Indexer) acceptMedia(intent *media.Intent) error {
	if intent == nil {
		return nil
	}
	i.mediaMu.Lock()
	queue := i.mediaQueue
	i.mediaMu.Unlock()
	if queue == nil {
		return errors.New("media dispatcher is unavailable after index commit")
	}
	if _, err := queue.Accept(intent); err != nil {
		return fmt.Errorf("source indexed but media queue acceptance failed: %w", err)
	}
	return nil
}

type mediaMove struct {
	SourceID   string
	DocumentID string
	UserID     uint
}

func (b *MultiBatch) stageMediaMove(originalID, documentID string, userID uint) error {
	if !b.indexer.mediaRegistry {
		return nil
	}
	source, err := model.GetMediaSource(originalID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	b.mediaMoves[originalID] = mediaMove{SourceID: source.ID, DocumentID: documentID, UserID: userID}
	return nil
}

func (b *MultiBatch) prepareMediaMoves() error {
	for originalID, move := range b.mediaMoves {
		source, err := model.GetMediaSourceByID(move.SourceID)
		if err != nil {
			return err
		}
		if source.DocumentID != originalID || b.indexer.getStoredDocumentState(move.DocumentID).found {
			return model.ErrMediaConflict
		}
		if err := model.SuspendMediaTransfer(move.SourceID, move.DocumentID, move.UserID); err != nil {
			return err
		}
		b.indexer.mediaMu.Lock()
		queue := b.indexer.mediaQueue
		b.indexer.mediaMu.Unlock()
		if queue != nil {
			queue.CancelSource(move.SourceID)
		}
	}
	return nil
}

func (b *MultiBatch) finishMediaMoves() error {
	for _, move := range b.mediaMoves {
		if err := model.TransferMediaSource(move.SourceID, move.DocumentID, move.UserID); err != nil {
			// The durable pre-write suspension remains in effect on failure.
			return fmt.Errorf("source indexed but media ownership finalization failed: %w", err)
		}
	}
	return nil
}

// Revoke before deletion so partial Bleve failure cannot leave a live binding
// that a later re-add silently revives. Files and history remain untouched.
func (i *Indexer) revokeMedia(id string) error {
	if !i.mediaRegistry {
		return nil
	}
	source, err := model.GetMediaSource(id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := model.RevokeMediaSource(source.ID); err != nil {
		return err
	}
	i.mediaMu.Lock()
	queue := i.mediaQueue
	i.mediaMu.Unlock()
	if queue != nil {
		queue.CancelSource(source.ID)
	}
	return nil
}
