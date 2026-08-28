package api

import (
	"context"
	"errors"
	"math"
	"net/url"
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
// NetworkRetry additionally retries transport-level failures (resets, EOF)
// and must only be enabled for idempotent operations. RateLimitMaxDelay is
// the (longer) ceiling applied when a server-reported rate limit demands a
// wait; rate limits are obeyed rather than hammered.
type RetryPolicy struct {
	MaxAttempts       int
	BaseDelay         time.Duration
	MaxDelay          time.Duration
	RateLimitMaxDelay time.Duration
	NetworkRetry      bool
	Sleep             func(context.Context, time.Duration) error
}

// DefaultRetryPolicy returns the production retry policy.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 4, BaseDelay: time.Second, MaxDelay: 30 * time.Second, RateLimitMaxDelay: 15 * time.Minute, Sleep: Sleep}
}

// RateLimitedError is implemented by errors caused by an exhausted rate
// limit whose server-provided reset time is available.
type RateLimitedError interface {
	RetryDelay() time.Duration
	RateLimited() bool
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
		if errors.As(err, &transient) && transient.Transient() && i < p.MaxAttempts-1 {
			rateLimited := false
			var limited RateLimitedError
			if errors.As(err, &limited) && limited.RateLimited() {
				rateLimited = true
			}
			if sleepErr := p.Sleep(ctx, p.delay(i, transient.RetryDelay(), rateLimited)); sleepErr != nil {
				return sleepErr
			}
			continue
		}
		if p.NetworkRetry && isTransportError(err) && i < p.MaxAttempts-1 {
			if sleepErr := p.Sleep(ctx, p.delay(i, 0, false)); sleepErr != nil {
				return sleepErr
			}
			continue
		}
		return err
	}
	return err
}

func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	var transient TransientError
	if errors.As(err, &transient) {
		return false
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

func (p RetryPolicy) delay(attempt int, serverDelay time.Duration, rateLimited bool) time.Duration {
	cap := p.MaxDelay
	if rateLimited && p.RateLimitMaxDelay > 0 {
		// Rate limits are obeyed, not hammered: waits may run much longer
		// than ordinary backoff.
		cap = p.RateLimitMaxDelay
	}
	if serverDelay > cap && cap > 0 {
		serverDelay = cap
	}
	grown := time.Duration(math.Pow(2, float64(attempt))) * p.BaseDelay
	if grown > cap && cap > 0 {
		grown = cap
	}
	if serverDelay > grown {
		return serverDelay
	}
	return grown
}
