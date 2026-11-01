// SPDX-License-Identifier: AGPL-3.0-or-later

package media

import (
	"errors"

	"github.com/asciimoo/hister/server/model"
)

var ErrDispatcherUnavailable = errors.New("media dispatcher is unavailable")

func (q *Queue) notify() {
	for range q.workers {
		select {
		case q.wake <- struct{}{}:
		default:
		}
	}
}

// Retry requires a previously authorized committed source and the indexer's
// source coordinator. Acceptance and its idempotency receipt commit together.
func (q *Queue) Retry(scope model.MediaScope, id, requestJSON string) (*model.MediaRetryResult, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil, ErrDispatcherUnavailable
	}
	result, err := model.RetryMediaAttempt(scope, id, requestJSON)
	if err != nil {
		return nil, err
	}
	if !result.Replayed {
		q.notify()
	}
	return result, nil
}

func (q *Queue) CancelScoped(scope model.MediaScope, id string) (*model.MediaAttempt, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil, ErrDispatcherUnavailable
	}
	attempt, err := model.CancelScopedMediaAttempt(scope, id)
	if err != nil {
		return nil, err
	}
	// SQL cancellation fences publication before signalling the process tree.
	if active, ok := q.active[id]; ok {
		active.cancel()
	}
	return attempt, nil
}
