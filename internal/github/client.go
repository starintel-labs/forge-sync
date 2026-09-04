package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/starintel-labs/forge-sync/internal/api"
	"github.com/starintel-labs/forge-sync/internal/model"
	"github.com/starintel-labs/forge-sync/internal/webhooks"
)

const (
	maxResponseBody = 32 << 20
	// defaultThrottleMaxWait bounds a single proactive rate-limit wait.
	defaultThrottleMaxWait = 15 * time.Minute
	// defaultThrottleLowMark starts waiting once fewer responses remain.
	defaultThrottleLowMark = 10
)

type Client struct {
	baseURL    string
	uploadBase string
	token      string
	http       *http.Client
	Retry      api.RetryPolicy

	limitsMu        sync.Mutex
	limitsRemaining int64
	limitsResetAt   time.Time
	throttleMaxWait time.Duration
	throttleLowMark int64
	throttleNow     func() time.Time
	throttleSleep   func(context.Context, time.Duration) error

	paceMu       sync.Mutex
	paceInterval time.Duration
	paceLast     time.Time
}

type APIError struct {
	Method      string
	URL         string
	StatusCode  int
	RequestID   string
	RetryAfter  time.Duration
	rateLimited bool
	Body        string
}

func (e *APIError) Error() string {
	detail := e.Body
	if len(detail) > 300 {
		detail = detail[:300]
	}
	return fmt.Sprintf("github API %s %s returned %d (request %s): %s", e.Method, e.URL, e.StatusCode, e.RequestID, detail)
}

// Transient reports whether the request may succeed when retried.
func (e *APIError) Transient() bool {
	if e.rateLimited {
		return true
	}
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// RateLimited reports whether the failure came from an exhausted rate limit.
func (e *APIError) RateLimited() bool {
	return e.rateLimited
}

// RetryDelay returns the server-provided retry delay, if any.
func (e *APIError) RetryDelay() time.Duration {
	return e.RetryAfter
}

type Option func(*Client)

// WithRetry overrides the default bounded retry policy.
func WithRetry(policy api.RetryPolicy) Option {
	return func(c *Client) { c.Retry = policy }
}

// WithThrottle overrides the proactive rate-limit wait bound and low mark.
func WithThrottle(maxWait time.Duration, lowRemaining int64) Option {
	return func(c *Client) {
		if maxWait > 0 {
			c.throttleMaxWait = maxWait
		}
		if lowRemaining > 0 {
			c.throttleLowMark = lowRemaining
		}
	}
}

// SetThrottleClockForTest replaces the clock and sleeper used by pacing and
// rate-limit waits. It exists for tests only.
func (c *Client) SetThrottleClockForTest(now func() time.Time, sleep func(context.Context, time.Duration) error) {
	if now != nil {
		c.throttleNow = now
	}
	if sleep != nil {
		c.throttleSleep = sleep
	}
}

// WithPacing enforces a minimum spacing between consecutive API requests so
// a full reconciliation cannot exhaust the operator's quota. Zero disables.
func WithPacing(interval time.Duration) Option {
	return func(c *Client) { c.paceInterval = interval }
}

func New(baseURL, token string, timeout time.Duration, options ...Option) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid GitHub API URL %q", baseURL)
	}
	if token == "" {
		return nil, errors.New("GitHub token is empty")
	}
	if timeout <= 0 {
		return nil, errors.New("GitHub timeout must be positive")
	}
	client := &Client{
		baseURL: parsed.String(), uploadBase: uploadsBase(parsed), token: token,
		http: &http.Client{Timeout: timeout}, Retry: api.DefaultRetryPolicy(),
		throttleMaxWait: defaultThrottleMaxWait, throttleLowMark: defaultThrottleLowMark,
		throttleNow: time.Now, throttleSleep: api.Sleep,
	}
	for _, option := range options {
		option(client)
	}
	if err := client.Retry.Validate(); err != nil {
		return nil, fmt.Errorf("GitHub retry policy: %w", err)
	}
	return client, nil
}

