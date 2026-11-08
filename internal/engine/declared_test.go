package engine_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
)

// IN A TEST, GITLET IS SIZED BY ITS CASE: a test's Gitlet node whose company
// hosts code refuses to build Gitlet with gitlet.cache_max_bytes or
// gitlet.max_transfers unset, naming each one it lacks, brokertest.GitletCacheIn and
// the one way out — and builds it with both declared. A node booted under
// engine.SizedByTheMachine passes the check, its case being the derivation
// itself, so only the volume's own room may still refuse it there — and its
// broker may derive its limit too, the context carrying jetstream's own
// opt-out; and a node whose company hosts no code builds no host and is not
// asked.
//
// Mutations: build Gitlet without the check (the undeclared rows
// boot); ask for the budget alone (the lane's row boots); ignore the opt-out
// (the derivation rows are refused); leave jetstream's opt-out off the context
// SizedByTheMachine makes (the broker's row is refused at the broker).
func TestATestsGitletIsSizedByItsCase(t *testing.T) {
	t.Parallel()
	gittest.RequireNode(t)
	undeclared := func(b *config.Bootstrap) { b.Gitlet.CacheMaxBytes, b.Gitlet.MaxTransfers = 0, 0 }
	derivedBroker := func(b *config.Bootstrap) { undeclared(b); b.Stream.StoreMaxBytes = 0 }
	for _, tc := range []struct {
		name     string
		unset    func(*config.Bootstrap)
		machine  bool
		doc      string
		refusing []string
	}{
		{"neither declared", undeclared, false, gitCompany,
			[]string{"gitlet.cache_max_bytes", "gitlet.max_transfers"}},
		{"the budget unset", func(b *config.Bootstrap) { b.Gitlet.CacheMaxBytes = 0 }, false, gitCompany,
			[]string{"gitlet.cache_max_bytes"}},
		{"the lane unset", func(b *config.Bootstrap) { b.Gitlet.MaxTransfers = 0 }, false, gitCompany,
			[]string{"gitlet.max_transfers"}},
		{"both declared", nil, false, gitCompany, nil},
		{"a case about the derivation", undeclared, true, gitCompany, nil},
		{"a case about the derivation, its broker's limit too", derivedBroker, true, gitCompany, nil},
		{"a company that hosts no code", undeclared, false, noCodeCompany, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := gitletNodeBootstrap(t, nil)
			if tc.unset != nil {
				tc.unset(b)
			}
			ctx := t.Context()
			if tc.machine {
				ctx = engine.SizedByTheMachine(ctx, "this case is about the figures a Gitlet node derives")
			}
			e, err := engine.New(ctx, engine.Options{
				Bootstrap: b, Company: parsedCompany(t, tc.doc),
				ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
				GitHook:     codetest.HookCommand(t),
			})
			if err == nil {
				t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })
			}
			switch {
			case tc.refusing == nil && (errors.Is(err, engine.ErrUndeclaredCodeCache) ||
				errors.Is(err, jetstream.ErrUndeclaredSize)):
				t.Fatalf("refused as undeclared: %v", err)
			case tc.refusing == nil && tc.machine:
				// THE ROOM MAY STILL REFUSE IT: the derived budget is the
				// machine's, which is this case's subject and not its
				// assertion.
			case tc.refusing == nil && err != nil:
				t.Fatalf("a node with Gitlet's figures declared, or hosting no code, refused: %v", err)
			case tc.refusing != nil && !errors.Is(err, engine.ErrUndeclaredCodeCache):
				t.Fatalf("Gitlet with %s unset answered %v, want engine.ErrUndeclaredCodeCache",
					strings.Join(tc.refusing, " and "), err)
			case tc.refusing != nil:
				for _, want := range append(slices.Clone(tc.refusing), "brokertest.GitletCacheIn",
					"engine.SizedByTheMachine") {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the refusal does not name %s: %v", want, err)
					}
				}
				for _, field := range []string{"gitlet.cache_max_bytes", "gitlet.max_transfers"} {
					if !slices.Contains(tc.refusing, field) && strings.Contains(err.Error(), field) {
						t.Errorf("the refusal names %s, which the case declared: %v", field, err)
					}
				}
			}
		})
	}
}
