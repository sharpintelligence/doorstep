package notify

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"time"
)

const (
	defaultAttempts       = 4
	defaultBaseDelay      = time.Second
	defaultMaxDelay       = 30 * time.Second
	defaultAttemptTimeout = 10 * time.Second
	defaultJitter         = 0.2
)

type Policy struct {
	Attempts       int
	BaseDelay      time.Duration
	MaxDelay       time.Duration
	AttemptTimeout time.Duration
	Jitter         float64
	Sleep          func(ctx context.Context, d time.Duration) error
	Random         func() float64
}

func DefaultPolicy() Policy {
	return Policy{
		Attempts:       defaultAttempts,
		BaseDelay:      defaultBaseDelay,
		MaxDelay:       defaultMaxDelay,
		AttemptTimeout: defaultAttemptTimeout,
		Jitter:         defaultJitter,
		Sleep:          sleep,
		Random:         randomFraction,
	}
}

func SingleAttempt() Policy {
	p := DefaultPolicy()
	p.Attempts = 1
	return p
}

func (p Policy) withDefaults() Policy {
	d := DefaultPolicy()
	if p.Attempts <= 0 {
		p.Attempts = d.Attempts
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = d.BaseDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = d.MaxDelay
	}
	if p.AttemptTimeout <= 0 {
		p.AttemptTimeout = d.AttemptTimeout
	}
	if p.Jitter <= 0 {
		p.Jitter = d.Jitter
	}
	if p.Sleep == nil {
		p.Sleep = d.Sleep
	}
	if p.Random == nil {
		p.Random = d.Random
	}
	return p
}

func (p Policy) Do(ctx context.Context, delivery Delivery) (int, error) {
	p = p.withDefaults()
	for attempt := 1; ; attempt++ {
		err := p.try(ctx, delivery)
		if err == nil {
			return attempt, nil
		}
		if attempt >= p.Attempts || !retryable(err) || ctx.Err() != nil {
			return attempt, err
		}
		if p.Sleep(ctx, p.delay(attempt, err)) != nil {
			return attempt, err
		}
	}
}

func (p Policy) try(ctx context.Context, delivery Delivery) error {
	ctx, cancel := context.WithTimeout(ctx, p.AttemptTimeout)
	defer cancel()
	return delivery(ctx)
}

func (p Policy) delay(attempt int, err error) time.Duration {
	if after, ok := retryAfter(err); ok {
		return min(after, p.MaxDelay)
	}
	backoff := p.BaseDelay << (attempt - 1)
	factor := 1 + p.Jitter*(2*p.Random()-1)
	return min(time.Duration(float64(backoff)*factor), p.MaxDelay)
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func randomFraction() float64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return float64(binary.BigEndian.Uint64(b[:])>>11) / (1 << 53)
}
