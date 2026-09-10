package mattermost_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// chatReconciler drives the REAL pass through the [integration.Reconciler]
// seam, shaped exactly like the engine's own adapter.
//
// The adapter lives here rather than in the engine because that is the only
// place a converged WORLD can be stood up: engine.mattermostPass on a bare
// Engine falls back to a read-only sink, [provision.CanMint] answers false,
// and [mattermost.Reconcile] returns before its first HTTP request — so
// every clause below would pass over a reconciler nothing ever reached,
// which is the exact shape this suite was written to replace.
type chatReconciler struct {
	client *mattermost.Client
	cfg    *config.Mattermost
	org    *org.Organization
	sink   provision.TokenSink
}

func (*chatReconciler) Kind() integration.Kind { return integration.KindMattermost }

func (r *chatReconciler) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := r.pass(ctx)
	if err != nil {
		return nil, err
	}
	return res.Findings(), nil
}

// pass is one reconcile, with the Result the suite's seam throws away.
//
// THE PLAN IS REBUILT ON EVERY PASS, exactly as engine.mattermostPass builds
// it: registration is static and the company document is edited live, so a
// pass reads the company as it is at that moment. A harness holding one plan
// across passes would also hold the notes a run appends to it, and the second
// pass would report the first one's caveats as its own.
func (r *chatReconciler) pass(ctx context.Context) (*mattermost.Result, error) {
	plan, err := mattermost.PlanFor(r.org, r.cfg)
	if err != nil {
		return nil, err
	}
	return mattermost.Reconcile(ctx, mattermost.Options{
		Client: r.client, Config: r.cfg, Org: r.org, Plan: plan, Sink: r.sink,
	})
}

// THE CONTRACT BINDS HERE, over a real Mattermost pass and a world it has
// itself converged.
//
// The world is converged BY A REAL RUN rather than by hand-seeding the
// fixture, which is the only way to be sure it is the state this pass
// actually leaves behind: a hand-built world can be converged in a way the
// provisioner never produces, and then the clause that matters — a second
// pass writes nothing — is answered about a world nobody runs.
//
// Neither Rotate nor Decommission is set, because the engine's pass sets
// neither: both are deliberate command-line gestures that take working
// agents down, and certifying the loop means certifying what the loop runs.
func TestTheMattermostReconcilerMeetsTheContract(t *testing.T) {
	t.Parallel()
	var srv *chatServer
	var sink *chatSink

	integrationtest.Run(t, integrationtest.Reconciler{
		New: func(tb integrationtest.TB) integration.Reconciler {
			tb.Helper()
			srv, sink = newChatServer(), newChatSink()
			rec := &chatReconciler{
				// The OUTER t, because integrationtest.TB has no Cleanup:
				// the suite calls New once per case, so the servers stand
				// until this test function returns.
				client: chatClient(t, srv),
				cfg:    enabledChat(),
				org: &org.Organization{Name: "Nimbus", Roles: []*org.Role{
					chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
				}},
				sink: sink,
			}

			res, err := rec.pass(context.Background())
			if err != nil {
				tb.Fatalf("converging the world: %v", err)
			}
			// AND IT REALLY CONVERGED IT, asserted rather than assumed.
			// A plan that admitted no seat, a sink that cannot mint or a
			// fixture route answering 404 all leave a pass that made no
			// request at all — and a suite driven against that certifies
			// nothing while reporting green.
			if len(res.Created) != 1 || len(res.Rotated) != 1 {
				tb.Fatalf("the converging pass provisioned nothing: %+v", res)
			}
			if got := strings.Join(res.Joined["ceo"], ","); got != "general,leadership" {
				tb.Fatalf("the converging pass joined %q, so the world it left "+
					"is not the one a converged pass would read", got)
			}
			if sink.value("MM_TOKEN_CEO") == "" {
				tb.Fatalf("%s", "the converging pass sealed no credential, so the "+
					"next pass has nothing to verify and would mint again")
			}
			return rec
		},

		// BOTH ESTATES. A write is anything a person would have to undo,
		// and this pass can write in two places: at the instance, and
		// into this deployment's own sealed store. Counting only the
		// first would miss a pass that re-seals a working credential on
		// every run — which is the very failure Options.Rotate exists to
		// keep deliberate.
		Mutations: func() int { return srv.mutations() + sink.mutations() },
	})
}