func uploadsBase(parsed *url.URL) string {
	if strings.HasPrefix(parsed.Host, "api.") {
		clone := *parsed
		clone.Host = "uploads." + strings.TrimPrefix(parsed.Host, "api.")
		return clone.String()
	}
	return parsed.String()
}

func (c *Client) ListRepositories(ctx context.Context, namespace string) ([]model.Repository, error) {
	if namespace == "" {
		return nil, errors.New("GitHub namespace is empty")
	}
	path := "/orgs/" + url.PathEscape(namespace) + "/repos?per_page=100&type=all&page=1"
	var apiRepos []repository
	status, err := c.getAll(ctx, path, &apiRepos)
	if err != nil && status == http.StatusNotFound {
		apiRepos = nil
		path = "/users/" + url.PathEscape(namespace) + "/repos?per_page=100&type=all&page=1"
		_, err = c.getAll(ctx, path, &apiRepos)
	}
	if err != nil {
		return nil, fmt.Errorf("list GitHub repositories for %s: %w", namespace, err)
	}
	result := make([]model.Repository, 0, len(apiRepos))
	for _, repo := range apiRepos {
		result = append(result, repo.model())
	}
	return result, nil
}

func (c *Client) ListIssues(ctx context.Context, owner, name string) ([]model.Issue, error) {
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/issues?state=all&per_page=100&page=1"
	var result []model.Issue
	for page := 0; endpoint != ""; page++ {
		if page >= 1000 {
			return nil, errors.New("GitHub issue pagination exceeded 1000 pages")
		}
		response, err := c.attempt(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		var batch []issue
		if err := decode(response.Body, &batch); err != nil {
			response.Body.Close()
			return nil, fmt.Errorf("decode GitHub issues: %w", err)
		}
		response.Body.Close()
		for _, item := range batch {
			if item.PullRequest != nil {
				continue
			}
			result = append(result, item.model())
		}
		endpoint = nextLink(response.Header.Get("Link"))
	}
	return result, nil
}

func (c *Client) CreateIssue(ctx context.Context, owner, name string, source model.Issue) (model.Issue, error) {
	payload, err := c.issuePayload(ctx, owner, name, source)
	if err != nil {
		return model.Issue{}, err
	}
	var created issue
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/issues"
	if err := c.writeJSON(ctx, http.MethodPost, endpoint, payload, &created); err != nil {
		return model.Issue{}, err
	}
	return created.model(), nil
}

func (c *Client) UpdateIssue(ctx context.Context, owner, name string, index int64, source model.Issue) (model.Issue, error) {
	payload, err := c.issuePayload(ctx, owner, name, source)
	if err != nil {
		return model.Issue{}, err
	}
	var updated issue
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/issues/" + strconv.FormatInt(index, 10)
	if err := c.writeJSON(ctx, http.MethodPatch, endpoint, payload, &updated); err != nil {
		return model.Issue{}, err
	}
	return updated.model(), nil
}

func (c *Client) ListPullRequests(ctx context.Context, owner, name string) ([]model.PullRequest, error) {
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/pulls?state=all&per_page=100&page=1"
	var result []model.PullRequest
	for page := 0; endpoint != ""; page++ {
		if page >= 1000 {
			return nil, errors.New("GitHub pull request pagination exceeded 1000 pages")
		}
		response, err := c.attempt(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		var batch []pullRequest
		if err := decode(response.Body, &batch); err != nil {
			response.Body.Close()
			return nil, fmt.Errorf("decode GitHub pull requests: %w", err)
		}
		response.Body.Close()
		for _, item := range batch {
			result = append(result, item.model())
		}
		endpoint = nextLink(response.Header.Get("Link"))
	}
	return result, nil
}

// ErrPullRequestExists reports GitHub's refusal to create a pull request
// because one already exists for the same head and base refs.
var ErrPullRequestExists = errors.New("pull request already exists for head and base")

// ErrNoCommits reports GitHub's refusal to create a pull request for refs
// that cannot span one here (no difference, invalid or missing head).
var ErrNoCommits = errors.New("pull request refs cannot span a difference")

func (c *Client) CreatePullRequest(ctx context.Context, owner, name string, source model.PullRequest) (model.PullRequest, error) {
	payload, err := pullRequestPayload(source, true)
	if err != nil {
		return model.PullRequest{}, err
	}
	var created pullRequest
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/pulls"
	err = c.writeJSON(ctx, http.MethodPost, endpoint, payload, &created)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnprocessableEntity {
		if strings.Contains(apiErr.Body, "A pull request already exists for") {
			return model.PullRequest{}, ErrPullRequestExists
		}
		// "No commits between", invalid head ref, and similar validation
		// refusals share one sentinel: the pair cannot exist on GitHub.
		return model.PullRequest{}, ErrNoCommits
	}
	if err != nil {
		return model.PullRequest{}, err
	}
	return created.model(), nil
}

// ErrCannotReopen reports GitHub's refusal to reopen a pull request (for
// example when its head ref was deleted). The reconciler must converge the
// Forgejo side to GitHub's closed state instead.
var ErrCannotReopen = errors.New("pull request cannot be reopened")

func (c *Client) FindPullRequestByHead(ctx context.Context, owner, name, headRef string) (model.PullRequest, bool, error) {
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/pulls?state=all&head=" + url.QueryEscape(owner+":"+headRef)
	response, err := c.attempt(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return model.PullRequest{}, false, err
	}
	defer response.Body.Close()
	var batch []pullRequest
	if err := decode(response.Body, &batch); err != nil {
		return model.PullRequest{}, false, fmt.Errorf("decode GitHub pull requests by head: %w", err)
	}
	if len(batch) == 0 {
		return model.PullRequest{}, false, nil
	}
	return batch[0].model(), true, nil
}

func (c *Client) UpdatePullRequest(ctx context.Context, owner, name string, number int64, source model.PullRequest) (model.PullRequest, error) {
	payload, err := pullRequestPayload(source, false)
	if err != nil {
		return model.PullRequest{}, err
	}
	var updated pullRequest
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/pulls/" + strconv.FormatInt(number, 10)
	err = c.writeJSON(ctx, http.MethodPatch, endpoint, payload, &updated)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnprocessableEntity && strings.Contains(apiErr.Body, "cannot be reopened") {
		return model.PullRequest{}, fmt.Errorf("pull request %d: %w", number, ErrCannotReopen)
	}
	if err != nil {
		return model.PullRequest{}, err
	}
	return updated.model(), nil
}

type pullRequest struct {
	ID        int64     `json:"id"`
	Number    int64     `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Draft     bool      `json:"draft"`
	Merged    bool      `json:"merged"`
	UpdatedAt time.Time `json:"updated_at"`
	Head      struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p pullRequest) model() model.PullRequest {
	return model.PullRequest{
		ID: p.ID, Index: p.Number, Title: p.Title, Body: p.Body, State: p.State,
		Head: p.Head.Ref, Base: p.Base.Ref, Draft: p.Draft, Merged: p.Merged, UpdatedAt: p.UpdatedAt,
	}
}

func pullRequestPayload(source model.PullRequest, create bool) (map[string]any, error) {
	if source.Title == "" || (source.State != "open" && source.State != "closed") || source.Head == "" || source.Base == "" {
		return nil, errors.New("pull request title, state, head, or base is invalid")
	}
	payload := map[string]any{
		"title": source.Title,
		"body":  source.Body,
	}
	if create {
		payload["head"] = source.Head
		payload["base"] = source.Base
		payload["draft"] = source.Draft
	} else {
		payload["state"] = source.State
	}
	return payload, nil
}

func (c *Client) ListComments(ctx context.Context, owner, name string, issueIndex int64) ([]model.Comment, error) {
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/issues/" + strconv.FormatInt(issueIndex, 10) + "/comments?per_page=100&page=1"
	var result []model.Comment
	for page := 0; endpoint != ""; page++ {
		if page >= 1000 {
			return nil, errors.New("GitHub comment pagination exceeded 1000 pages")
		}
		response, err := c.attempt(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		var batch []comment
		if err := decode(response.Body, &batch); err != nil {
			response.Body.Close()
			return nil, err
		}
		response.Body.Close()
		for _, item := range batch {
			result = append(result, item.model(issueIndex))
		}
		endpoint = nextLink(response.Header.Get("Link"))
	}
	return result, nil
}

func (c *Client) CreateComment(ctx context.Context, owner, name string, issueIndex int64, source model.Comment) (model.Comment, error) {
	var created comment
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/issues/" + strconv.FormatInt(issueIndex, 10) + "/comments"
	if err := c.writeJSON(ctx, http.MethodPost, endpoint, map[string]string{"body": source.Body}, &created); err != nil {
		return model.Comment{}, err
	}
	return created.model(issueIndex), nil
}

func (c *Client) UpdateComment(ctx context.Context, owner, name string, commentID int64, source model.Comment) (model.Comment, error) {
	var updated comment
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/issues/comments/" + strconv.FormatInt(commentID, 10)
	if err := c.writeJSON(ctx, http.MethodPatch, endpoint, map[string]string{"body": source.Body}, &updated); err != nil {
		return model.Comment{}, err
	}
	return updated.model(source.IssueID), nil
}

type comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (c comment) model(issueID int64) model.Comment {
	return model.Comment{ID: c.ID, IssueID: issueID, Body: c.Body, UpdatedAt: c.UpdatedAt}
}

type issue struct {
	ID          int64     `json:"id"`
	Number      int64     `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	UpdatedAt   time.Time `json:"updated_at"`
	PullRequest any       `json:"pull_request"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Milestone *struct {
		Title string `json:"title"`
	} `json:"milestone"`
}

func (i issue) model() model.Issue {
	result := model.Issue{ID: i.ID, Index: i.Number, Title: i.Title, Body: i.Body, State: i.State, UpdatedAt: i.UpdatedAt}
	for _, label := range i.Labels {
		result.Labels = append(result.Labels, label.Name)
	}
	if i.Milestone != nil {
		result.Milestone = i.Milestone.Title
	}
	return result
}

func (c *Client) issuePayload(ctx context.Context, owner, name string, source model.Issue) (map[string]any, error) {
	if source.Title == "" || (source.State != "open" && source.State != "closed") {
		return nil, errors.New("issue title or state is invalid")
	}
	labels, err := c.ensureLabels(ctx, owner, name, source.Labels)
	if err != nil {
		return nil, err
	}
	if labels == nil {
		// GitHub rejects a null labels value; absent labels are [].
		labels = []string{}
	}
	payload := map[string]any{
		"title":  source.Title,
		"body":   source.Body,
		"state":  source.State,
		"labels": labels,
	}
	if source.Milestone != "" {
		number, err := c.ensureMilestone(ctx, owner, name, source.Milestone)
		if err != nil {
			return nil, err
		}
		payload["milestone"] = number
	} else {
		payload["milestone"] = nil
	}
	return payload, nil
}

func (c *Client) ensureLabels(ctx context.Context, owner, name string, names []string) ([]string, error) {
	root := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/labels"
	response, err := c.attempt(ctx, http.MethodGet, root+"?per_page=100&page=1", nil)
	if err != nil {
		return nil, err
	}
	var existing []struct {
		Name string `json:"name"`
	}
	if err := decode(response.Body, &existing); err != nil {
		response.Body.Close()
		return nil, err
	}
	response.Body.Close()
	present := map[string]bool{}
	for _, label := range existing {
		present[label.Name] = true
	}
	for _, name := range names {
		if present[name] {
			continue
		}
		var created struct {
			Name string `json:"name"`
		}
		if err := c.writeJSON(ctx, http.MethodPost, root, map[string]string{"name": name, "color": "ededed"}, &created); err != nil {
			return nil, err
		}
		present[name] = true
	}
	return append([]string(nil), names...), nil
}

func (c *Client) ensureMilestone(ctx context.Context, owner, name, title string) (int64, error) {
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/milestones?state=all&per_page=100"
	response, err := c.attempt(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	var milestones []struct {
		Number int64  `json:"number"`
		Title  string `json:"title"`
	}
	if err := decode(response.Body, &milestones); err != nil {
		response.Body.Close()
		return 0, err
	}
	response.Body.Close()
	for _, milestone := range milestones {
		if milestone.Title == title {
			return milestone.Number, nil
		}
	}
	var created struct {
		Number int64 `json:"number"`
	}
	endpoint = c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/milestones"
	if err := c.writeJSON(ctx, http.MethodPost, endpoint, map[string]string{"title": title}, &created); err != nil {
		return 0, err
	}
	return created.Number, nil
}

func (c *Client) ListReleases(ctx context.Context, owner, name string) ([]model.Release, error) {
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/releases?per_page=100&page=1"
	var result []model.Release
	for page := 0; endpoint != ""; page++ {
		if page >= 1000 {
			return nil, errors.New("GitHub release pagination exceeded 1000 pages")
		}
		response, err := c.attempt(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		var batch []release
		if err := decode(response.Body, &batch); err != nil {
			response.Body.Close()
			return nil, fmt.Errorf("decode GitHub releases: %w", err)
		}
		response.Body.Close()
		for _, item := range batch {
			result = append(result, item.model())
		}
		endpoint = nextLink(response.Header.Get("Link"))
	}
	return result, nil
}

func (c *Client) CreateRelease(ctx context.Context, owner, name string, source model.Release) (model.Release, error) {
	var created release
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/releases"
	if err := c.writeJSON(ctx, http.MethodPost, endpoint, releasePayload(source), &created); err != nil {
		return model.Release{}, err
	}
	return created.model(), nil
}

func (c *Client) UpdateRelease(ctx context.Context, owner, name string, id int64, source model.Release) (model.Release, error) {
	var updated release
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/releases/" + strconv.FormatInt(id, 10)
	if err := c.writeJSON(ctx, http.MethodPatch, endpoint, releasePayload(source), &updated); err != nil {
		return model.Release{}, err
	}
	return updated.model(), nil
}

func (c *Client) DownloadReleaseAsset(ctx context.Context, owner, name string, releaseID, assetID int64) ([]byte, error) {
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/releases/assets/" + strconv.FormatInt(assetID, 10)
	var content []byte
	err := c.Retry.Do(ctx, func() error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Accept", "application/octet-stream")
		request.Header.Set("Authorization", "Bearer "+c.token)
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		response, err := c.http.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBody))
			return &APIError{Method: http.MethodGet, URL: endpoint, StatusCode: response.StatusCode, RequestID: response.Header.Get("X-GitHub-Request-Id"), RetryAfter: retryAfter(response.Header)}
		}
		content, err = io.ReadAll(io.LimitReader(response.Body, maxResponseBody))
		return err
	})
	if err != nil {
		return nil, err
	}
	return content, nil
}

func (c *Client) UploadReleaseAsset(ctx context.Context, owner, name string, releaseID int64, assetName string, content []byte) error {
	endpoint := c.uploadBase + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/releases/" + strconv.FormatInt(releaseID, 10) + "/assets?name=" + url.QueryEscape(assetName)
	return c.Retry.Do(ctx, func() error {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(content))
		if err != nil {
			return err
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("Authorization", "Bearer "+c.token)
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		request.Header.Set("Content-Type", "application/octet-stream")
		request.ContentLength = int64(len(content))
		response, err := c.http.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBody))
			return &APIError{Method: http.MethodPost, URL: endpoint, StatusCode: response.StatusCode, RequestID: response.Header.Get("X-GitHub-Request-Id"), RetryAfter: retryAfter(response.Header)}
		}
		return nil
	})
}

type release struct {
	ID         int64      `json:"id"`
	TagName    string     `json:"tag_name"`
	Name       string     `json:"name"`
	Body       string     `json:"body"`
	Draft      bool       `json:"draft"`
	Prerelease bool       `json:"prerelease"`
	CreatedAt  time.Time  `json:"created_at"`
	Assets     []assetRef `json:"assets"`
}

type assetRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func (r release) model() model.Release {
	result := model.Release{
		ID: r.ID, Tag: r.TagName, Name: r.Name, Body: r.Body,
		Draft: r.Draft, Prerelease: r.Prerelease, CreatedAt: r.CreatedAt,
	}
	for _, asset := range r.Assets {
		result.Assets = append(result.Assets, model.ReleaseAsset{ID: asset.ID, Name: asset.Name, Size: asset.Size})
	}
	return result
}

func releasePayload(source model.Release) map[string]any {
	return map[string]any{
		"tag_name":    source.Tag,
		"name":        source.Name,
		"body":        source.Body,
		"draft":       source.Draft,
		"prerelease":  source.Prerelease,
		"make_latest": "legacy",
	}
}

func (c *Client) writeJSON(ctx context.Context, method, endpoint string, payload, destination any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	response, err := c.attempt(ctx, method, endpoint, func() io.Reader { return bytes.NewReader(encoded) })
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if destination != nil {
		if err := decode(response.Body, destination); err != nil {
			return err
		}
	}
	return nil
}

type repository struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	FullName      string    `json:"full_name"`
	CloneURL      string    `json:"clone_url"`
	DefaultBranch string    `json:"default_branch"`
	Visibility    string    `json:"visibility"`
	Private       bool      `json:"private"`
	Archived      bool      `json:"archived"`
	SizeKB        int64     `json:"size"`
	UpdatedAt     time.Time `json:"updated_at"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

func (r repository) model() model.Repository {
	visibility := model.VisibilityPrivate
	if r.Visibility == string(model.VisibilityPublic) && !r.Private {
		visibility = model.VisibilityPublic
	} else if r.Visibility == string(model.VisibilityInternal) {
		visibility = model.VisibilityInternal
	}
	owner := r.Owner.Login
	if owner == "" {
		owner, _, _ = strings.Cut(r.FullName, "/")
	}
	return model.Repository{
		ID:            r.ID,
		Owner:         owner,
		Name:          r.Name,
		FullName:      r.FullName,
		CloneURL:      r.CloneURL,
		DefaultBranch: r.DefaultBranch,
		Visibility:    visibility,
		Archived:      r.Archived,
		SizeKB:        r.SizeKB,
		UpdatedAt:     r.UpdatedAt,
	}
}

func (c *Client) getAll(ctx context.Context, path string, destination *[]repository) (int, error) {
	next := c.baseURL + path
	for page := 0; next != ""; page++ {
		if page >= 1000 {
			return 0, errors.New("GitHub pagination exceeded 1000 pages")
		}
		var repositories []repository
		response, err := c.attempt(ctx, http.MethodGet, next, nil)
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) {
				return apiErr.StatusCode, err
			}
			return 0, err
		}
		if err := decode(response.Body, &repositories); err != nil {
			response.Body.Close()
			return response.StatusCode, fmt.Errorf("decode GitHub repositories: %w", err)
		}
		response.Body.Close()
		*destination = append(*destination, repositories...)
		next = nextLink(response.Header.Get("Link"))
	}
	return http.StatusOK, nil
}

func (c *Client) request(ctx context.Context, method, endpoint string, body io.Reader) (*http.Response, error) {
	if err := c.pace(ctx); err != nil {
		return nil, err
	}
	if err := c.obeyRateLimit(ctx); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	c.recordRateLimit(response.Header)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBody))
		response.Body.Close()
		apiErr := &APIError{
			Method: method, URL: endpoint, StatusCode: response.StatusCode,
			RequestID: response.Header.Get("X-GitHub-Request-Id"), Body: string(body),
		}
		// GitHub reports exhausted limits as 403 or 429 with rate headers;
		// ordinary 403s are permission failures and stay permanent.
		if response.StatusCode == http.StatusTooManyRequests || (response.StatusCode == http.StatusForbidden && c.rateLimited(response.Header)) {
			apiErr.rateLimited = true
			apiErr.RetryAfter = c.rateWaitFromHeaders(response.Header)
		} else {
			apiErr.RetryAfter = retryAfter(response.Header)
		}
		return nil, apiErr
	}
	return response, nil
}

