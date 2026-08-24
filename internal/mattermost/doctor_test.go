package mattermost_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/org"
)

// dials records what the doctor asked for and answers how a test wants.
type dials struct {
	origins []string
	tokens  []string
	// refuse answers this error for a dial carrying this origin, which
	// is how a real server refuses a mismatched one.
	refuseOrigin string
	// refuseToken answers this error for a dial carrying this token.
	refuseToken string
}

func (d *dials) dial(_ context.Context, _, origin, token string) error {
	d.origins = append(d.origins, origin)
	d.tokens = append(d.tokens, token)
	if d.refuseOrigin != "" && origin == d.refuseOrigin {
		return errors.New("unexpected HTTP response: 403 Forbidden")
	}
	if d.refuseToken != "" && token == d.refuseToken {
		return errors.New("unexpected HTTP response: 401 Unauthorized")
	}
	return nil
}

func doctorAgainst(t *testing.T, srv *chatServer, mutate func(*mattermost.DoctorOptions)) *mattermost.Report {
	t.Helper()
	report, err := doctorRun(t, srv, mutate)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	return report
}

func doctorRun(t *testing.T, srv *chatServer, mutate func(*mattermost.DoctorOptions)) (*mattermost.Report, error) {
	t.Helper()
	client := chatClient(t, srv)
	cfg := enabledChat()
	cfg.URL = client.URL()
	opts := mattermost.DoctorOptions{
		Client: client, Config: cfg,
		Dial: (&dials{}).dial,
	}
	if mutate != nil {
		mutate(&opts)
	}
	return mattermost.Doctor(context.Background(), opts)
}

func finding(t *testing.T, report *mattermost.Report, check string) mattermost.Finding {
	t.Helper()
	for _, f := range report.Findings {
		if f.Check == check {
			return f
		}
	}
	t.Fatalf("no finding %q in %+v", check, report.Findings)
	return mattermost.Finding{}
}

// A SITE URL THAT DOES NOT MATCH BLINDS EVERY HUMAN while agents keep
// working — which is why this command exists: the failure has no symptom
// anybody reports.
func TestAMismatchedSiteURLIsReportedAsBlindingHumans(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.siteURL = "https://chat.internal.example.com"
	report := doctorAgainst(t, srv, nil)
	f := finding(t, report, "site url")
	if f.OK {
		t.Fatalf("a mismatched SiteURL passed: %+v", f)
	}
	if !strings.Contains(f.Detail, "live feed") {
		t.Errorf("the detail does not say what breaks: %q", f.Detail)
	}
	if report.Healthy() {
		t.Error("the report reads healthy with a mismatched SiteURL")
	}
}

// A MATCHING SITE URL PASSES, including when it differs in case or a
// trailing slash: this check exists to tell an operator their humans are
// blind, so a false alarm is worse than useless.
func TestAMatchingSiteURLPasses(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		srv.siteURL = strings.ToUpper(o.Config.URL[:4]) + o.Config.URL[4:] + "/"
	})
	if f := finding(t, report, "site url"); !f.OK {
		t.Errorf("a matching SiteURL failed: %q", f.Detail)
	}
}

// AN UNSET SITE URL IS THE SAME FAILURE and names where to set it.
func TestAnUnsetSiteURLIsReported(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.siteURL = " "
	report := doctorAgainst(t, srv, nil)
	f := finding(t, report, "site url")
	if f.OK || !strings.Contains(f.Detail, "System Console") {
		t.Errorf("finding = %+v", f)
	}
}

// A PATH MISMATCH IS ITS OWN FINDING: the socket works, so reporting it as
// a socket problem would send an operator to fix something that is fine.
func TestAPathMismatchIsNotReportedAsASocketProblem(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		srv.siteURL = o.Config.URL + "/chat"
	})
	f := finding(t, report, "site url")
	if f.OK {
		t.Fatal("a path mismatch passed")
	}
	if !strings.Contains(f.Detail, "websockets work") {
		t.Errorf("the detail reads as a socket failure: %q", f.Detail)
	}
}

