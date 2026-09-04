package github_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	githubclient "github.com/starintel-labs/forge-sync/internal/github"
)

// Pacing must serialize request starts so a full reconciliation cannot
// exhaust the operator's API quota.
func TestPacingSpacesRequests(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var sentAt []time.Time
	clock := struct {
		mu  sync.Mutex
		now time.Time
	}{
		now: time.Unix(0, 0),
	}
	nowFn := func() time.Time {
		clock.mu.Lock()
		defer clock.mu.Unlock()
		return clock.now
	}
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sentAt = append(sentAt, nowFn())
		mu.Unlock()
		atomic.AddInt32(&requests, 1)
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)

	client, err := githubclient.New(server.URL, "token", time.Second, githubclient.WithPacing(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	client.SetThrottleClockForTest(nowFn, func(ctx context.Context, d time.Duration) error {
		clock.mu.Lock()
		clock.now = clock.now.Add(d)
		clock.mu.Unlock()
		return nil
	})
	for i := 0; i < 4; i++ {
		if _, err := client.ListPullRequests(context.Background(), "o", "r"); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sentAt) != 4 {
		t.Fatalf("requests=%d", len(sentAt))
	}
	for i := 1; i < len(sentAt); i++ {
		if gap := sentAt[i].Sub(sentAt[i-1]); gap < 40*time.Millisecond {
			t.Fatalf("gap %d = %v, want >= pace interval", i, gap)
		}
	}
}