// pace serializes request starts at least paceInterval apart so the client
// cannot burst through the operator's API quota.
func (c *Client) pace(ctx context.Context) error {
	if c.paceInterval <= 0 {
		return nil
	}
	c.paceMu.Lock()
	now := c.throttleNow()
	wait := c.paceInterval - now.Sub(c.paceLast)
	if wait > 0 {
		c.paceLast = now.Add(wait)
		c.paceMu.Unlock()
		return c.throttleSleep(ctx, wait)
	}
	c.paceLast = now
	c.paceMu.Unlock()
	return nil
}

// obeyRateLimit blocks before a request when the last observed budget is
// nearly exhausted, waiting for the documented reset time (bounded by
// throttleMaxWait). Rate limits are obeyed, not hammered.
func (c *Client) obeyRateLimit(ctx context.Context) error {
	c.limitsMu.Lock()
	remaining, resetAt := c.limitsRemaining, c.limitsResetAt
	lowMark, maxWait := c.throttleLowMark, c.throttleMaxWait
	now := c.throttleNow()
	c.limitsMu.Unlock()
	if remaining <= 0 || remaining >= lowMark {
		return nil
	}
	wait := resetAt.Sub(now)
	if wait <= 0 || maxWait <= 0 {
		return nil
	}
	if wait > maxWait {
		wait = maxWait
	}
	return c.throttleSleep(ctx, wait)
}