// THE UPGRADE IS SHAPED EXACTLY AS A BROWSER'S. A probe sending no Origin,
// or one carrying a path, passes a check every real browser fails.
func TestTheBrowserProbeSendsTheOriginABrowserWouldSend(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	d := &dials{}
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) { o.Dial = d.dial })
	if f := finding(t, report, "browser socket"); !f.OK {
		t.Fatalf("browser socket = %q", f.Detail)
	}
	if len(d.origins) == 0 {
		t.Fatal("nothing was dialled")
	}
	origin := d.origins[0]
	if origin == "" {
		t.Fatal("the probe sent no Origin, which every real browser sends")
	}
	if strings.Count(origin, "/") != 2 {
		t.Errorf("the Origin carries a path: %q — an Origin never does, and "+
			"the server compares this string exactly", origin)
	}
}

// A REFUSED UPGRADE IS THE FINDING THAT MATTERS, because the real symptom
// is a chat client that just seems slow.
func TestARefusedBrowserUpgradeIsReported(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Dial = (&dials{refuseOrigin: mattermost.BrowserOrigin(o.Config.URL)}).dial
	})
	f := finding(t, report, "browser socket")
	if f.OK {
		t.Fatal("a refused upgrade passed")
	}
	if !strings.Contains(f.Detail, "silent") {
		t.Errorf("the detail does not say the failure is silent: %q", f.Detail)
	}
}

// EACH SEAT IS DIALLED WITH ITS OWN CREDENTIAL: "the server accepts
// sockets" and "this bot wakes" are different questions, and only the
// second one delivers a message.
func TestEachSeatIsDialledWithItsOwnToken(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	d := &dials{}
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Dial = d.dial
		o.Org = &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			chatSeat("SWE", "${MM_TOKEN_SWE}", "eng"),
			chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
		}}
		o.SeatToken = func(seat *org.Role) (string, bool) {
			return "tok-" + seat.Handle(), true
		}
		srv.issue("user-swe", "tok-swe")
		srv.issue("user-ceo", "tok-ceo")
	})
	for _, handle := range []string{"swe", "ceo"} {
		if f := finding(t, report, "seat "+handle); !f.OK {
			t.Errorf("seat %s = %q", handle, f.Detail)
		}
	}
	var sawSWE, sawCEO bool
	for _, token := range d.tokens {
		sawSWE = sawSWE || token == "tok-swe"
		sawCEO = sawCEO || token == "tok-ceo"
	}
	if !sawSWE || !sawCEO {
		t.Errorf("the seats were not dialled with their own tokens: %v", d.tokens)
	}
}

// A SEAT WHOSE TOKEN IS MISSING is an agent that will never wake, which is
// the whole question this command answers — so it is reported, never
// skipped.
func TestASeatWithNoTokenIsReportedRatherThanSkipped(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Org = &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			chatSeat("SWE", "${MM_TOKEN_SWE}", "eng"),
		}}
		o.SeatToken = func(*org.Role) (string, bool) { return "", false }
	})
	f := finding(t, report, "seat swe")
	if f.OK {
		t.Fatal("a seat with no token passed")
	}
	if !strings.Contains(f.Detail, "never receive a message") {
		t.Errorf("finding = %q", f.Detail)
	}
}

// ONE SEAT'S REFUSED SOCKET IS ONE SEAT'S FINDING: a revoked token leaves
// that agent deaf while the company looks healthy.
func TestOneSeatsRefusedSocketDoesNotHideTheOthers(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Dial = (&dials{refuseToken: "tok-swe"}).dial
		o.Org = &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			chatSeat("SWE", "${MM_TOKEN_SWE}", "eng"),
			chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
		}}
		o.SeatToken = func(seat *org.Role) (string, bool) {
			return "tok-" + seat.Handle(), true
		}
		srv.issue("user-swe", "tok-swe")
		srv.issue("user-ceo", "tok-ceo")
	})
	if f := finding(t, report, "seat swe"); f.OK {
		t.Error("a refused seat socket passed")
	}
	if f := finding(t, report, "seat ceo"); !f.OK {
		t.Errorf("a healthy seat failed because another did: %q", f.Detail)
	}
}

