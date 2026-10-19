// Package clock은 지금 시각과 기다리기를 감싼다. 테스트는 가짜 시계로 시간을 바로 흘려보낸다.
package clock

import (
	"context"
	"sync"
	"time"
)

type Clock interface {
	Now() time.Time
	// Sleep은 d만큼 기다린다. ctx가 끝나면 그 오류를 돌려준다
	Sleep(ctx context.Context, d time.Duration) error
}

// Real은 실제 시계다.
type Real struct{}

func (Real) Now() time.Time { return time.Now().UTC() }

func (Real) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Fake는 Sleep을 부르면 그만큼 시각을 바로 옮기는 시계다.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

func NewFake(t time.Time) *Fake { return &Fake{now: t.UTC()} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
	return nil
}