func (c *Client) recordRateLimit(header http.Header) {
	remaining, remainingErr := strconv.ParseInt(header.Get("X-RateLimit-Remaining"), 10, 64)
	reset, resetErr := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64)
	if remainingErr != nil || resetErr != nil {
		return
	}
	c.limitsMu.Lock()
	c.limitsRemaining, c.limitsResetAt = remaining, time.Unix(reset, 0)
	c.limitsMu.Unlock()
}

func (c *Client) rateLimited(header http.Header) bool {
	remaining, err := strconv.ParseInt(header.Get("X-RateLimit-Remaining"), 10, 64)
	if err == nil && remaining == 0 {
		return true
	}
	return header.Get("Retry-After") != ""
}

func (c *Client) rateWaitFromHeaders(header http.Header) time.Duration {
	if seconds, err := strconv.Atoi(header.Get("Retry-After")); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if reset, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		if wait := time.Unix(reset, 0).Sub(c.throttleNow()); wait > 0 {
			return wait
		}
	}
	return time.Minute
}

// attempt performs a single HTTP request with bounded retry on transient
// failures. The body factory rebuilds the request body for every attempt.
func (c *Client) attempt(ctx context.Context, method, endpoint string, body func() io.Reader) (*http.Response, error) {
	var response *http.Response
	err := c.Retry.Do(ctx, func() error {
		var reader io.Reader
		if body != nil {
			reader = body()
		}
		result, err := c.request(ctx, method, endpoint, reader)
		if err != nil {
			return err
		}
		response = result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

func decode(reader io.Reader, destination any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxResponseBody))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return nil
}

func nextLink(header string) string {
	for _, link := range strings.Split(header, ",") {
		parts := strings.Split(link, ";")
		if len(parts) < 2 || !strings.Contains(parts[1], `rel="next"`) {
			continue
		}
		return strings.Trim(strings.TrimSpace(parts[0]), "<>")
	}
	return ""
}

func retryAfter(header http.Header) time.Duration {
	if seconds, err := strconv.Atoi(header.Get("Retry-After")); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return 0
}

type webhookConfig struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Secret      string `json:"secret,omitempty"`
}