// A CREDENTIAL THAT DOES NOT AUTHENTICATE STOPS THE RUN: every later
// finding would be a consequence rather than a cause.
func TestABadOperatorTokenStopsTheChecks(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.refuseIdentity = true
	report := doctorAgainst(t, srv, nil)
	f := finding(t, report, "credential")
	if f.OK || !f.Fatal {
		t.Fatalf("finding = %+v", f)
	}
	// NOTHING BELOW WAS ASKED: every later check would report a
	// consequence rather than a cause.
	for _, later := range report.Findings {
		if strings.HasPrefix(later.Check, "seat ") || later.Check == "team" {
			t.Errorf("the run continued past a bad credential: %+v", report.Findings)
		}
	}
	// MARKED AS THE STOPPING POINT, so the report does not read as "one
	// thing is wrong" when it means "nothing else was even asked".
	if !report.Stopped() {
		t.Error("the report does not say the checks stopped")
	}
}

// A SERVER WHOSE OWN CONFIGURATION CANNOT BE READ stops the run too: every
// check below it is derived from that payload.
func TestAnUnreadableServerConfigStopsTheChecks(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.refuseConfig = true
	report := doctorAgainst(t, srv, nil)
	if !report.Stopped() || report.Healthy() {
		t.Fatalf("report = %+v", report.Findings)
	}
	if len(report.Findings) != 2 {
		t.Errorf("the run continued past an unreadable config: %+v", report.Findings)
	}
}

// A MISSING TEAM IS A COMPANY THAT NEVER RECEIVES ANYTHING.
func TestAMissingTeamIsReported(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Config.Team = "no-such-team"
	})
	f := finding(t, report, "team")
	if f.OK || !strings.Contains(f.Detail, "will ever receive a message") {
		t.Errorf("finding = %+v", f)
	}
}

// A DISABLED INTEGRATION IS A REFUSAL, not a clean bill of health.
func TestDoctorRefusesADisabledIntegration(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	_, err := doctorRun(t, srv, func(o *mattermost.DoctorOptions) {
		o.Config = &config.Mattermost{Enabled: false}
	})
	if err == nil {
		t.Fatal("a disabled integration was checked")
	}
}

// A LITERAL BOT TOKEN IS A VALUE, not a reference: an operator managing a
// seat's credential by hand is a supported choice, and refusing to check it
// would report a working seat as unconfigured.
func TestSeatTokensHonoursALiteralAndResolvesAReference(t *testing.T) {
	t.Parallel()
	env := config.NewResolver(config.MapSource{"MM_TOKEN_SWE": "resolved-token"})
	resolve := mattermost.SeatTokens(env)

	literal := chatSeat("PM", "written-out-by-hand", "")
	if got, ok := resolve(literal); !ok || got != "written-out-by-hand" {
		t.Errorf("a literal resolved to %q/%v", got, ok)
	}
	reference := chatSeat("SWE", "${MM_TOKEN_SWE}", "eng")
	if got, ok := resolve(reference); !ok || got != "resolved-token" {
		t.Errorf("a reference resolved to %q/%v", got, ok)
	}
	unset := chatSeat("QA", "${MM_TOKEN_QA}", "")
	if got, ok := resolve(unset); ok {
		t.Errorf("an unset reference resolved to %q", got)
	}
	none := chatSeat("CTO", "", "")
	if _, ok := resolve(none); ok {
		t.Error("a seat with no bot token resolved")
	}
}

// AN UNREACHABLE SERVER IS ITS OWN ANSWER, and it comes first,
// unauthenticated: a bad credential must not make a healthy server look
// dead, because the two have completely different remedies.
func TestAnUnreachableServerStopsBeforeAnythingElse(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.unreachable = true
	report := doctorAgainst(t, srv, nil)
	f := finding(t, report, "reachable")
	if f.OK || !f.Fatal {
		t.Fatalf("finding = %+v", f)
	}
	if len(report.Findings) != 1 {
		t.Errorf("the run continued past an unreachable server: %+v", report.Findings)
	}
}

