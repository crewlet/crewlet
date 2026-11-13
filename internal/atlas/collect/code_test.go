package collect_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
)

// fakeHost is a source that also hosts repositories.
type fakeHost struct{ fakeSource }

func (*fakeHost) Kind() integration.Kind { return integration.KindGitHub }

func (*fakeHost) OpenCode(context.Context, collect.Input) (collect.CodeSession, error) {
	return nil, nil
}

// fakePass is a code pass that says when it is due and records what the duty
// asked of it.
type fakePass struct {
	due   bool
	asked []string
}

func (p *fakePass) Due(_ context.Context, kind integration.Kind, _ time.Duration, _ time.Time) bool {
	return p.due
}

func (p *fakePass) Pass(ctx context.Context, host collect.CodeHost, in collect.Input, now time.Time) {
	_, bounded := ctx.Deadline()
	if in.Config == nil || !in.Now.Equal(now) || !bounded {
		p.asked = append(p.asked, "a pass without the document, the read's instant or a deadline")
		return
	}
	p.asked = append(p.asked, "pass "+host.Kind().String())
}

func (p *fakePass) Forget(_ context.Context, kind integration.Kind) {
	p.asked = append(p.asked, "forget "+kind.String())
}

// A FORGE'S REPOSITORIES ARE READ AS A VISIT OF THEIR OWN: the duty asks the
// pass about every connected source that hosts code, after the source's own
// read and whether or not that read was due; it runs the pass only while it
// holds the duty, which it claims again for it; a source that hosts none is
// never asked; and a forge the company no longer connects is forgotten.
//
// Mutation: run the pass only when the source's own read is due, and a pass
// that left work behind waits out the hour instead of five minutes.
func TestAForgesRepositoriesAreReadAsAVisitOfTheirOwn(t *testing.T) {
	t.Parallel()
	store, err := collect.NewCoordStore(memory.NewFleet())
	if err != nil {
		t.Fatal(err)
	}
	company := &config.Company{Name: "acme"}
	company.Integrations.GitHub = &config.GitHub{Enabled: true, Provisioning: &config.GitHubProvisioning{Org: "acme"}}
	company.Integrations.Cloudflare = &config.Cloudflare{Enabled: true, Provisioning: &config.CloudflareProvisioning{
		AccountID: "0123456789abcdef0123456789abcdef", APIToken: "${CF_TOKEN}"}}
	host, plain, pass := &fakeHost{}, &fakeSource{}, &fakePass{due: true}
	clock, held, claims := t0, true, 0
	worker, err := collect.New(collect.Options{
		Sources:  []collect.Source{plain, host},
		Store:    store,
		Log:      newFakeLog(),
		Input:    func(context.Context) (collect.Input, bool) { return collect.Input{Config: company}, true },
		Interval: func() time.Duration { return time.Hour },
		ClaimDuty: func(context.Context) (bool, error) {
			claims++
			return held, nil
		},
		Code: pass,
		Now:  func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	tick := func(after time.Duration) {
		clock = clock.Add(after)
		worker.Tick(t.Context())
	}
	// Both sources are due and read; the forge's pass follows its read.
	tick(0)
	if !slices.Equal(pass.asked, []string{"pass github"}) || host.reads != 1 || plain.reads != 1 || claims != 4 {
		t.Fatalf("the first tick asked %v after %d and %d reads and %d claims", pass.asked, host.reads, plain.reads, claims)
	}
	// Five minutes on, no source is due and the pass is: it runs alone.
	tick(5 * time.Minute)
	if len(pass.asked) != 2 || host.reads != 1 {
		t.Fatalf("a pass due between the source's reads: %v after %d reads", pass.asked, host.reads)
	}
	// A pass that is not due is not run, and one this node may not run is not.
	pass.due = false
	tick(time.Minute)
	pass.due, held = true, false
	tick(time.Minute)
	if len(pass.asked) != 2 {
		t.Fatalf("a pass ran that was not due or not this node's: %v", pass.asked)
	}
	// The company disconnects the forge: what its files declared is withdrawn.
	held = true
	company.Integrations.GitHub = nil
	tick(time.Minute)
	if got := pass.asked[len(pass.asked)-1]; got != "forget github" {
		t.Fatalf("a disconnected forge was asked %v", pass.asked)
	}
}
