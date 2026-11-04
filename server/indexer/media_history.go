// SPDX-License-Identifier: AGPL-3.0-or-later

package indexer

import (
	"context"
	"errors"

	yt "github.com/asciimoo/hister/server/extractor/extractors/yt_downloader"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"

	"gorm.io/gorm"
)

var (
	ErrMediaHistoryForbidden    = errors.New("historical media registration requires an authorized administrator")
	ErrMediaHistoryConfirmation = errors.New("historical media confirmation does not match current plan")
)

// ImportHistoricalMedia is INTERNAL administrative infrastructure, deliberately
// not registered as an HTTP handler or CLI apply mode. The actor comes from
// verified server/local authority, never the approval JSON. It only registers
// retained history: no source creation, re-index, selection change or download.
func (i *Indexer) CheckHistoricalMedia(ctx context.Context, stateDir, mediaRoot string, approval *yt.HistoryApproval, actor MediaActor) (*model.MediaHistoryCheck, error) {
	prepared, input, unlock, err := i.prepareHistoricalMedia(ctx, stateDir, mediaRoot, approval, actor)
	if err != nil {
		return nil, err
	}
	defer func() { _ = prepared.Close() }()
	defer unlock()
	if err := i.checkMediaHistoryBinding(input, prepared.SourceURL(), actor); err != nil {
		return nil, err
	}
	if err := prepared.Recheck(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return model.CheckMediaHistory(input)
}

func (i *Indexer) ImportHistoricalMedia(ctx context.Context, stateDir, mediaRoot string, approval *yt.HistoryApproval, actor MediaActor) (*model.MediaHistoryResult, error) {
	return i.applyHistoricalMedia(ctx, stateDir, mediaRoot, approval, "", actor, false)
}

func (i *Indexer) ApplyHistoricalMedia(ctx context.Context, stateDir, mediaRoot string, approval *yt.HistoryApproval, confirmation string, actor MediaActor) (*model.MediaHistoryResult, error) {
	return i.applyHistoricalMedia(ctx, stateDir, mediaRoot, approval, confirmation, actor, true)
}

func (i *Indexer) applyHistoricalMedia(ctx context.Context, stateDir, mediaRoot string, approval *yt.HistoryApproval, confirmation string, actor MediaActor, guarded bool) (*model.MediaHistoryResult, error) {
	prepared, input, unlock, err := i.prepareHistoricalMedia(ctx, stateDir, mediaRoot, approval, actor)
	if err != nil {
		return nil, err
	}
	defer func() { _ = prepared.Close() }()
	defer unlock()
	if err := i.checkMediaHistoryBinding(input, prepared.SourceURL(), actor); err != nil {
		return nil, err
	}
	if err := prepared.Recheck(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if guarded {
		result, err := model.ApplyMediaHistory(input, confirmation)
		if errors.Is(err, model.ErrMediaHistoryConfirmation) {
			return nil, ErrMediaHistoryConfirmation
		}
		return result, err
	}
	return model.ImportMediaHistory(input)
}

func (i *Indexer) prepareHistoricalMedia(ctx context.Context, stateDir, mediaRoot string, approval *yt.HistoryApproval, actor MediaActor) (*yt.PreparedHistoryApproval, model.MediaHistoryInput, func(), error) {
	if !actor.IsAdmin || (i.userHandling && actor.UserID == 0) {
		return nil, model.MediaHistoryInput{}, nil, ErrMediaHistoryForbidden
	}
	if err := ctx.Err(); err != nil {
		return nil, model.MediaHistoryInput{}, nil, err
	}
	if err := approval.Validate(); err != nil {
		return nil, model.MediaHistoryInput{}, nil, err
	}
	// Check live scope before touching retained evidence. Hashing can be slow;
	// release the short source coordinator and recheck everything at commit.
	binding := model.MediaHistoryInput{
		SourceID: approval.Source.SourceID, DocumentID: approval.Source.DocumentID,
		UserID: *approval.Source.OwnerID,
	}
	unlock := i.lockMediaDocuments(binding.DocumentID)
	err := i.checkMediaHistoryBinding(binding, approval.Source.URL, actor)
	unlock()
	if err != nil {
		return nil, model.MediaHistoryInput{}, nil, err
	}
	prepared, err := yt.PrepareHistoryApproval(ctx, stateDir, mediaRoot, approval)
	if err != nil {
		return nil, model.MediaHistoryInput{}, nil, err
	}
	input := prepared.Input(actor.UserID)
	unlock = i.lockMediaDocuments(input.DocumentID)
	return prepared, input, unlock, nil
}

// Caller holds the source commit coordinator. SQL rechecks the exact live
// incarnation/owner under its own transaction before creating/replaying rows.
func (i *Indexer) checkMediaHistoryBinding(input model.MediaHistoryInput, url string, actor MediaActor) error {
	i.mediaMu.Lock()
	closed := i.mediaClosed
	i.mediaMu.Unlock()
	if closed {
		return media.ErrDispatcherUnavailable
	}
	doc, err := i.mediaCommandDocument(input.DocumentID, actor)
	if err != nil {
		return err
	}
	if doc.URL != url || doc.UserID != input.UserID {
		return model.ErrMediaConflict
	}
	source, err := model.GetMediaSourceByID(input.SourceID)
	if err != nil {
		return err
	}
	if source.RevokedAt != nil {
		return gorm.ErrRecordNotFound
	}
	if source.Suspended || source.DocumentID != input.DocumentID || source.UserID != input.UserID {
		return model.ErrMediaConflict
	}
	return nil
}
