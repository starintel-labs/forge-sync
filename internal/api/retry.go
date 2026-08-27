package api

import (
	"context"
	"errors"
	"math"
	"time"
)

// TransientError is implemented by API client errors that may succeed when
// retried (rate limits and server-side failures).
type TransientError interface {
	error
	Transient() bool
	RetryDelay() time.Duration
}

// RetryPolicy bounds retry attempts and delays for transient API failures.
// Delays are deterministic: exponential growth from BaseDelay capped at
// MaxDelay, always overridden by a longer server-provided Retry-After.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Sleep       func(context.Context, time.Duration) error
}

// DefaultRetryPolicy returns the production retry policy.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 4, BaseDelay: time.Second, MaxDelay: 30 * time.Second, Sleep: Sleep}
}

// DisabledRetryPolicy performs exactly one attempt.
func DisabledRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 1}
}

// Sleep waits for the given duration or until the context is cancelled.
func Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Validate checks the policy bounds.
func (p RetryPolicy) Validate() error {
	if p.MaxAttempts < 1 {
		return errors.New("retry policy requires at least one attempt")
	}
	if p.BaseDelay < 0 || p.MaxDelay < 0 {
		return errors.New("retry delays must not be negative")
	}
	if p.MaxDelay > 0 && p.BaseDelay > p.MaxDelay {
		return errors.New("retry base delay exceeds max delay")
	}
	return nil
}

// Do runs attempt until it succeeds, is non-transient, the attempt budget is
// exhausted, or the context is cancelled. It returns the final error.
func (p RetryPolicy) Do(ctx context.Context, attempt func() error) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Sleep == nil {
		p.Sleep = Sleep
	}
	var err error
	for i := 0; i < p.MaxAttempts; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err = attempt()
		if err == nil {
			return nil
		}
		var transient TransientError
		if !errors.As(err, &transient) || !transient.Transient() || i == p.MaxAttempts-1 {
			return err
		}
		if sleepErr := p.Sleep(ctx, p.delay(i, transient.RetryDelay())); sleepErr != nil {
			return sleepErr
		}
	}
	return err
}

func (p RetryPolicy) delay(attempt int, serverDelay time.Duration) time.Duration {
	if serverDelay > p.MaxDelay && p.MaxDelay > 0 {
		serverDelay = p.MaxDelay
	}
	grown := time.Duration(math.Pow(2, float64(attempt))) * p.BaseDelay
	if grown > p.MaxDelay && p.MaxDelay > 0 {
		grown = p.MaxDelay
	}
	if serverDelay > grown {
		return serverDelay
	}
	return grown
}
