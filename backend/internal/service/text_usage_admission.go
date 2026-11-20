package service

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/google/uuid"
)

type textUsageAdmissionStore interface {
	AdmitTextUsage(ctx context.Context, userID int64, ticket string, minimum float64, ttl time.Duration) error
	RenewTextUsage(ctx context.Context, ticket string, ttl time.Duration) (bool, error)
	ReleaseTextUsage(ctx context.Context, ticket string) error
}

type textUsageAdmissionKey struct{}

type TextUsageAdmission struct {
	ticket string
	users  textUsageAdmissionStore
	refs   atomic.Int64
	cancel context.CancelFunc
}

// AdmitTextUsage attaches the database refund guard independently of optional cost estimation.
func (s *BillingCacheService) AdmitTextUsage(ctx context.Context, userID int64) (context.Context, func(), error) {
	store, ok := s.userRepo.(textUsageAdmissionStore)
	if !ok || (s.cfg != nil && s.cfg.RunMode == config.RunModeSimple) || TextUsageAdmissionFromContext(ctx) != nil {
		return ctx, noopRelease, nil
	}
	const ttl = 5 * time.Minute
	ticket := uuid.NewString()
	if err := store.AdmitTextUsage(ctx, userID, ticket, s.minimumBalanceReserve(), ttl); err != nil {
		return ctx, noopRelease, err
	}
	leaseCtx, cancel := context.WithCancel(context.Background())
	a := &TextUsageAdmission{ticket: ticket, users: store, cancel: cancel}
	a.refs.Store(1)
	go a.renew(leaseCtx, ttl)
	return a.Bind(ctx), sync.OnceFunc(a.release), nil
}

func (a *TextUsageAdmission) release() {
	if a.refs.Add(-1) != 0 {
		return
	}
	a.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.users.ReleaseTextUsage(ctx, a.ticket); err != nil {
		slog.Warn("release text usage admission", "error", err)
	}
}

func (a *TextUsageAdmission) renew(ctx context.Context, ttl time.Duration) {
	ticker := time.NewTicker(ttl / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			op, cancel := context.WithTimeout(ctx, 5*time.Second)
			ok, err := a.users.RenewTextUsage(op, a.ticket, ttl)
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.Warn("renew text usage admission", "error", err)
			} else if !ok {
				return
			}
		}
	}
}

func TextUsageAdmissionFromContext(ctx context.Context) *TextUsageAdmission {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(textUsageAdmissionKey{}).(*TextUsageAdmission)
	return a
}

// HoldTask transfers one reference before the handler can release its own.
func (a *TextUsageAdmission) HoldTask() func() {
	if a == nil {
		return noopRelease
	}
	a.refs.Add(1)
	return sync.OnceFunc(a.release)
}

func (a *TextUsageAdmission) Bind(ctx context.Context) context.Context {
	if a == nil {
		return ctx
	}
	return context.WithValue(ctx, textUsageAdmissionKey{}, a)
}
