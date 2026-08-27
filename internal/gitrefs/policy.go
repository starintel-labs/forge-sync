package gitrefs

import (
	"fmt"
	"sort"
	"strings"
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

var developmentPrefixes = []string{
	"refs/heads/feature/",
	"refs/heads/fix/",
	"refs/heads/agent/",
	"refs/heads/rage/",
}

func Plan(repository string, githubRefs, forgejoRefs map[string]string, isAncestor Ancestry) ([]Action, []model.Conflict, error) {
	if repository == "" || isAncestor == nil {
		return nil, nil, fmt.Errorf("repository and ancestry checker are required")
	}
	var actions []Action
	var conflicts []model.Conflict

	for _, ref := range sortedKeys(githubRefs) {
		if !canonical(ref) {
			continue
		}
		sourceSHA := githubRefs[ref]
		targetSHA, exists := forgejoRefs[ref]
		if !exists {
			actions = append(actions, Action{Ref: ref, SHA: sourceSHA, From: GitHub, To: Forgejo})
			continue
		}
		if sourceSHA == targetSHA {
			continue
		}
		if strings.HasPrefix(ref, "refs/tags/") {
			conflicts = append(conflicts, refConflict(repository, ref, sourceSHA, targetSHA))
			continue
		}
		fastForward, err := isAncestor(targetSHA, sourceSHA)
		if err != nil {
			return nil, nil, fmt.Errorf("check ancestry for %s: %w", ref, err)
		}
		if fastForward {
			actions = append(actions, Action{Ref: ref, SHA: sourceSHA, From: GitHub, To: Forgejo})
		} else {
			conflicts = append(conflicts, refConflict(repository, ref, sourceSHA, targetSHA))
		}
	}

	for _, ref := range sortedKeys(forgejoRefs) {
		if !development(ref) {
			continue
		}
		sourceSHA := forgejoRefs[ref]
		targetSHA, exists := githubRefs[ref]
		if !exists {
			actions = append(actions, Action{Ref: ref, SHA: sourceSHA, From: Forgejo, To: GitHub})
			continue
		}
		if sourceSHA == targetSHA {
			continue
		}
		fastForward, err := isAncestor(targetSHA, sourceSHA)
		if err != nil {
			return nil, nil, fmt.Errorf("check ancestry for %s: %w", ref, err)
		}
		if fastForward {
			actions = append(actions, Action{Ref: ref, SHA: sourceSHA, From: Forgejo, To: GitHub})
		} else {
			conflicts = append(conflicts, refConflict(repository, ref, targetSHA, sourceSHA))
		}
	}

	return actions, conflicts, nil
}

func canonical(ref string) bool {
	return ref == "refs/heads/main" || ref == "refs/heads/master" || strings.HasPrefix(ref, "refs/tags/")
}

func development(ref string) bool {
	for _, prefix := range developmentPrefixes {
		if strings.HasPrefix(ref, prefix) && len(ref) > len(prefix) {
			return true
		}
	}
	return false
}

func sortedKeys(refs map[string]string) []string {
	keys := make([]string, 0, len(refs))
	for ref := range refs {
		keys = append(keys, ref)
	}
	sort.Strings(keys)
	return keys
}

func refConflict(repository, ref, githubSHA, forgejoSHA string) model.Conflict {
	return model.Conflict{
		Kind: "git-ref", Repository: repository, ObjectKey: ref,
		GitHubState: githubSHA, ForgejoState: forgejoSHA, CreatedAt: time.Now().UTC(),
	}
}
