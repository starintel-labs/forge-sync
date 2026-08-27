package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/starintel-labs/forge-sync/internal/model"
)

const maxResponseBody = 32 << 20

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

type APIError struct {
	Method     string
	URL        string
	StatusCode int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Forgejo API %s %s returned %d", e.Method, e.URL, e.StatusCode)
}

func New(baseURL, token string, timeout time.Duration) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid Forgejo API URL %q", baseURL)
	}
	if token == "" {
		return nil, errors.New("Forgejo token is empty")
	}
	if timeout <= 0 {
		return nil, errors.New("Forgejo timeout must be positive")
	}
	return &Client{baseURL: parsed.String(), token: token, http: &http.Client{Timeout: timeout}}, nil
}

func (c *Client) ListRepositories(ctx context.Context, namespace string) ([]model.Repository, error) {
	if namespace == "" {
		return nil, errors.New("Forgejo namespace is empty")
	}
	repositories, status, err := c.listRepositories(ctx, "/api/v1/orgs/"+url.PathEscape(namespace)+"/repos?limit=50&page=1")
	if err != nil && status == http.StatusNotFound {
		repositories, _, err = c.listRepositories(ctx, "/api/v1/users/"+url.PathEscape(namespace)+"/repos?limit=50&page=1")
	}
	if err != nil {
		return nil, fmt.Errorf("list Forgejo repositories for %s: %w", namespace, err)
	}
	result := make([]model.Repository, 0, len(repositories))
	for _, repo := range repositories {
		result = append(result, repo.model())
	}
	return result, nil
}

func (c *Client) ListIssues(ctx context.Context, owner, name string) ([]model.Issue, error) {
	base := c.baseURL + "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/issues?state=all&type=issues&limit=50"
	var result []model.Issue
	for page := 1; page <= 1000; page++ {
		var batch []issue
		if _, err := c.doJSON(ctx, http.MethodGet, base+"&page="+fmt.Sprint(page), nil, &batch); err != nil {
			return nil, err
		}
		for _, item := range batch {
			if item.PullRequest != nil {
				continue
			}
			result = append(result, item.model())
		}
		if len(batch) < 50 {
			return result, nil
		}
	}
	return nil, errors.New("Forgejo issue pagination exceeded 1000 pages")
}

func (c *Client) CreateIssue(ctx context.Context, owner, name string, source model.Issue) (model.Issue, error) {
	payload, labelIDs, err := c.issuePayload(ctx, owner, name, source, true)
	if err != nil {
		return model.Issue{}, err
	}
	payload["labels"] = labelIDs
	var created issue
	endpoint := c.baseURL + "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/issues"
	if _, err := c.doJSON(ctx, http.MethodPost, endpoint, payload, &created); err != nil {
		return model.Issue{}, err
	}
	return created.model(), nil
}

func (c *Client) UpdateIssue(ctx context.Context, owner, name string, index int64, source model.Issue) (model.Issue, error) {
	payload, labelIDs, err := c.issuePayload(ctx, owner, name, source, false)
	if err != nil {
		return model.Issue{}, err
	}
	root := c.baseURL + "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/issues/" + fmt.Sprint(index)
	var updated issue
	if _, err := c.doJSON(ctx, http.MethodPatch, root, payload, &updated); err != nil {
		return model.Issue{}, err
	}
	if _, err := c.doJSON(ctx, http.MethodPut, root+"/labels", map[string]any{"labels": labelIDs}, nil); err != nil {
		return model.Issue{}, err
	}
	updated.Labels = make([]label, 0, len(labelIDs))
	for i, id := range labelIDs {
		updated.Labels = append(updated.Labels, label{ID: id, Name: source.Labels[i]})
	}
	return updated.model(), nil
}

func (c *Client) ListComments(ctx context.Context, owner, name string, issueIndex int64) ([]model.Comment, error) {
	root := c.baseURL + "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/issues/" + fmt.Sprint(issueIndex) + "/comments"
	var result []model.Comment
	for page := 1; page <= 1000; page++ {
		var batch []comment
		if _, err := c.doJSON(ctx, http.MethodGet, root+"?limit=50&page="+fmt.Sprint(page), nil, &batch); err != nil {
			return nil, err
		}
		for _, item := range batch {
			result = append(result, item.model(issueIndex))
		}
		if len(batch) < 50 {
			return result, nil
		}
	}
	return nil, errors.New("Forgejo comment pagination exceeded 1000 pages")
}

