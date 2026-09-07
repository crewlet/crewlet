package github_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
)

func kinds(findings []integration.Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, string(f.Kind))
	}
	return out
}

// A SEAT WITH NO APP IS A STEP OUTSTANDING, NOT A FAULT.
//
// Nobody has done anything wrong: there is a click left. The finding carries
// no action URL, because an app is created by POSTing a manifest from a page
// and there is no address to send anybody to.
func TestASeatWithNoAppReportsTheClickThatIsLeft(t *testing.T) {
	t.Parallel()
	res, err := github.ReconcileSeatApps(context.Background(), github.SeatAppOptions{
		Seats: []github.SeatApp{{Handle: "sre-lead", Tier: github.TierReview}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(res.Findings); len(got) != 1 || got[0] != string(integration.FindingIdentityMissing) {
		t.Fatalf("findings = %v", got)
	}
	if url := res.Findings[0].ActionURL; url != "" {
		t.Errorf("creating an app was given an address to follow: %q", url)
	}
	if len(res.Ready) != 0 {
		t.Errorf("a seat with no app reported ready: %v", res.Ready)
	}
}

// THE KEY IS ISSUED ONCE AND NEVER REISSUED, so an unresolved one is not a
// credential to re-enter: the app has to be created again, and the sentence
// has to say so or an operator hunts for a value that does not exist.
func TestAnUnresolvedKeySaysTheAppMustBeCreatedAgain(t *testing.T) {
	t.Parallel()
	res, err := github.ReconcileSeatApps(context.Background(), github.SeatAppOptions{
		Seats: []github.SeatApp{{Handle: "sre-lead", AppID: 7, Tier: github.TierReview}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(res.Findings); len(got) != 1 || got[0] != string(integration.FindingCredentialMissing) {
		t.Fatalf("findings = %v", got)
	}
	if detail := res.Findings[0].Detail; !strings.Contains(detail, "create it again") {
		t.Errorf("the finding does not say the app must be recreated: %q", detail)
	}
}

// A TIER IS A WORD IN A DOCUMENT UNTIL THE INSTALLATION IS READ BACK.
//
// A manifest can be edited before it is submitted and an installation widened
// afterwards, so what the app HOLDS is compared against what the tier asks
// for, in both directions.
func TestTheInstallationIsComparedAgainstTheTierInBothDirections(t *testing.T) {
	t.Parallel()
	held := map[string]string{
		"metadata": "read", "contents": "read", "issues": "read",
		"administration": "write",
	}
	if excess := github.Excess(held); len(excess) != 1 || excess[0] != "administration" {
		t.Fatalf("Excess = %v, want the denied grant", excess)
	}
	// A review seat cannot write what it was set up to write, and GitHub
	// would only say so at the call site.
	short := github.Shortfall(github.TierReview, held)
	if len(short) == 0 {
		t.Fatal("a review seat on a read-only installation reports no shortfall")
	}
	for _, want := range []string{"issues", "pull_requests"} {
		if !strings.Contains(strings.Join(short, ","), want) {
			t.Errorf("the shortfall omits %q: %v", want, short)
		}
	}
}

// MINTING NEEDS AN INSTALLED APP, and saying so beats a request GitHub
// refuses: an app nobody installed sees no repository, so the token endpoint
// has nothing to scope to.
func TestMintingRefusesASeatWithNoInstallation(t *testing.T) {
	t.Parallel()
	_, err := github.TokenFor(context.Background(), github.SeatAppOptions{}, github.SeatApp{
		Handle: "sre-lead", AppID: 7,
	})
	if err == nil {
		t.Fatal("a seat with no installation minted a token")
	}
	if !strings.Contains(err.Error(), "no installed app") {
		t.Errorf("the refusal does not name the cause: %v", err)
	}
}

// A TEARDOWN OF SOMETHING ALREADY GONE IS THE OUTCOME ASKED FOR. Treating it
// as a failure would leave the record behind for ever, retried on every pass.
func TestUninstallingWhatIsAlreadyGoneSucceeds(t *testing.T) {
	t.Parallel()
	err := github.UninstallSeat(context.Background(), github.SeatAppOptions{}, github.SeatApp{
		Handle: "sre-lead",
	})
	if err != nil {
		t.Errorf("uninstalling a seat with no app failed: %v", err)
	}
}

// SEATS ARE REPORTED IN A STABLE ORDER, so two passes name the same seat
// first and a status line does not change every tick.
func TestSeatsAreOrderedSoAStatusLineIsStable(t *testing.T) {
	t.Parallel()
	got := github.SeatsFrom([]github.SeatApp{{Handle: "zeta"}, {Handle: "alpha"}, {Handle: "mid"}})
	if got[0].Handle != "alpha" || got[2].Handle != "zeta" {
		t.Errorf("order = %v", []string{got[0].Handle, got[1].Handle, got[2].Handle})
	}
}
