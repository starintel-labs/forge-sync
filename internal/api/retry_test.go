package api_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/starintel-labs/forge-sync/internal/api"
)

type fakeTransient struct {
	delay time.Duration
}

func (f fakeTransient) Error() string             { return "transient" }
func (f fakeTransient) Transient() bool           { return true }
func (f fakeTransient) RetryDelay() time.Duration { return f.delay }

func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	t.Parallel()
	var slept []time.Duration
	policy := api.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: 10 * time.Second, Sleep: func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}}
	attempts := 0
	if err := policy.Do(context.Background(), func() error {
		attempts++
		if attempts < 3 {
			return fakeTransient{}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts=%d", attempts)
	}
	if len(slept) != 2 || slept[0] != time.Second || slept[1] != 2*time.Second {
		t.Fatalf("slept=%v", slept)
	}
}

func TestRetryDelaysAreBoundedAndHonorRetryAfter(t *testing.T) {
	t.Parallel()
	var slept []time.Duration
	policy := api.RetryPolicy{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: 4 * time.Second, Sleep: func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}}
	err := policy.Do(context.Background(), func() error { return fakeTransient{delay: 3 * time.Second} })
	if err == nil {
		t.Fatal("exhausted retries returned nil")
	}
	want := []time.Duration{3 * time.Second, 3 * time.Second, 4 * time.Second, 4 * time.Second}
	if len(slept) != len(want) {
		t.Fatalf("slept=%v want=%v", slept, want)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Fatalf("slept=%v want=%v", slept, want)
		}
	}
}

func TestRetryFailsImmediatelyOnNonTransient(t *testing.T) {
	t.Parallel()
	called := false
	policy := api.RetryPolicy{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: time.Second, Sleep: func(context.Context, time.Duration) error {
		t.Fatal("non-transient failure was retried")
		return nil
	}}
	err := policy.Do(context.Background(), func() error { return errors.New("permanent") })
	if err == nil || err.Error() != "permanent" {
		t.Fatalf("err=%v", err)
	}
	called = true
	if !called {
		t.Fatal("attempt was not made")
	}
}

func TestRetryRespectsContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	policy := api.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: time.Second}
	if err := policy.Do(ctx, func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestRetryPolicyValidation(t *testing.T) {
	t.Parallel()
	if err := (api.RetryPolicy{MaxAttempts: 0}).Validate(); err == nil {
		t.Fatal("zero attempts accepted")
	}
	if err := (api.RetryPolicy{MaxAttempts: 1, BaseDelay: 10 * time.Second, MaxDelay: time.Second}).Validate(); err == nil {
		t.Fatal("base delay above max accepted")
	}
	if err := (api.RetryPolicy{MaxAttempts: 1}).Validate(); err != nil {
		t.Fatal(err)
	}
}
