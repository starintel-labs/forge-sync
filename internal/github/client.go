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
	"time"

	"github.com/starintel-labs/forge-sync/internal/api"
	"github.com/starintel-labs/forge-sync/internal/model"
)

const maxResponseBody = 32 << 20

type Client struct {
	baseURL    string
	uploadBase string
	token      string
	http       *http.Client
	Retry      api.RetryPolicy
}

type APIError struct {
	Method     string
	URL        string
	StatusCode int
	RequestID  string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github API %s %s returned %d (request %s)", e.Method, e.URL, e.StatusCode, e.RequestID)
}

// Transient reports whether the request may succeed when retried.
func (e *APIError) Transient() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
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
	client := &Client{baseURL: parsed.String(), uploadBase: uploadsBase(parsed), token: token, http: &http.Client{Timeout: timeout}, Retry: api.DefaultRetryPolicy()}
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

func (c *Client) CreatePullRequest(ctx context.Context, owner, name string, source model.PullRequest) (model.PullRequest, error) {
	payload, err := pullRequestPayload(source, true)
	if err != nil {
		return model.PullRequest{}, err
	}
	var created pullRequest
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/pulls"
	if err := c.writeJSON(ctx, http.MethodPost, endpoint, payload, &created); err != nil {
		return model.PullRequest{}, err
	}
	return created.model(), nil
}

func (c *Client) UpdatePullRequest(ctx context.Context, owner, name string, number int64, source model.PullRequest) (model.PullRequest, error) {
	payload, err := pullRequestPayload(source, false)
	if err != nil {
		return model.PullRequest{}, err
	}
	var updated pullRequest
	endpoint := c.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/pulls/" + strconv.FormatInt(number, 10)
	if err := c.writeJSON(ctx, http.MethodPatch, endpoint, payload, &updated); err != nil {
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
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBody))
		response.Body.Close()
		return nil, &APIError{
			Method: method, URL: endpoint, StatusCode: response.StatusCode,
			RequestID: response.Header.Get("X-GitHub-Request-Id"), RetryAfter: retryAfter(response.Header),
		}
	}
	return response, nil
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