type githubWebhook struct {
	ID     int64         `json:"id"`
	Active bool          `json:"active"`
	Events []string      `json:"events"`
	Config webhookConfig `json:"config"`
}

func (w githubWebhook) model() webhooks.Hook {
	return webhooks.Hook{ID: w.ID, URL: w.Config.URL, Events: append([]string(nil), w.Events...), Active: w.Active}
}

// listWebhooks pages through a GitHub webhook listing. It returns the raw
// HTTP status so callers can distinguish a missing org (404) from an empty
// hook list (200).
func (c *Client) listWebhooks(ctx context.Context, path string) ([]webhooks.Hook, int, error) {
	var hooks []webhooks.Hook
	next := c.baseURL + path
	for page := 0; next != ""; page++ {
		if page >= 1000 {
			return nil, 0, errors.New("GitHub webhook pagination exceeded 1000 pages")
		}
		response, err := c.attempt(ctx, http.MethodGet, next, nil)
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) {
				return nil, apiErr.StatusCode, err
			}
			return nil, 0, err
		}
		var batch []githubWebhook
		if err := decode(response.Body, &batch); err != nil {
			response.Body.Close()
			return nil, response.StatusCode, fmt.Errorf("decode GitHub webhooks: %w", err)
		}
		response.Body.Close()
		for _, hook := range batch {
			hooks = append(hooks, hook.model())
		}
		next = nextLink(response.Header.Get("Link"))
	}
	return hooks, http.StatusOK, nil
}

