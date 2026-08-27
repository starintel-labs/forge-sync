package model

import "time"

type Visibility string

const (
	VisibilityPrivate  Visibility = "private"
	VisibilityInternal Visibility = "internal"
	VisibilityPublic   Visibility = "public"
)

type Repository struct {
	ID            int64
	Owner         string
	Name          string
	FullName      string
	CloneURL      string
	DefaultBranch string
	Visibility    Visibility
	Archived      bool
	UpdatedAt     time.Time
}

type RepositoryMapping struct {
	GitHubID       int64
	GitHubFullName string
	ForgejoOwner   string
	ForgejoName    string
	Visibility     Visibility
	Archived       bool
	LastStateHash  string
	UpdatedAt      time.Time
}

type Issue struct {
	ID        int64
	Index     int64
	Title     string
	Body      string
	State     string
	Labels    []string
	Milestone string
	UpdatedAt time.Time
}

type PullRequest struct {
	ID        int64
	Index     int64
	Title     string
	Body      string
	State     string
	Head      string
	Base      string
	Draft     bool
	Merged    bool
	UpdatedAt time.Time
}

type Comment struct {
	ID        int64
	IssueID   int64
	Body      string
	UpdatedAt time.Time
}

type IssueMapping struct {
	RepositoryGitHubID int64
	GitHubID           int64
	ForgejoID          int64
	GitHubIndex        int64
	ForgejoIndex       int64
	LastStateHash      string
	UpdatedAt          time.Time
}

type PullRequestMapping struct {
	RepositoryGitHubID int64
	GitHubID           int64
	ForgejoID          int64
	GitHubIndex        int64
	ForgejoIndex       int64
	LastStateHash      string
	UpdatedAt          time.Time
}

type CommentMapping struct {
	RepositoryGitHubID int64
	IssueGitHubID      int64
	GitHubID           int64
	ForgejoID          int64
	LastStateHash      string
	UpdatedAt          time.Time
}

type Release struct {
	ID         int64
	Tag        string
	Name       string
	Body       string
	Draft      bool
	Prerelease bool
	CreatedAt  time.Time
	Assets     []ReleaseAsset
}

type ReleaseAsset struct {
	ID   int64
	Name string
	Size int64
}

type ReleaseMapping struct {
	RepositoryGitHubID int64
	GitHubID           int64
	ForgejoID          int64
	Tag                string
	LastStateHash      string
	UpdatedAt          time.Time
}

type Ref struct {
	Name string
	SHA  string
}

type Conflict struct {
	Kind           string
	Repository     string
	ObjectKey      string
	GitHubState    string
	ForgejoState   string
	LastKnownState string
	CreatedAt      time.Time
}

type Inventory struct {
	GitHubRepositories  int `json:"github_repositories"`
	ForgejoRepositories int `json:"forgejo_repositories"`
	Missing             int `json:"missing"`
	InSync              int `json:"in_sync"`
	Conflicted          int `json:"conflicted"`
}
