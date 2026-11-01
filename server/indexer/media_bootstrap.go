// SPDX-License-Identifier: AGPL-3.0-or-later

package indexer

import (
	"context"
	"errors"

	"github.com/asciimoo/hister/server/document"
	yt "github.com/asciimoo/hister/server/extractor/extractors/yt_downloader"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"

	"gorm.io/gorm"
)

var (
	ErrMediaSourceBootstrapForbidden    = errors.New("media source bootstrap requires administrator authority")
	ErrMediaSourceBootstrapConfirmation = errors.New("media source bootstrap confirmation does not match current plan")
)

// CheckMediaSourceBootstrap verifies the exact committed index document and
// returns a read-only plan for creating a missing registry incarnation.
func (i *Indexer) CheckMediaSourceBootstrap(ctx context.Context, documentID, url string, userID uint, actor MediaActor) (*model.MediaSourceBootstrapCheck, error) {
	input, unlock, err := i.lockMediaSourceBootstrap(ctx, documentID, url, userID, actor)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return model.CheckMediaSourceBootstrap(input)
}

// ApplyMediaSourceBootstrap repeats the live indexed binding check under the
// source coordinator, then creates (or confirms) the exact checked incarnation.
func (i *Indexer) ApplyMediaSourceBootstrap(ctx context.Context, documentID, url string, userID uint, confirmation string, actor MediaActor) (*model.MediaSourceBootstrapResult, error) {
	input, unlock, err := i.lockMediaSourceBootstrap(ctx, documentID, url, userID, actor)
	if err != nil {
		return nil, err
	}
	defer unlock()
	result, err := model.ApplyMediaSourceBootstrap(input, confirmation)
	if errors.Is(err, model.ErrMediaSourceBootstrapConfirmation) {
		return nil, ErrMediaSourceBootstrapConfirmation
	}
	return result, err
}

func (i *Indexer) lockMediaSourceBootstrap(ctx context.Context, documentID, url string, userID uint, actor MediaActor) (model.MediaSourceBootstrapInput, func(), error) {
	if !actor.IsAdmin || (i.userHandling && actor.UserID == 0) {
		return model.MediaSourceBootstrapInput{}, nil, ErrMediaSourceBootstrapForbidden
	}
	if err := ctx.Err(); err != nil {
		return model.MediaSourceBootstrapInput{}, nil, err
	}
	unlock := i.lockMediaDocuments(documentID)
	i.mediaMu.Lock()
	closed, registry := i.mediaClosed, i.mediaRegistry
	i.mediaMu.Unlock()
	if closed || !registry {
		unlock()
		return model.MediaSourceBootstrapInput{}, nil, media.ErrDispatcherUnavailable
	}
	doc := i.GetByDocID(documentID)
	if doc == nil || doc.ID() != documentID || doc.URL != url || doc.UserID != userID ||
		doc.ID() != document.GetDocID(userID, url) || !(&yt.YTDownloader{}).Match(doc) {
		unlock()
		return model.MediaSourceBootstrapInput{}, nil, gorm.ErrRecordNotFound
	}
	if !i.userHandling && userID != 0 {
		unlock()
		return model.MediaSourceBootstrapInput{}, nil, gorm.ErrRecordNotFound
	}
	if err := ctx.Err(); err != nil {
		unlock()
		return model.MediaSourceBootstrapInput{}, nil, err
	}
	return model.MediaSourceBootstrapInput{DocumentID: doc.ID(), UserID: doc.UserID, URL: doc.URL}, unlock, nil
}