// ListOrgWebhooks returns the org's webhooks; found is false when the
// namespace is not an organization (HTTP 404).
func (c *Client) ListOrgWebhooks(ctx context.Context, org string) ([]webhooks.Hook, bool, error) {
	hooks, status, err := c.listWebhooks(ctx, "/orgs/"+url.PathEscape(org)+"/hooks")
	if err != nil {
		return nil, false, err
	}
	return hooks, status != http.StatusNotFound, nil
}

func (c *Client) CreateOrgWebhook(ctx context.Context, org string, hook webhooks.Hook, secret string) error {
	return c.writeJSON(ctx, http.MethodPost, "/orgs/"+url.PathEscape(org)+"/hooks", githubWebhookCreate(hook, secret), nil)
}

func (c *Client) UpdateOrgWebhook(ctx context.Context, org string, id int64, hook webhooks.Hook, secret string) error {
	return c.writeJSON(ctx, http.MethodPatch, fmt.Sprintf("/orgs/%s/hooks/%d", url.PathEscape(org), id), githubWebhookUpdate(hook, secret), nil)
}

func (c *Client) ListRepoWebhooks(ctx context.Context, owner, name string) ([]webhooks.Hook, error) {
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/hooks"
	hooks, status, err := c.listWebhooks(ctx, path)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("list GitHub webhooks for %s/%s: repository not found", owner, name)
	}
	return hooks, nil
}