func (c *Client) CreateComment(ctx context.Context, owner, name string, issueIndex int64, source model.Comment) (model.Comment, error) {
	var created comment
	endpoint := c.baseURL + "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/issues/" + fmt.Sprint(issueIndex) + "/comments"
	if _, err := c.doJSON(ctx, http.MethodPost, endpoint, map[string]string{"body": source.Body}, &created); err != nil {
		return model.Comment{}, err
	}
	return created.model(issueIndex), nil
}

func (c *Client) UpdateComment(ctx context.Context, owner, name string, commentID int64, source model.Comment) (model.Comment, error) {
	var updated comment
	endpoint := c.baseURL + "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/issues/comments/" + fmt.Sprint(commentID)
	if _, err := c.doJSON(ctx, http.MethodPatch, endpoint, map[string]string{"body": source.Body}, &updated); err != nil {
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

type label struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type issue struct {
	ID          int64      `json:"id"`
	Index       int64      `json:"number"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	UpdatedAt   time.Time  `json:"updated_at"`
	Labels      []label    `json:"labels"`
	PullRequest any        `json:"pull_request"`
	Milestone   *milestone `json:"milestone"`
}

type milestone struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

func (i issue) model() model.Issue {
	result := model.Issue{ID: i.ID, Index: i.Index, Title: i.Title, Body: i.Body, State: i.State, UpdatedAt: i.UpdatedAt}
	for _, item := range i.Labels {
		result.Labels = append(result.Labels, item.Name)
	}
	if i.Milestone != nil {
		result.Milestone = i.Milestone.Title
	}
	return result
}

func (c *Client) issuePayload(ctx context.Context, owner, name string, source model.Issue, create bool) (map[string]any, []int64, error) {
	if source.Title == "" || (source.State != "open" && source.State != "closed") {
		return nil, nil, errors.New("issue title or state is invalid")
	}
	labelIDs, err := c.ensureLabels(ctx, owner, name, source.Labels)
	if err != nil {
		return nil, nil, err
	}
	payload := map[string]any{"title": source.Title, "body": source.Body}
	if create {
		payload["closed"] = source.State == "closed"
	} else {
		payload["state"] = source.State
	}
	if source.Milestone != "" {
		milestoneID, err := c.ensureMilestone(ctx, owner, name, source.Milestone)
		if err != nil {
			return nil, nil, err
		}
		payload["milestone"] = milestoneID
	} else {
		payload["milestone"] = 0
	}
	return payload, labelIDs, nil
}

func (c *Client) ensureLabels(ctx context.Context, owner, name string, names []string) ([]int64, error) {
	root := c.baseURL + "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/labels"
	var existing []label
	if _, err := c.doJSON(ctx, http.MethodGet, root+"?limit=50&page=1", nil, &existing); err != nil {
		return nil, err
	}
	byName := map[string]int64{}
	for _, item := range existing {
		byName[item.Name] = item.ID
	}
	result := make([]int64, 0, len(names))
	for _, name := range names {
		id, found := byName[name]
		if !found {
			var created label
			if _, err := c.doJSON(ctx, http.MethodPost, root, map[string]any{"name": name, "color": "#ededed"}, &created); err != nil {
				return nil, err
			}
			id = created.ID
			byName[name] = id
		}
		result = append(result, id)
	}
	return result, nil
}

func (c *Client) ensureMilestone(ctx context.Context, owner, name, title string) (int64, error) {
	root := c.baseURL + "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/milestones"
	var existing []milestone
	if _, err := c.doJSON(ctx, http.MethodGet, root+"?state=all&limit=50&page=1", nil, &existing); err != nil {
		return 0, err
	}
	for _, item := range existing {
		if item.Title == title {
			return item.ID, nil
		}
	}
	var created milestone
	if _, err := c.doJSON(ctx, http.MethodPost, root, map[string]any{"title": title, "state": "open"}, &created); err != nil {
		return 0, err
	}
	return created.ID, nil
}

func (c *Client) listRepositories(ctx context.Context, path string) ([]repository, int, error) {
	var result []repository
	for page := 1; page <= 1000; page++ {
		separator := "&"
		if !strings.Contains(path, "?") {
			separator = "?"
		}
		endpoint := c.baseURL + path
		if page > 1 {
			endpoint = strings.Split(endpoint, "&page=")[0] + separator + fmt.Sprintf("page=%d", page)
		}
		var batch []repository
		status, err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &batch)
		if err != nil {
			return nil, status, err
		}
		result = append(result, batch...)
		if len(batch) < 50 {
			return result, status, nil
		}
	}
	return nil, 0, errors.New("Forgejo pagination exceeded 1000 pages")
}

func (c *Client) MigrateRepository(ctx context.Context, source model.Repository, githubToken string) (model.Repository, error) {
	if source.Owner == "" || source.Name == "" || source.CloneURL == "" {
		return model.Repository{}, errors.New("source repository is incomplete")
	}
	private := source.Visibility != model.VisibilityPublic
	payload := map[string]any{
		"clone_addr":    source.CloneURL,
		"repo_owner":    source.Owner,
		"repo_name":     source.Name,
		"service":       "github",
		"auth_token":    githubToken,
		"private":       private,
		"mirror":        false,
		"issues":        true,
		"labels":        true,
		"milestones":    true,
		"pull_requests": true,
		"releases":      true,
		"wiki":          true,
	}
	var created repository
	if _, err := c.doJSON(ctx, http.MethodPost, c.baseURL+"/api/v1/repos/migrate", payload, &created); err != nil {
		return model.Repository{}, fmt.Errorf("migrate %s: %w", source.FullName, err)
	}
	return created.model(), nil
}

func (c *Client) UpdateRepositoryIdentity(ctx context.Context, oldOwner, oldName, newOwner, newName string) error {
	if oldOwner == "" || oldName == "" || newOwner == "" || newName == "" {
		return errors.New("repository identity is incomplete")
	}
	owner := oldOwner
	name := oldName
	if oldOwner != newOwner {
		endpoint := c.baseURL + "/api/v1/repos/" + url.PathEscape(oldOwner) + "/" + url.PathEscape(oldName) + "/transfer"
		if _, err := c.doJSON(ctx, http.MethodPost, endpoint, map[string]any{"new_owner": newOwner}, nil); err != nil {
			return fmt.Errorf("transfer Forgejo repository: %w", err)
		}
		owner = newOwner
	}
	if name != newName {
		endpoint := c.baseURL + "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
		if _, err := c.doJSON(ctx, http.MethodPatch, endpoint, map[string]any{"name": newName}, nil); err != nil {
			return fmt.Errorf("rename Forgejo repository: %w", err)
		}
	}
	return nil
}

func (c *Client) UpdateRepositorySettings(ctx context.Context, repository model.Repository) error {
	if repository.Owner == "" || repository.Name == "" {
		return errors.New("repository identity is incomplete")
	}
	payload := map[string]any{
		"private":  repository.Visibility != model.VisibilityPublic,
		"archived": repository.Archived,
	}
	endpoint := c.baseURL + "/api/v1/repos/" + url.PathEscape(repository.Owner) + "/" + url.PathEscape(repository.Name)
	if _, err := c.doJSON(ctx, http.MethodPatch, endpoint, payload, nil); err != nil {
		return fmt.Errorf("update Forgejo repository settings: %w", err)
	}
	return nil
}

type repository struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	FullName      string    `json:"full_name"`
	CloneURL      string    `json:"clone_url"`
	DefaultBranch string    `json:"default_branch"`
	Private       bool      `json:"private"`
	Internal      bool      `json:"internal"`
	Archived      bool      `json:"archived"`
	UpdatedAt     time.Time `json:"updated_at"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

func (r repository) model() model.Repository {
	visibility := model.VisibilityPublic
	if r.Private {
		visibility = model.VisibilityPrivate
	} else if r.Internal {
		visibility = model.VisibilityInternal
	}
	owner := r.Owner.Login
	if owner == "" {
		owner, _, _ = strings.Cut(r.FullName, "/")
	}
	return model.Repository{
		ID: r.ID, Owner: owner, Name: r.Name, FullName: r.FullName, CloneURL: r.CloneURL,
		DefaultBranch: r.DefaultBranch, Visibility: visibility, Archived: r.Archived, UpdatedAt: r.UpdatedAt,
	}
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, requestBody, responseBody any) (int, error) {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "token "+c.token)
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBody))
		return response.StatusCode, &APIError{Method: method, URL: endpoint, StatusCode: response.StatusCode}
	}
	if responseBody != nil {
		if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBody)).Decode(responseBody); err != nil {
			return response.StatusCode, fmt.Errorf("decode Forgejo response: %w", err)
		}
	}
	return response.StatusCode, nil
}
