// SPDX-License-Identifier: AGPL-3.0-or-later

package indexer

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/asciimoo/hister/server/document"
	yt "github.com/asciimoo/hister/server/extractor/extractors/yt_downloader"
	"github.com/asciimoo/hister/server/media"
	"github.com/asciimoo/hister/server/model"

	"gorm.io/gorm"
)

var (
	ErrMediaDisabled      = errors.New("media downloader is disabled")
	ErrMediaSourceSkipped = errors.New("media source is excluded by current URL rules")
)

// MediaActor comes from the authenticated server context, not the request body
// or submission ownership headers. Admin mutation scope matches update/delete;
// shared-document read access alone is never mutation authority.
type MediaActor struct {
	UserID  uint
	IsAdmin bool
}

// Caller holds the actual source commit coordinator, so delete/re-add and
// ownership moves cannot race this authorization check and command acceptance.
func (i *Indexer) mediaCommandDocument(id string, actor MediaActor) (*document.Document, error) {
	if !i.mediaRegistry {
		return nil, media.ErrDispatcherUnavailable
	}
	doc := i.GetByDocID(id)
	if doc == nil || doc.ID() != id || !(&yt.YTDownloader{}).Match(doc) {
		return nil, gorm.ErrRecordNotFound
	}
	if i.userHandling {
		if actor.UserID == 0 || (doc.UserID != actor.UserID && !actor.IsAdmin) {
			return nil, gorm.ErrRecordNotFound
		}
	} else if doc.UserID != 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return doc, nil
}

func (i *Indexer) RetryMedia(ctx context.Context, documentID, attemptID string, actor MediaActor, options ...AddOption) (*model.MediaRetryResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	unlock := i.lockMediaDocuments(documentID)
	defer unlock()
	doc, err := i.mediaCommandDocument(documentID, actor)
	if err != nil {
		return nil, err
	}
	scope := model.MediaScope{DocumentID: doc.ID(), UserID: doc.UserID}
	if _, err := model.MediaAttemptInScope(scope, attemptID); err != nil {
		return nil, err
	}
	if !doc.IgnoreSkipRules() && newAddOptions(options).rules.IsSkip(doc.URL) {
		return nil, ErrMediaSourceSkipped
	}
	if i.mediaRetryConfig == nil {
		return nil, ErrMediaDisabled
	}
	e := &yt.YTDownloader{}
	if err := e.SetConfig(i.mediaRetryConfig.Clone()); err != nil {
		return nil, err
	}
	request, err := e.MediaRequest(doc)
	if err != nil {
		return nil, model.ErrMediaConflict
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	i.mediaMu.Lock()
	queue, closed := i.mediaQueue, i.mediaClosed
	i.mediaMu.Unlock()
	if closed || queue == nil {
		return nil, media.ErrDispatcherUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return queue.Retry(scope, attemptID, string(encoded))
}

func (i *Indexer) CancelMedia(ctx context.Context, documentID, attemptID string, actor MediaActor) (*model.MediaAttempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	unlock := i.lockMediaDocuments(documentID)
	defer unlock()
	doc, err := i.mediaCommandDocument(documentID, actor)
	if err != nil {
		return nil, err
	}
	i.mediaMu.Lock()
	queue, closed := i.mediaQueue, i.mediaClosed
	i.mediaMu.Unlock()
	if closed {
		return nil, media.ErrDispatcherUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope := model.MediaScope{DocumentID: doc.ID(), UserID: doc.UserID}
	if queue == nil {
		return model.CancelScopedMediaAttempt(scope, attemptID)
	}
	return queue.CancelScoped(scope, attemptID)
}
