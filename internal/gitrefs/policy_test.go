package gitrefs_test

import (
	"errors"
	"testing"

	"github.com/starintel-labs/forge-sync/internal/gitrefs"
)

func TestCanonicalBranchFastForwardsGitHubToForgejo(t *testing.T) {
	t.Parallel()
	ancestry := graph(map[string][]string{"B": {"A"}})
	actions, conflicts, err := gitrefs.Plan("starintel-labs/example", map[string]string{"refs/heads/main": "B"}, map[string]string{"refs/heads/main": "A"}, ancestry)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 || len(actions) != 1 {
		t.Fatalf("actions=%#v conflicts=%#v", actions, conflicts)
	}
	if actions[0].From != gitrefs.GitHub || actions[0].To != gitrefs.Forgejo || actions[0].SHA != "B" {
		t.Fatalf("wrong action: %#v", actions[0])
	}
}

func TestCanonicalBranchDivergenceConflictsWithoutPush(t *testing.T) {
	t.Parallel()
	ancestry := graph(map[string][]string{"B": {"A"}, "C": {"A"}})
	actions, conflicts, err := gitrefs.Plan("starintel-labs/example", map[string]string{"refs/heads/main": "B"}, map[string]string{"refs/heads/main": "C"}, ancestry)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 || len(conflicts) != 1 {
		t.Fatalf("actions=%#v conflicts=%#v", actions, conflicts)
	}
	if conflicts[0].GitHubState != "B" || conflicts[0].ForgejoState != "C" {
		t.Fatalf("conflict lacks both SHAs: %#v", conflicts[0])
	}
}

func TestForgejoDevelopmentBranchPromotesToGitHub(t *testing.T) {
	t.Parallel()
	actions, conflicts, err := gitrefs.Plan("starintel-labs/example", nil, map[string]string{"refs/heads/feature/foo": "D"}, graph(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 || len(actions) != 1 || actions[0].From != gitrefs.Forgejo || actions[0].To != gitrefs.GitHub {
		t.Fatalf("actions=%#v conflicts=%#v", actions, conflicts)
	}
}

func TestForgejoOnlyDevelopmentBranchIsNeverDeleted(t *testing.T) {
	t.Parallel()
	actions, _, err := gitrefs.Plan("starintel-labs/example", map[string]string{"refs/heads/main": "A"}, map[string]string{
		"refs/heads/main": "A", "refs/heads/fix/local": "D", "refs/heads/personal": "E",
	}, graph(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Ref != "refs/heads/fix/local" {
		t.Fatalf("unexpected actions: %#v", actions)
	}
}

func TestChangedTagAlwaysConflicts(t *testing.T) {
	t.Parallel()
	actions, conflicts, err := gitrefs.Plan("starintel-labs/example", map[string]string{"refs/tags/v1": "B"}, map[string]string{"refs/tags/v1": "A"}, graph(map[string][]string{"B": {"A"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 || len(conflicts) != 1 {
		t.Fatalf("actions=%#v conflicts=%#v", actions, conflicts)
	}
}

func TestAncestryFailureAbortsPlanning(t *testing.T) {
	t.Parallel()
	_, _, err := gitrefs.Plan("starintel-labs/example", map[string]string{"refs/heads/main": "B"}, map[string]string{"refs/heads/main": "A"}, func(string, string) (bool, error) {
		return false, errors.New("missing object")
	})
	if err == nil {
		t.Fatal("ancestry failure was ignored")
	}
}

func graph(parents map[string][]string) gitrefs.Ancestry {
	return func(ancestor, descendant string) (bool, error) {
		if ancestor == descendant {
			return true, nil
		}
		seen := map[string]bool{}
		queue := []string{descendant}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			if seen[current] {
				continue
			}
			seen[current] = true
			for _, parent := range parents[current] {
				if parent == ancestor {
					return true, nil
				}
				queue = append(queue, parent)
			}
		}
		return false, nil
	}
}
