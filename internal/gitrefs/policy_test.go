package gitrefs_test

import (
	"errors"
	"testing"

	"github.com/starintel-labs/forge-sync/internal/gitrefs"
)

func TestGitHubAheadBranchFastForwardsIntoForgejo(t *testing.T) {
	t.Parallel()
	ancestry := graph(map[string][]string{"B": {"A"}})
	actions, conflicts, err := gitrefs.Plan("starintel-labs/example", map[string]string{"refs/heads/main": "B"}, map[string]string{"refs/heads/main": "A"}, ancestry)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 || len(actions) != 1 {
		t.Fatalf("actions=%#v conflicts=%#v", actions, conflicts)
	}
	if actions[0].From != gitrefs.GitHub || actions[0].To != gitrefs.Forgejo || actions[0].SHA != "B" || actions[0].Force {
		t.Fatalf("wrong action: %#v", actions[0])
	}
}

func TestForgejoAheadBranchFastForwardsGitHub(t *testing.T) {
	t.Parallel()
	ancestry := graph(map[string][]string{"B": {"A"}})
	actions, conflicts, err := gitrefs.Plan("starintel-labs/example", map[string]string{"refs/heads/main": "A"}, map[string]string{"refs/heads/main": "B"}, ancestry)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 || len(actions) != 1 {
		t.Fatalf("actions=%#v conflicts=%#v", actions, conflicts)
	}
	if actions[0].From != gitrefs.Forgejo || actions[0].To != gitrefs.GitHub || actions[0].SHA != "B" || actions[0].Force {
		t.Fatalf("wrong action: %#v", actions[0])
	}
}

func TestDivergenceEnforcesForgejoMaster(t *testing.T) {
	t.Parallel()
	ancestry := graph(map[string][]string{"B": {"A"}, "C": {"A"}})
	actions, conflicts, err := gitrefs.Plan("starintel-labs/example", map[string]string{"refs/heads/main": "B"}, map[string]string{"refs/heads/main": "C"}, ancestry)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || len(conflicts) != 1 {
		t.Fatalf("actions=%#v conflicts=%#v", actions, conflicts)
	}
	if actions[0].From != gitrefs.Forgejo || actions[0].To != gitrefs.GitHub || actions[0].SHA != "C" || !actions[0].Force {
		t.Fatalf("master enforcement action missing: %#v", actions[0])
	}
	if conflicts[0].Kind != "git-ref-override" || conflicts[0].GitHubState != "B" || conflicts[0].ForgejoState != "C" {
		t.Fatalf("override record lacks both SHAs: %#v", conflicts[0])
	}
}

func TestForgejoOnlyBranchMirrorsToGitHub(t *testing.T) {
	t.Parallel()
	actions, conflicts, err := gitrefs.Plan("starintel-labs/example", map[string]string{"refs/heads/main": "A"}, map[string]string{
		"refs/heads/main": "A", "refs/heads/feature/foo": "D", "refs/heads/personal": "E",
	}, graph(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 || len(actions) != 2 {
		t.Fatalf("actions=%#v conflicts=%#v", actions, conflicts)
	}
	for _, action := range actions {
		if action.From != gitrefs.Forgejo || action.To != gitrefs.GitHub {
			t.Fatalf("unexpected action: %#v", action)
		}
	}
}

func TestGitHubOnlyBranchIsPulledIntoForgejo(t *testing.T) {
	t.Parallel()
	actions, conflicts, err := gitrefs.Plan("starintel-labs/example", map[string]string{
		"refs/heads/main": "A", "refs/heads/agent/x": "F",
	}, map[string]string{"refs/heads/main": "A"}, graph(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 || len(actions) != 1 {
		t.Fatalf("actions=%#v conflicts=%#v", actions, conflicts)
	}
	if actions[0].From != gitrefs.GitHub || actions[0].To != gitrefs.Forgejo || actions[0].Ref != "refs/heads/agent/x" {
		t.Fatalf("unexpected action: %#v", actions[0])
	}
}

func TestFastForwardedTagMirrorsToGitHub(t *testing.T) {
	t.Parallel()
	actions, conflicts, err := gitrefs.Plan("starintel-labs/example", map[string]string{"refs/tags/v1": "A"}, map[string]string{"refs/tags/v1": "B"}, graph(map[string][]string{"B": {"A"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 || len(actions) != 1 {
		t.Fatalf("actions=%#v conflicts=%#v", actions, conflicts)
	}
	if actions[0].From != gitrefs.Forgejo || actions[0].To != gitrefs.GitHub || actions[0].Force {
		t.Fatalf("unexpected action: %#v", actions[0])
	}
}

func TestDivergedTagEnforcesForgejoMaster(t *testing.T) {
	t.Parallel()
	actions, conflicts, err := gitrefs.Plan("starintel-labs/example", map[string]string{"refs/tags/v1": "B"}, map[string]string{"refs/tags/v1": "C"}, graph(map[string][]string{"B": {"A"}, "C": {"A"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || len(conflicts) != 1 {
		t.Fatalf("actions=%#v conflicts=%#v", actions, conflicts)
	}
	if !actions[0].Force || actions[0].SHA != "C" {
		t.Fatalf("unexpected action: %#v", actions[0])
	}
	if conflicts[0].Kind != "git-ref-override" || conflicts[0].GitHubState != "B" || conflicts[0].ForgejoState != "C" {
		t.Fatalf("override record lacks both SHAs: %#v", conflicts[0])
	}
}

func TestIdenticalRefsAreNoOp(t *testing.T) {
	t.Parallel()
	refs := map[string]string{"refs/heads/main": "A", "refs/tags/v1": "T"}
	actions, conflicts, err := gitrefs.Plan("starintel-labs/example", refs, refs, graph(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 || len(conflicts) != 0 {
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
