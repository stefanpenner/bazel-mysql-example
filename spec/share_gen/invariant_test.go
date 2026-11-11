package sharespec

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestShareInvariantsHold(t *testing.T) {
	domain := []string{"t1", "t2"}
	start := Init()
	seen := map[string]bool{stateKey(start): true}
	queue := []State{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if !currentMatches(cur) {
			t.Fatalf("current does not match the server: %+v", cur)
		}
		if !holdersDownMeansStopped(cur) {
			t.Fatalf("holders see a down server that is still current: %+v", cur)
		}
		for _, step := range cur.EnabledSteps(domain) {
			next := cur.ApplyStep(step)
			key := stateKey(next)
			if seen[key] {
				continue
			}
			seen[key] = true
			queue = append(queue, next)
		}
	}
	if len(seen) < 2 {
		t.Fatalf("explored %d states", len(seen))
	}
}

func TestCanStopMutationRefusesHeldStart(t *testing.T) {
	s := Init().LockStart("t1").Create("t1").Release("t1").BeginGrace()
	if !s.CanStop() {
		t.Fatal("grace with no holder must stop")
	}
	bad := s
	bad.StartLock = "t2"
	if bad.CanStop() {
		t.Fatal("held start.lock must refuse stop")
	}
	bad = s
	bad.Holders = map[string]bool{"t2": true}
	if bad.CanStop() {
		t.Fatal("a holder must refuse stop")
	}
}

func currentMatches(s State) bool {
	run := s.Current == "run"
	up := s.Server == "up" || s.Supervisor == "dead"
	return run == up
}

func holdersDownMeansStopped(s State) bool {
	if len(s.Holders) == 0 || s.Server != "down" {
		return true
	}
	return s.Current == "none" && s.Supervisor == "absent"
}

func stateKey(s State) string {
	holders := make([]string, 0, len(s.Holders))
	for name, in := range s.Holders {
		if in {
			holders = append(holders, name)
		}
	}
	sort.Strings(holders)
	return fmt.Sprintf("%s|%s|%s|%s|%s", s.StartLock, strings.Join(holders, ","), s.Server, s.Current, s.Supervisor)
}
