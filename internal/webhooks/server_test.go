package webhooks_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/starintel-labs/forge-sync/internal/state"
	"github.com/starintel-labs/forge-sync/internal/webhooks"
)

type processor struct {
	calls int
	forge string
	event string
}

func (p *processor) ProcessWebhook(_ context.Context, forge, event string, _ []byte) error {
	p.calls++
	p.forge, p.event = forge, event
	return nil
}

func TestGitHubSignatureAndReplaySuppression(t *testing.T) {
	t.Parallel()
	store := webhookStore(t)
	processor := &processor{}
	handler := webhooks.New(store, processor, "github-secret", "forgejo-secret", 1024)
	payload := []byte(`{"repository":{"full_name":"starintel-labs/example"}}`)
	signature := "sha256=" + sign("github-secret", payload)

	for range 2 {
		request := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(payload))
		request.Header.Set("X-Hub-Signature-256", signature)
		request.Header.Set("X-GitHub-Delivery", "delivery-1")
		request.Header.Set("X-GitHub-Event", "issues")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusAccepted {
			t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
		}
	}
	if processor.calls != 1 || processor.forge != "github" || processor.event != "issues" {
		t.Fatalf("processor=%#v", processor)
	}
}

func TestForgejoUsesRawSHA256Signature(t *testing.T) {
	t.Parallel()
	store := webhookStore(t)
	processor := &processor{}
	handler := webhooks.New(store, processor, "github-secret", "forgejo-secret", 1024)
	payload := []byte(`{"repository":{"full_name":"starintel-labs/example"}}`)
	request := httptest.NewRequest(http.MethodPost, "/webhooks/forgejo", bytes.NewReader(payload))
	request.Header.Set("X-Forgejo-Signature", sign("forgejo-secret", payload))
	request.Header.Set("X-Forgejo-Delivery", "delivery-2")
	request.Header.Set("X-Forgejo-Event", "issue")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || processor.calls != 1 || processor.forge != "forgejo" {
		t.Fatalf("status=%d processor=%#v body=%s", response.Code, processor, response.Body.String())
	}
}

func TestInvalidSignatureNeverProcessesOrClaims(t *testing.T) {
	t.Parallel()
	store := webhookStore(t)
	processor := &processor{}
	handler := webhooks.New(store, processor, "github-secret", "forgejo-secret", 1024)
	request := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader([]byte(`{}`)))
	request.Header.Set("X-Hub-Signature-256", "sha256=bad")
	request.Header.Set("X-GitHub-Delivery", "delivery-3")
	request.Header.Set("X-GitHub-Event", "push")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || processor.calls != 0 {
		t.Fatalf("status=%d calls=%d", response.Code, processor.calls)
	}
	claimed, err := store.ClaimWebhookDelivery(context.Background(), "github", "delivery-3", "push", "hash")
	if err != nil || !claimed {
		t.Fatalf("invalid request claimed delivery: claimed=%v err=%v", claimed, err)
	}
}

func TestWebhookBodyIsBounded(t *testing.T) {
	t.Parallel()
	handler := webhooks.New(webhookStore(t), &processor{}, "github-secret", "forgejo-secret", 1024)
	payload := bytes.Repeat([]byte("x"), 1025)
	request := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(payload))
	request.Header.Set("X-Hub-Signature-256", "sha256="+sign("github-secret", payload))
	request.Header.Set("X-GitHub-Delivery", "delivery-4")
	request.Header.Set("X-GitHub-Event", "push")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func webhookStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func sign(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
