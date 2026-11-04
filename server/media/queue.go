// SPDX-License-Identifier: AGPL-3.0-or-later

package media

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/asciimoo/hister/server/model"

	"github.com/rs/zerolog/log"
)

type ModeEvents struct {
	Prepared  func(role, jobID string) error
	Started   func(role string) error
	Completed func(role string) error
}

type (
	Runner          func(context.Context, Request, ModeEvents) ([]model.MediaArtifactInput, error)
	SourceValidator func(*model.MediaSource, Request) bool
)

type activeAttempt struct {
	sourceID   string
	claimToken string
	cancel     context.CancelFunc
}

// Queue owns a bounded set of workers. Downloads use its lifetime, never an
// indexing/preview request's context. SQL is the work set; wakeups are only hints.
// One live queue owns a model database, as with Hister's embedding queue.
type Queue struct {
	ctx      context.Context
	cancel   context.CancelFunc
	runner   Runner
	validate SourceValidator
	workers  int
	wake     chan struct{}
	wg       sync.WaitGroup
	mu       sync.Mutex
	closed   bool
	active   map[string]activeAttempt
	lock     *model.MediaQueueLock
}

func NewQueue(workers int, runner Runner, validate SourceValidator) (*Queue, error) {
	if workers < 1 || runner == nil || validate == nil {
		return nil, errors.New("media queue requires workers, runner and source validation")
	}
	lock, err := model.AcquireMediaQueueLock()
	if err != nil {
		return nil, err
	}
	if err := lock.InterruptMediaAttempts(); err != nil {
		_ = lock.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &Queue{
		ctx: ctx, cancel: cancel, workers: workers, runner: runner, validate: validate,
		wake: make(chan struct{}, workers), active: make(map[string]activeAttempt), lock: lock,
	}
	for range workers {
		q.wg.Go(q.work)
	}
	q.wg.Go(q.watchLock)
	return q, nil
}

// Accept must be called under the source commit coordinator, after Bleve Save.
func (q *Queue) Accept(intent *Intent) (*model.MediaAttempt, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil, ErrDispatcherUnavailable
	}
	attempt, err := model.AcceptReservedMediaAttempt(intent.AttemptID)
	if err != nil {
		return nil, err
	}
	q.notify()
	return attempt, nil
}

func (q *Queue) work() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for q.ctx.Err() == nil {
		pending, err := model.NextQueuedMediaAttempt()
		if err == nil && pending != nil {
			claimed, claimErr := q.lock.ClaimMediaAttempt(pending.ID)
			if claimErr == nil {
				q.run(claimed)
				continue
			}
			if errors.Is(claimErr, model.ErrMediaConflict) || errors.Is(claimErr, model.ErrMediaSourceRevoked) {
				continue
			}
			err = claimErr
		}
		if err != nil {
			log.Error().Err(err).Msg("media queue database operation failed")
		}
		select {
		case <-q.ctx.Done():
			return
		case <-q.wake:
		case <-ticker.C:
		}
	}
}

func (q *Queue) run(attempt *model.MediaAttempt) {
	ctx, cancel := context.WithCancel(q.ctx)
	q.mu.Lock()
	q.active[attempt.ID] = activeAttempt{sourceID: attempt.SourceID, claimToken: attempt.ClaimToken, cancel: cancel}
	q.mu.Unlock()
	defer func() {
		cancel()
		q.mu.Lock()
		delete(q.active, attempt.ID)
		q.mu.Unlock()
	}()
	finishError := func(err error, code string) {
		if ctx.Err() != nil {
			_ = model.InterruptClaimedMediaAttempt(attempt.ID, attempt.ClaimToken)
			return
		}
		if failure := model.FailMediaAttempt(attempt.ID, attempt.ClaimToken, code); failure != nil &&
			!errors.Is(failure, model.ErrMediaConflict) && !errors.Is(failure, model.ErrMediaSourceRevoked) {
			log.Error().Err(failure).Str("attempt_id", attempt.ID).Msg("failed to record media failure")
		}
		log.Warn().Err(err).Str("attempt_id", attempt.ID).Msg("media attempt did not publish")
	}
	var request Request
	if err := json.Unmarshal([]byte(attempt.RequestJSON), &request); err != nil {
		finishError(err, "validation_failed")
		return
	}
	source, err := model.GetMediaSourceByID(attempt.SourceID)
	if err != nil || !q.validate(source, request) {
		if err == nil {
			err = errors.New("media source no longer matches the committed document")
		}
		finishError(err, "validation_failed")
		return
	}
	events := ModeEvents{
		Prepared: func(role, jobID string) error {
			return model.SetMediaModeJob(attempt.ID, attempt.ClaimToken, role, jobID)
		},
		Started:   func(role string) error { return model.StartMediaMode(attempt.ID, attempt.ClaimToken, role) },
		Completed: func(role string) error { return model.CompleteMediaMode(attempt.ID, attempt.ClaimToken, role) },
	}
	pair, err := q.runner(ctx, request, events)
	if err != nil {
		code := "download_failed"
		if errors.Is(err, ErrInvalidOutput) {
			code = "validation_failed"
		}
		finishError(err, code)
		return
	}
	for _, artifact := range pair {
		file, _, err := OpenArtifact(artifact)
		if err != nil {
			finishError(err, "validation_failed")
			return
		}
		if err := file.Close(); err != nil {
			finishError(err, "validation_failed")
			return
		}
	}
	if err := ctx.Err(); err != nil {
		finishError(err, "download_failed")
		return
	}
	if _, err := model.PublishMediaPair(attempt.ID, attempt.ClaimToken, pair); err != nil {
		finishError(err, "publication_failed")
	}
}

// CancelSource follows registry revocation/transfer, which fences publication
// even if process cancellation races a producer finishing.
func (q *Queue) CancelSource(sourceID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, active := range q.active {
		if active.sourceID == sourceID {
			active.cancel()
		}
	}
}

func (q *Queue) CancelAttempt(id string) error {
	if err := model.CancelMediaAttempt(id); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if active, ok := q.active[id]; ok {
		active.cancel()
	}
	return nil
}

func (q *Queue) watchLock() {
	select {
	case <-q.ctx.Done():
	case err := <-q.lock.Lost():
		q.mu.Lock()
		q.closed = true
		q.cancel()
		q.mu.Unlock()
		log.Error().Err(err).Msg("media queue lease lost; workers are stopping")
	}
}

func (q *Queue) Close() {
	q.mu.Lock()
	q.closed = true
	// Fence publication in SQL before cancelling process contexts. A worker
	// that already passed its last context check cannot publish afterwards.
	for id, active := range q.active {
		if err := model.InterruptClaimedMediaAttempt(id, active.claimToken); err != nil &&
			!errors.Is(err, model.ErrMediaConflict) && !errors.Is(err, model.ErrMediaSourceRevoked) {
			log.Error().Err(err).Str("attempt_id", id).Msg("could not fence media worker during shutdown")
		}
	}
	q.cancel()
	q.mu.Unlock()
	q.wg.Wait()
	if err := q.lock.Close(); err != nil {
		log.Error().Err(err).Msg("could not release media queue lock")
	}
}