// NO OPERATOR CREDENTIAL IS NEEDED: the seat tokens in the config are what
// the engine authenticates with, so they are the honest thing to check
// with — and minting an admin token to ask whether a company works is a
// step that exists only to be skipped.
func TestTheChecksRunOnASeatTokenWhenNoOperatorOneIsGiven(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.issue("user-swe", "tok-swe")
	client := chatClientWithout(t, srv)
	cfg := enabledChat()
	cfg.URL = client.URL()

	report, err := mattermost.Doctor(context.Background(), mattermost.DoctorOptions{
		Client: client, Config: cfg, Dial: (&dials{}).dial,
		Org: &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			chatSeat("SWE", "${MM_TOKEN_SWE}", "eng"),
		}},
		SeatToken: func(*org.Role) (string, bool) { return "tok-swe", true },
	})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if f := finding(t, report, "credential"); !f.OK {
		t.Errorf("credential = %q", f.Detail)
	}
	if f := finding(t, report, "seat swe"); !f.OK {
		t.Errorf("seat swe = %q", f.Detail)
	}
}

// WITH NO CREDENTIAL AT ALL, the authenticated half is not reported as a
// pile of separate failures — the answer is one line, and the checks that
// need no credential still ran.
func TestWithNoCredentialTheUnauthenticatedChecksStillRun(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	client := chatClientWithout(t, srv)
	cfg := enabledChat()
	cfg.URL = client.URL()

	report, err := mattermost.Doctor(context.Background(), mattermost.DoctorOptions{
		Client: client, Config: cfg, Dial: (&dials{}).dial,
	})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if f := finding(t, report, "reachable"); !f.OK {
		t.Error("reachability was not checked without a credential")
	}
	if f := finding(t, report, "site url"); !f.OK {
		t.Error("the site url was not checked without a credential")
	}
	if f := finding(t, report, "credential"); f.OK || !f.Fatal {
		t.Errorf("finding = %+v", f)
	}
}

// A BOT IN NO CHANNEL HEARS ONLY DIRECT MESSAGES, and its account looks
// perfectly healthy — which is exactly the kind of silence this command
// exists to explain.
func TestASeatInNoChannelIsReported(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.channelsOf = nil
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Org = &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			chatSeat("SWE", "${MM_TOKEN_SWE}", "eng"),
		}}
		o.SeatToken = func(*org.Role) (string, bool) { return "tok-swe", true }
		srv.issue("user-swe", "tok-swe")
	})
	f := finding(t, report, "seat swe")
	if f.OK {
		t.Fatal("a bot in no channel passed")
	}
	if !strings.Contains(f.Detail, "only ever hear direct messages") {
		t.Errorf("finding = %q", f.Detail)
	}
}

// A SEAT THAT FAILS EARLY IS NOT ASKED THE LATER QUESTIONS: reporting a
// refused credential AND a missing socket AND no channels would send an
// operator after faults nobody observed.
func TestASeatWhoseCredentialIsRefusedIsNotAlsoReportedAsDeaf(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	d := &dials{}
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Dial = d.dial
		o.Org = &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			chatSeat("SWE", "${MM_TOKEN_SWE}", "eng"),
		}}
		o.SeatToken = func(*org.Role) (string, bool) { return "never-issued", true }
	})
	f := finding(t, report, "seat swe")
	if f.OK || !strings.Contains(f.Detail, "refused") {
		t.Fatalf("finding = %+v", f)
	}
	for _, token := range d.tokens {
		if token == "never-issued" {
			t.Error("a seat whose credential was refused was still dialled")
		}
	}
}

// A TEAM THAT DOES NOT RESOLVE STILL LEAVES THE SEAT CHECKS WORTH READING:
// the channel question has no answer without it, but whether each seat
// authenticates and opens a socket does — and reporting that as a channel
// failure would send an operator after a second fault that does not exist.
func TestSeatsAreStillCheckedWhenTheTeamDoesNotResolve(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.issue("user-swe", "tok-swe")
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Config.Team = "no-such-team"
		o.Org = &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			chatSeat("SWE", "${MM_TOKEN_SWE}", "eng"),
		}}
		o.SeatToken = func(*org.Role) (string, bool) { return "tok-swe", true }
	})
	if f := finding(t, report, "team"); f.OK {
		t.Fatal("a missing team passed")
	}
	f := finding(t, report, "seat swe")
	if !f.OK {
		t.Errorf("a healthy seat failed because the team did not resolve: %q", f.Detail)
	}
	if strings.Contains(f.Detail, "channel") {
		t.Errorf("the seat was reported against a question that had no answer: %q", f.Detail)
	}
}
