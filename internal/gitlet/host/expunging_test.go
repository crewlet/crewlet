package host_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/host"
)

// A PUSH CARRIES THE DROP EPOCH ITS COPY WAS SYNCED AT (cache.Synced.DropEpoch)
// and its committer's refusal of another reaches git whole: while the cache's
// snapshots say drop epoch 7 and the rows 0, the proposal carries 7, the
// committer refuses it expunging with no push row, git prints the refusal's
// sentence on the ref and the audit records it refused under its code; once
// the snapshots match the rows, the same push lands.
//
// Mutation: propose without the synced copy's drop epoch, and the first push
// lands.
func TestAPushCarriesTheDropEpochItsCopyWasSyncedAt(t *testing.T) {
	var (
		mu      sync.Mutex
		skew    int64 = 7
		carried []int64
	)
	e := &recording{
		state: func(ctx context.Context, inner host.Estate, q gitlet.StateQuery) (gitlet.RepoSnapshot, error) {
			snap, err := inner.RepoState(ctx, q)
			if err != nil {
				return snap, err
			}
			mu.Lock()
			defer mu.Unlock()
			snap.Repo.DropEpoch += skew
			return snap, nil
		},
		propose: func(ctx context.Context, inner host.Estate, p gitlet.Proposal) (gitlet.ProposeResult, error) {
			mu.Lock()
			carried = append(carried, p.DropEpoch)
			mu.Unlock()
			return inner.Propose(ctx, p)
		},
	}
	c := newCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{Timing: fast, WrapEstate: e.wrap}},
		gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")

	res := p.Push(c.Ctx, work, c.URL(0), "main")
	reason, rejected := res.Rejected("refs/heads/main")
	want := "(expunging: " + gitlet.ExpungingDetail(gitlettest.RepoName, 7, 0) + ")"
	if !rejected || !strings.Contains(reason, want) {
		t.Fatalf("a push checked against a copy at drop epoch 7 printed %q, want %q: %s", reason, want, res)
	}
	if rows := pushRows(t, c); len(rows) != 0 {
		t.Fatalf("a push refused expunging left %d push rows", len(rows))
	}
	audited := c.Fleet.Audited()
	if last := audited[len(audited)-1]; last.Outcome != types.AuditRefused ||
		last.Refusal != string(gitlet.RefusalExpunging) {
		t.Errorf("a push refused expunging was recorded %s/%s, want refused/expunging", last.Outcome,
			last.Refusal)
	}

	mu.Lock()
	skew = 0
	mu.Unlock()
	if res := p.Push(c.Ctx, work, c.URL(0), "main"); !res.OK("refs/heads/main") {
		t.Fatalf("the push checked against a copy at the rows' epoch: %s", res)
	}
	mu.Lock()
	got := slices.Clone(carried)
	mu.Unlock()
	if !slices.Equal(got, []int64{7, 0}) {
		t.Fatalf("the pushes carried drop epochs %v, want 7 and then 0", got)
	}
	if at := p.LsRemote(c.URL(0), 0)["refs/heads/main"]; at != head {
		t.Errorf("ls-remote shows main at %q, want %s", at, head)
	}
}
