package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// builderGoogle is `builder`'s issued variable.
const builderGoogle = "GOOGLE_CLOUD_ACCESS_TOKEN_BUILDER"

// googleCloudLane is the seat's lane as the Agents tab answers it, from the
// ledger as a converged sync left it, with settling recorded where it is set.
func googleCloudLane(t *testing.T, e *Engine, c *Company, settling *seataccess.Settling) seataccess.LaneAnswer {
	t.Helper()
	ledger, err := e.SeatAccessLedger()
	if err != nil {
		t.Fatal(err)
	}
	records, err := ledger.Records(t.Context(), integration.KindGoogleCloud)
	if err != nil {
		t.Fatal(err)
	}
	rec := records["builder"]
	want := seataccess.Desired{Enabled: true, Level: "viewer", Scope: []string{"acme-web"}}
	rec.Lanes = map[seataccess.Lane]seataccess.LaneRecord{seataccess.LaneGoogleCloud: {
		Applied: &want, Settling: settling,
		Observed: seataccess.Observed{Exists: true, Enabled: true, Account: rec.Account.Name,
			AccountID: rec.Account.ID, Level: "viewer", Scope: []string{"acme-web"}},
		Sync: &seataccess.SyncResult{At: time.Now(), Outcome: seataccess.OutcomeOK, Steps: []seataccess.StepOutcome{}},
	}}
	vendor, _ := seataccess.Lookup(integration.KindGoogleCloud)
	q := setupapi.AccessQuery{Kind: integration.KindGoogleCloud, Company: c.Config, Roster: c.Org}
	desired := map[seataccess.Lane]seataccess.Desired{seataccess.LaneGoogleCloud: want}
	sa, err := (&SeatAccessAPI{engine: e}).seatAnswer(q, vendor, false,
		seataccess.SeatRef{Handle: "builder"}, desired, rec, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	return sa.Lanes[seataccess.LaneGoogleCloud]
}

// A GOOGLE CLOUD SEAT IS SERVED BY THE PATHS EVERY ISSUED CREDENTIAL SHARES,
// end to end: its Monitoring server, recognised by its host, is started with
// the seat's own token as its bearer, issued at use and sealed nowhere; its
// box is handed the token file, which the poll keeps, and the settings that
// point gcloud at it; and the Agents tab reports the credential as issued,
// the box lines as Crewlet's, and the tool as live through the server alone.
func TestAGoogleCloudSeatThroughIssuingBoxAndTool(t *testing.T) {
	t.Parallel()
	g := newSeatGoogle(t)
	c := googleCloudCompany(t)
	e := googleCloudEngine(t, c, g, time.Now().Add(-time.Hour))
	builder := c.Org.AgentSeatByHandle("builder")

	// ISSUING: the server's bearer is the seat's own token.
	env, tokens := e.seatResolver(t.Context(), c, builder, toolTokenLife)
	specs, outcomes := seatSpecs(c, builder, env, &seatMint{kinds: e.mintedFor(c, "builder"), token: tokens})
	if len(specs) != 1 || len(outcomes) != 0 {
		t.Fatalf("specs = %+v, outcomes = %+v: want the Monitoring server started", specs, outcomes)
	}
	issued, ok := tokens.link(builderGoogle).issued()
	if !ok || !strings.HasPrefix(issued.Token, "ya29.seat-"+gcpBuilderID) ||
		specs[0].Headers["Authorization"] != "Bearer "+issued.Token {
		t.Fatalf("the server is started with %q, want the seat's issued token as its bearer",
			specs[0].Headers["Authorization"])
	}
	// NEVER SEALED: the company's chain, which every shared server and every
	// other seat reads, holds nothing under the seat's variable.
	if e.resolver().Value("${"+builderGoogle+"}") != "" {
		t.Error("the company's chain answers the seat's token")
	}

	// A SEAT WHOSE ACCOUNT IS NOT MADE YET waits, and is retried.
	planner := c.Org.AgentSeatByHandle("planner")
	env, tokens = e.seatResolver(t.Context(), c, planner, toolTokenLife)
	_, outcomes = seatSpecs(c, planner, env, &seatMint{kinds: e.mintedFor(c, "planner"), token: tokens})
	var missing *credentialMissing
	if len(outcomes) != 1 || !errors.As(outcomes[0].err, &missing) || !missing.waiting() || !tokens.settling() {
		t.Fatalf("outcomes = %+v: want the server waiting and retried", outcomes)
	}

	// THE BOX: the token file at launch, kept by the poll, and its settings.
	run := e.runEnvironment(t.Context(), c, builder, nil, nil, e.boxDefaults(c.Config, "builder"))
	if len(run.setup) == 0 || run.setup[0].Name != integrationFilesStep ||
		!strings.HasPrefix(run.setup[0].Files[googlecloud.TokenFile], "ya29.seat-") {
		t.Fatalf("setup = %+v, want the gcloud token file first", run.setup)
	}
	if run.homePaths[googlecloud.TokenFileEnv] != googlecloud.TokenFile ||
		run.defaults[googlecloud.ProjectEnv] != "acme-web" {
		t.Errorf("home paths %v, defaults %v: want gcloud pointed at its file and the seat's project",
			run.homePaths, run.defaults)
	}
	box := sandbox.NewFakeSandbox("box-1")
	box.Put(googlecloud.TokenFile, "ya29.expired")
	e.refreshTokenFiles(t.Context(), nil, sandbox.PendingRun{TurnID: "t", AgentHandle: "builder"}, box, takesEveryToken)
	if held, _ := box.ReadFile(t.Context(), googlecloud.TokenFile); !strings.HasPrefix(string(held), "ya29.seat-") {
		t.Errorf("the poll left %q in the gcloud token file", held)
	}

	// THE TOOL AND THE WIRE.
	lane := googleCloudLane(t, e, c, nil)
	if lane.Tool != seataccess.ToolLive || lane.ToolReason != "" {
		t.Errorf("the tool reads %s %q, want live through the Monitoring server", lane.Tool, lane.ToolReason)
	}
	wire, err := json.Marshal(lane)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"credential":{"variable":"` + builderGoogle + `","source":"issued"}`,
		`{"name":"` + googlecloud.TokenFileEnv + `","value":"~/` + googlecloud.TokenFile + `"}`,
		`{"name":"` + googlecloud.ProjectEnv + `","value":"acme-web"}`,
		`{"path":"` + googlecloud.TokenFile + `","value":"${` + builderGoogle + `}","rewritten":true}`,
	} {
		if !strings.Contains(string(wire), want) {
			t.Errorf("the lane's wire does not hold %s: %s", want, wire)
		}
	}
	settling := &seataccess.Settling{Reason: googlecloud.ReasonGranting, Until: time.Now().Add(time.Minute)}
	if lane := googleCloudLane(t, e, c, settling); lane.Tool != seataccess.ToolWaiting ||
		lane.ToolReason != googlecloud.ReasonGranting {
		t.Errorf("while Google applies the grant the tool reads %s %q", lane.Tool, lane.ToolReason)
	}
	// THE FILE IS NO TOOL: without a server the box's gcloud is not counted.
	c.Config.MCPServers = nil
	if lane := googleCloudLane(t, e, c, nil); lane.Tool != seataccess.ToolNoServer || lane.Box == nil {
		t.Errorf("with no server the tool reads %s with box %+v, want no_server beside the box lines",
			lane.Tool, lane.Box)
	}
}
