package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"

	"github.com/starintel-labs/forge-sync/internal/state"
)

type Processor interface {
	ProcessWebhook(context.Context, string, string, []byte) error
}

type Server struct {
	store         *state.Store
	processor     Processor
	githubSecret  []byte
	forgejoSecret []byte
	maxBody       int64
}

var allowedEvents = map[string]map[string]bool{
	"github": {
		"ping": true, "repository": true, "push": true, "issues": true,
		"issue_comment": true, "pull_request": true, "pull_request_review": true,
		"pull_request_review_comment": true, "release": true,
	},
	"forgejo": {
		"ping": true, "repository": true, "push": true, "issues": true, "issue": true,
		"issue_comment": true, "pull_request": true, "release": true, "create": true,
	},
}

func New(store *state.Store, processor Processor, githubSecret, forgejoSecret string, maxBody int64) *Server {
	if store == nil || processor == nil || githubSecret == "" || forgejoSecret == "" || maxBody <= 0 {
		panic("webhook server requires state, processor, secrets, and positive body limit")
	}
	return &Server{
		store: store, processor: processor, githubSecret: []byte(githubSecret),
		forgejoSecret: []byte(forgejoSecret), maxBody: maxBody,
	}
}

func (s *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	forge := ""
	switch request.URL.Path {
	case "/webhooks/github":
		forge = "github"
	case "/webhooks/forgejo":
		forge = "forgejo"
	default:
		http.NotFound(response, request)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, s.maxBody+1))
	if err != nil {
		http.Error(response, "read request", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > s.maxBody {
		http.Error(response, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	deliveryID, event, signature := headers(request.Header, forge)
	if deliveryID == "" || len(deliveryID) > 200 || event == "" || len(event) > 100 {
		http.Error(response, "missing or invalid webhook headers", http.StatusBadRequest)
		return
	}
	if !allowedEvents[forge][event] {
		http.Error(response, "unsupported webhook event", http.StatusBadRequest)
		return
	}
	secret := s.githubSecret
	if forge == "forgejo" {
		secret = s.forgejoSecret
	}
	if !validSignature(forge, signature, secret, body) {
		http.Error(response, "invalid webhook signature", http.StatusUnauthorized)
		return
	}
	payloadHashBytes := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(payloadHashBytes[:])
	claimed, err := s.store.ClaimWebhookDelivery(request.Context(), forge, deliveryID, event, payloadHash)
	if err != nil {
		http.Error(response, "record webhook", http.StatusInternalServerError)
		return
	}
	if !claimed {
		response.WriteHeader(http.StatusAccepted)
		return
	}
	processErr := s.processor.ProcessWebhook(request.Context(), forge, event, body)
	markErr := s.store.MarkWebhookProcessed(request.Context(), forge, deliveryID, processErr)
	if processErr != nil || markErr != nil {
		http.Error(response, "process webhook", http.StatusInternalServerError)
		return
	}
	response.WriteHeader(http.StatusAccepted)
}

func headers(header http.Header, forge string) (deliveryID, event, signature string) {
	if forge == "github" {
		return header.Get("X-GitHub-Delivery"), header.Get("X-GitHub-Event"), header.Get("X-Hub-Signature-256")
	}
	deliveryID = header.Get("X-Forgejo-Delivery")
	event = header.Get("X-Forgejo-Event")
	signature = header.Get("X-Forgejo-Signature")
	if deliveryID == "" {
		deliveryID = header.Get("X-Gitea-Delivery")
	}
	if event == "" {
		event = header.Get("X-Gitea-Event")
	}
	if signature == "" {
		signature = header.Get("X-Gitea-Signature")
	}
	return deliveryID, event, signature
}

func validSignature(forge, signature string, secret, body []byte) bool {
	if forge == "github" {
		var found bool
		signature, found = strings.CutPrefix(signature, "sha256=")
		if !found {
			return false
		}
	}
	provided, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hmac.Equal(provided, mac.Sum(nil))
}