func (c *Client) CreateRepoWebhook(ctx context.Context, owner, name string, hook webhooks.Hook, secret string) error {
	endpoint := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/hooks"
	return c.writeJSON(ctx, http.MethodPost, endpoint, githubWebhookCreate(hook, secret), nil)
}

func (c *Client) UpdateRepoWebhook(ctx context.Context, owner, name string, id int64, hook webhooks.Hook, secret string) error {
	endpoint := fmt.Sprintf("/repos/%s/%s/hooks/%d", url.PathEscape(owner), url.PathEscape(name), id)
	return c.writeJSON(ctx, http.MethodPatch, endpoint, githubWebhookUpdate(hook, secret), nil)
}

type githubWebhookCreatePayload struct {
	Name   string        `json:"name"`
	Active bool          `json:"active"`
	Events []string      `json:"events"`
	Config webhookConfig `json:"config"`
}

type githubWebhookUpdatePayload struct {
	Active bool          `json:"active"`
	Events []string      `json:"events"`
	Config webhookConfig `json:"config"`
}

func githubWebhookCreate(hook webhooks.Hook, secret string) githubWebhookCreatePayload {
	return githubWebhookCreatePayload{Name: "web", Active: hook.Active, Events: hook.Events,
		Config: webhookConfig{URL: hook.URL, ContentType: "json", Secret: secret}}
}

func githubWebhookUpdate(hook webhooks.Hook, secret string) githubWebhookUpdatePayload {
	return githubWebhookUpdatePayload{Active: hook.Active, Events: hook.Events, Config: webhookConfig{URL: hook.URL, ContentType: "json", Secret: secret}}
}
