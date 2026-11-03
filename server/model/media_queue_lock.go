// SPDX-License-Identifier: AGPL-3.0-or-later

package model

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	mediaQueueLeaseName    = "dispatcher"
	mediaQueueLeaseTTL     = 30 * time.Second
	mediaQueueLeaseRenewal = 5 * time.Second
)

var (
	ErrMediaQueueAlreadyRunning = errors.New("media queue is already owned by another process")
	ErrMediaQueueLockRequired   = errors.New("media queue lock is required")
	ErrMediaQueueLockLost       = errors.New("media queue lock was lost")
)

// MediaQueueLock is a renewable database lease held for the lifetime of the
// media dispatcher. A fence prevents an expired owner from releasing or
// renewing a successor's lease. Process crashes are recovered after the TTL.
type MediaQueueLock struct {
	mu        sync.Mutex
	owner     string
	fence     uint64
	held      bool
	lost      chan error
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// AcquireMediaQueueLock excludes another dispatcher or startup recovery for
// the same database. It must be held until all workers have stopped.
func AcquireMediaQueueLock() (*MediaQueueLock, error) {
	db, err := mediaDB()
	if err != nil {
		return nil, err
	}
	owner := rand.Text()
	var lease MediaQueueLease
	err = db.Transaction(func(tx *gorm.DB) error {
		seed := MediaQueueLease{
			Name: mediaQueueLeaseName, ExpiresAt: time.Unix(0, 0).UTC(),
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
			return err
		}
		result := tx.Model(&MediaQueueLease{}).
			Where("name = ? AND "+expiredMediaQueueLeasePredicate(tx), mediaQueueLeaseName).
			Updates(map[string]any{
				"owner_token": owner,
				"fence":       gorm.Expr("fence + 1"),
				"expires_at":  mediaQueueLeaseExpiry(tx),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrMediaQueueAlreadyRunning
		}
		return tx.First(&lease, "name = ?", mediaQueueLeaseName).Error
	})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	lock := &MediaQueueLock{
		owner: owner, fence: lease.Fence, held: true,
		lost: make(chan error, 1), cancel: cancel, done: make(chan struct{}),
	}
	go lock.renewLoop(ctx)
	return lock, nil
}

// Lost reports lease renewal failures. The dispatcher must stop accepting and
// cancel workers when this channel receives an error.
func (l *MediaQueueLock) Lost() <-chan error {
	if l == nil {
		return nil
	}
	return l.lost
}

// Close stops renewal and conditionally releases this owner's lease. It is
// safe to call more than once; a stale owner cannot clear a successor's lease.
func (l *MediaQueueLock) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.held = false
		l.cancel()
		l.mu.Unlock()
		<-l.done

		db, err := mediaDB()
		if err != nil {
			l.closeErr = err
			return
		}
		l.closeErr = db.Model(&MediaQueueLease{}).
			Where("name = ? AND owner_token = ? AND fence = ?", mediaQueueLeaseName, l.owner, l.fence).
			Updates(map[string]any{"owner_token": "", "expires_at": time.Unix(0, 0).UTC()}).Error
	})
	return l.closeErr
}

func (l *MediaQueueLock) ClaimMediaAttempt(id string) (*MediaAttempt, error) {
	var claimed MediaAttempt
	err := l.withLeaseTransaction(func(tx *gorm.DB) error {
		return withMediaAttemptLockedTx(tx, id, func(tx *gorm.DB, _ *MediaSource, attempt *MediaAttempt) error {
			return claimMediaAttempt(tx, attempt, &claimed)
		})
	})
	if err != nil {
		return nil, err
	}
	return &claimed, nil
}

func (l *MediaQueueLock) withLeaseTransaction(operation func(*gorm.DB) error) error {
	if l == nil {
		return ErrMediaQueueLockRequired
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.held {
		return ErrMediaQueueLockRequired
	}
	db, err := mediaDB()
	if err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		// Updating the lease row both verifies ownership using database time and
		// serializes recovery against another owner's takeover transaction.
		result := tx.Model(&MediaQueueLease{}).
			Where("name = ? AND owner_token = ? AND fence = ? AND "+activeMediaQueueLeasePredicate(tx), mediaQueueLeaseName, l.owner, l.fence).
			Update("expires_at", mediaQueueLeaseExpiry(tx))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrMediaQueueLockLost
		}
		return operation(tx)
	})
}

func (l *MediaQueueLock) renewLoop(ctx context.Context) {
	defer close(l.done)
	ticker := time.NewTicker(mediaQueueLeaseRenewal)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := l.withLeaseTransaction(func(*gorm.DB) error { return nil }); err != nil {
				select {
				case l.lost <- fmt.Errorf("%w: %v", ErrMediaQueueLockLost, err):
				default:
				}
				return
			}
		}
	}
}

func expiredMediaQueueLeasePredicate(db *gorm.DB) string {
	if db.Name() == "sqlite" {
		return "julianday(expires_at) <= julianday('now')"
	}
	return "expires_at <= CURRENT_TIMESTAMP"
}

func activeMediaQueueLeasePredicate(db *gorm.DB) string {
	if db.Name() == "sqlite" {
		return "julianday(expires_at) > julianday('now')"
	}
	return "expires_at > CURRENT_TIMESTAMP"
}

func mediaQueueLeaseExpiry(db *gorm.DB) clause.Expr {
	seconds := int(mediaQueueLeaseTTL / time.Second)
	if db.Name() == "sqlite" {
		return gorm.Expr("datetime('now', ?)", fmt.Sprintf("+%d seconds", seconds))
	}
	return gorm.Expr(fmt.Sprintf("CURRENT_TIMESTAMP + INTERVAL '%d seconds'", seconds))
}
