package gitrefs

import (
	"fmt"
	"sort"
	"time"

	"github.com/starintel-labs/forge-sync/internal/model"
)

type Forge string

const (
	GitHub  Forge = "github"
	Forgejo Forge = "forgejo"
)

type Action struct {
	Ref   string
	SHA   string
	From  Forge
	To    Forge
	Force bool
}

type Ancestry func(ancestor, descendant string) (bool, error)

// Plan derives the reconciliation actions for every branch and tag ref that
// exists on either forge. Sync is two-way: a ref that is behind on one forge
// fast-forwards from the other, in whichever direction that is. Forgejo is
// the master forge: when the two sides truly diverge, its state is enforced
// on the GitHub mirror and the overridden GitHub SHA is recorded for audit.
// Refs are never deleted; a ref missing on one forge is copied from the
// other, so history is only ever overwritten on GitHub, never on Forgejo.
func Plan(repository string, githubRefs, forgejoRefs map[string]string, isAncestor Ancestry) ([]Action, []model.Conflict, error) {
	if repository == "" || isAncestor == nil {
		return nil, nil, fmt.Errorf("repository and ancestry checker are required")
	}
	var actions []Action
	var conflicts []model.Conflict

	for _, ref := range union(githubRefs, forgejoRefs) {
		githubSHA, onGitHub := githubRefs[ref]
		forgejoSHA, onForgejo := forgejoRefs[ref]
		switch {
		case !onForgejo:
			actions = append(actions, Action{Ref: ref, SHA: githubSHA, From: GitHub, To: Forgejo})
		case !onGitHub:
			actions = append(actions, Action{Ref: ref, SHA: forgejoSHA, From: Forgejo, To: GitHub})
		case githubSHA == forgejoSHA:
		default:
			forgejoAhead, err := isAncestor(githubSHA, forgejoSHA)
			if err != nil {
				return nil, nil, fmt.Errorf("check ancestry for %s: %w", ref, err)
			}
			if forgejoAhead {
				actions = append(actions, Action{Ref: ref, SHA: forgejoSHA, From: Forgejo, To: GitHub})
				continue
			}
			githubAhead, err := isAncestor(forgejoSHA, githubSHA)
			if err != nil {
				return nil, nil, fmt.Errorf("check ancestry for %s: %w", ref, err)
			}
			if githubAhead {
				actions = append(actions, Action{Ref: ref, SHA: githubSHA, From: GitHub, To: Forgejo})
				continue
			}
			actions = append(actions, Action{Ref: ref, SHA: forgejoSHA, From: Forgejo, To: GitHub, Force: true})
			conflicts = append(conflicts, model.Conflict{
				Kind: "git-ref-override", Repository: repository, ObjectKey: ref,
				GitHubState: githubSHA, ForgejoState: forgejoSHA, CreatedAt: time.Now().UTC(),
			})
		}
	}

	return actions, conflicts, nil
}

func union(githubRefs, forgejoRefs map[string]string) []string {
	set := map[string]bool{}
	for ref := range githubRefs {
		set[ref] = true
	}
	for ref := range forgejoRefs {
		set[ref] = true
	}
	refs := make([]string, 0, len(set))
	for ref := range set {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}
