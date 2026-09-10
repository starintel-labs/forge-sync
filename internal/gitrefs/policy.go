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
	Ref  string
	SHA  string
	From Forge
	To   Forge
}

type Ancestry func(ancestor, descendant string) (bool, error)

// Plan derives the reconciliation actions for every branch and tag ref that
// exists on either forge. Sync is truly master-to-master: a ref that is
// behind on one forge fast-forwards from the other, in whichever direction
// that is, and neither forge is privileged. On true divergence no mutating
// action is taken in either direction; the divergence is recorded as a
// conflict carrying both SHAs so a human can resolve it, after which normal
// fast-forward sync converges the other side. Refs are never deleted and
// history is never force-overwritten on either forge.
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
			conflicts = append(conflicts, model.Conflict{
				Kind: "git-ref-divergence", Repository: repository, ObjectKey: ref,
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
