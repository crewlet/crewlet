package mattermost_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/org"
)

// dials records what the doctor asked for and answers how a test wants.
type dials struct {
	targets []string
	origins []string
	tokens  []string
	// site, when set, makes every dial answer as a server with this Site
	// URL and this AllowCorsFrom would — Mattermost's own origin rule
	// ([mattermost.AdmitsOrigin]), not this fake's idea of it.
	site, allowCorsFrom string
	// noSite makes every dial answer as a server with NO Site URL does,
	// which compares the Origin with the Host the upgrade was sent to.
	noSite bool
	// refuse answers this error for a dial carrying this origin, which
	// is how a real server refuses a mismatched one.
	refuseOrigin string
	// refuseToken answers this error for a dial carrying this token.
	refuseToken string
}

// seatOrigins is the origin of every dial made with a seat's token.
func (d *dials) seatOrigins(seatTokens ...string) []string {
	var out []string
	for i, token := range d.tokens {
		if slices.Contains(seatTokens, token) {
			out = append(out, d.origins[i])
		}
	}
	return out
}

func (d *dials) dial(_ context.Context, target, origin, token string) error {
	d.targets = append(d.targets, target)
	d.origins = append(d.origins, origin)
	d.tokens = append(d.tokens, token)
	if d.site != "" || d.noSite {
		u, err := url.Parse(target)
		if err != nil {
			return err
		}
		if !mattermost.AdmitsOrigin(origin, u.Host, d.site, d.allowCorsFrom) {
			return errors.New("expected handshake response status code 101 but got 403")
		}
	}
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

// A SITE URL ON ANOTHER ADDRESS IS NOT A FAULT. People browse the Site URL
// and the engine dials whatever reaches the server — on Kubernetes, the
// in-cluster Service — so the two differing says nothing about anybody's
// live feed. The doctor used to fail this, and the running engine warned
// that browsers would fail, on deployments whose browsers worked.
func TestASiteURLOnAnotherAddressPasses(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.siteURL = "https://chat.example.com"
	d := &dials{site: srv.siteURL}
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Dial = d.dial
		o.Org = &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			chatSeat("SWE", "${MM_TOKEN_SWE}"),
		}}
		o.SeatToken = func(*org.Role) (string, bool) { return "tok-swe", true }
		srv.issue("user-swe", "tok-swe")
		srv.join("eng", "user-swe")
	})
	for _, check := range []string{"site url", "browser socket", "seat swe"} {
		if f := finding(t, report, check); !f.OK {
			t.Errorf("%s failed on a split-horizon deployment: %q", check, f.Detail)
		}
	}
	if !report.Healthy() {
		t.Errorf("the report is unhealthy: %+v", report.Findings)
	}
}

// A MATCHING SITE URL PASSES, including when it differs in case or a
// trailing slash.
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

// AN UNSET SITE URL IS REPORTED for what it breaks, and names where to set
// it — the sentence the running engine logs too.
func TestAnUnsetSiteURLIsReported(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.siteURL = " "
	d := &dials{noSite: true}
	var configured string
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Dial = d.dial
		configured = o.Config.URL
	})
	f := finding(t, report, "site url")
	want, _ := mattermost.JudgeSiteURL("")
	if f.OK || f.Detail != want.Detail || !strings.Contains(f.Detail, "System Console") {
		t.Errorf("finding = %+v", f)
	}
	// AND THE BROWSER PROBE AGREES WITH IT: with no Site URL the server
	// admits an Origin naming the host an upgrade arrives at, so the probe
	// stands in for a browser at the address it dials — and passes, as the
	// finding beside it says browsers do. Without that Origin it would
	// claim every person's websocket refused.
	if f := finding(t, report, "browser socket"); !f.OK {
		t.Errorf("browser socket failed against an unset Site URL: %q", f.Detail)
	}
	if len(d.origins) == 0 || d.origins[0] != mattermost.BrowserOrigin(configured) {
		t.Errorf("the probe sent Origin %q, want the dialled address's %q",
			d.origins, mattermost.BrowserOrigin(configured))
	}
}

// A DEFAULT PORT IN THE URL IS WRITTEN AS A BROWSER WRITES IT. With no Site
// URL the server compares the Origin's host with the Host header as strings;
// a browser at https://chat.example.com:443 sends neither with the port, and
// a probe that dialled the address as written would send a Host with it and
// be refused where every browser is admitted.
func TestTheProbeAddressesTheServerAsABrowserDoes(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.siteURL = " "
	d := &dials{noSite: true}
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Dial = d.dial
		o.Config.URL = "https://chat.example.com:443"
	})
	if f := finding(t, report, "browser socket"); !f.OK {
		t.Errorf("browser socket = %q", f.Detail)
	}
	if len(d.targets) == 0 || d.targets[0] != "wss://chat.example.com/api/v4/websocket" {
		t.Errorf("the probe went to %v", d.targets)
	}
}

// A SITE URL NO BROWSER CAN MATCH — one the server's own validation accepts —
// fails both the setting and the browser probe, while the seats, whose
// sockets carry no Origin, still connect.
func TestASiteURLNoBrowserCanMatchIsReported(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.siteURL = "localhost:8065"
	d := &dials{site: srv.siteURL}
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Dial = d.dial
		o.Org = &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			chatSeat("SWE", "${MM_TOKEN_SWE}"),
		}}
		o.SeatToken = func(*org.Role) (string, bool) { return "tok-swe", true }
		srv.issue("user-swe", "tok-swe")
		srv.join("eng", "user-swe")
	})
	if f := finding(t, report, "site url"); f.OK || !strings.Contains(f.Detail, "absolute") {
		t.Errorf("site url = %+v", f)
	}
	if f := finding(t, report, "browser socket"); f.OK {
		t.Errorf("browser socket passed against a Site URL no browser can match: %q", f.Detail)
	}
	if f := finding(t, report, "seat swe"); !f.OK {
		t.Errorf("the seat failed, though its socket carries no Origin: %q", f.Detail)
	}
}

// A URL THE SERVER REDIRECTS STOPS THE RUN AT THE FIRST QUESTION, naming the
// address to set: a Mattermost served under a Site URL path redirects every
// request outside it, and the engine follows no redirect — so nothing below
// could be asked, and an operator needs the address, not a list of failures.
func TestAURLTheServerRedirectsStopsTheRunNamingTheAddress(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	api := http.StripPrefix("/chat", http.HandlerFunc(srv.serve))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/chat/") {
			r.URL.Path = path.Join("/chat", r.URL.Path)
			http.Redirect(w, r, r.URL.String(), http.StatusFound)
			return
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := mattermost.NewClient(mattermost.ClientOptions{URL: server.URL, Token: adminToken})
	if err != nil {
		t.Fatal(err)
	}
	cfg := enabledChat()
	cfg.URL = server.URL
	report, err := mattermost.Doctor(t.Context(), mattermost.DoctorOptions{
		Client: client, Config: cfg, Dial: (&dials{}).dial,
	})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	f := finding(t, report, "reachable")
	if f.OK || !report.Stopped() {
		t.Fatalf("a redirected url did not stop the run: %+v", report.Findings)
	}
	if want := "set the url to " + server.URL + "/chat"; !strings.Contains(f.Detail, want) {
		t.Errorf("the finding does not say %q: %q", want, f.Detail)
	}
}

// A SITE URL THAT IS NOT WHERE PEOPLE BROWSE IS THE FAULT THIS COMMAND EXISTS
// FOR, and only the person running it can name where they browse. Here the
// Site URL was left on localhost while people use another address — the
// compose stack's first mistake — and with an http Site URL behind a TLS
// proxy, the scheme alone. Judged on its own, either Site URL is usable, so
// without the address people type both passed as healthy.
func TestASiteURLPeopleDoNotBrowseIsReported(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ site, browsed string }{
		{"http://localhost:8065", "http://203.0.113.7:8065"},
		{"http://chat.example.com", "https://chat.example.com"},
	} {
		srv := newChatServer()
		srv.siteURL = tc.site
		d := &dials{site: tc.site}
		report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
			o.Dial = d.dial
			o.BrowsedAt = tc.browsed
		})
		f := finding(t, report, "site url")
		if f.OK || !strings.Contains(f.Detail, tc.browsed) ||
			!strings.Contains(f.Detail, "Set ServiceSettings.SiteURL") {
			t.Errorf("Site URL %s browsed at %s: site url = %+v", tc.site, tc.browsed, f)
		}
		if f := finding(t, report, "browser socket"); f.OK {
			t.Errorf("Site URL %s browsed at %s: a browser there was admitted: %q",
				tc.site, tc.browsed, f.Detail)
		}
		if report.Healthy() {
			t.Errorf("Site URL %s browsed at %s: the report is healthy", tc.site, tc.browsed)
		}
	}
}

// THE ADDRESS PEOPLE BROWSE PASSES WHEN THE SITE URL IS IT, however each is
// spelled — and a company url on another route still passes beside it.
func TestASiteURLWherePeopleBrowsePasses(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.siteURL = "https://chat.example.com/team"
	d := &dials{site: srv.siteURL}
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Dial = d.dial
		o.BrowsedAt = "https://Chat.Example.com/team/channels/town-square"
		o.Config.URL += "/team"
	})
	for _, check := range []string{"site url", "browser socket"} {
		if f := finding(t, report, check); !f.OK {
			t.Errorf("%s = %q", check, f.Detail)
		}
	}
	if f := finding(t, report, "site url"); !strings.Contains(f.Detail, "agrees") {
		t.Errorf("site url does not say the two agree: %q", f.Detail)
	}
}

// GIVEN WHERE PEOPLE BROWSE, THE BROWSER PROBE IS A BROWSER THERE: sent to
// that address, under the Site URL's path where the web app opens its
// socket, carrying that Origin — so it passes through whatever is in front
// of the server, which a probe sent to this company's url never sees.
func TestTheBrowserProbeGoesWhereABrowserGoes(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.siteURL = "https://chat.example.com/team"
	d := &dials{}
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Dial = d.dial
		o.BrowsedAt = "https://chat.example.com:443/team/channels/town-square"
	})
	if f := finding(t, report, "browser socket"); !f.OK {
		t.Fatalf("browser socket = %q", f.Detail)
	}
	if len(d.targets) == 0 {
		t.Fatal("nothing was dialled")
	}
	if got, want := d.targets[0], "wss://chat.example.com/team/api/v4/websocket"; got != want {
		t.Errorf("the probe went to %s, want %s", got, want)
	}
	if got, want := d.origins[0], "https://chat.example.com"; got != want {
		t.Errorf("the probe sent Origin %s, want %s", got, want)
	}
}

// NOT TOLD WHERE PEOPLE BROWSE, THE REPORT SAYS SO rather than passing the
// question it did not ask: the Site URL is judged on its own, and the line
// names how to ask the rest.
func TestAnUnaskedQuestionIsNamed(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.siteURL = "https://chat.example.com"
	report := doctorAgainst(t, srv, nil)
	f := finding(t, report, "site url")
	if !f.OK || !strings.Contains(f.Detail, "-browser-url") ||
		!strings.Contains(f.Detail, "not asked") {
		t.Errorf("site url = %+v", f)
	}
	if f := finding(t, report, "browser socket"); !strings.Contains(f.Detail, "not told where people browse") {
		t.Errorf("browser socket does not say what it stood in for: %q", f.Detail)
	}
}

// AN ADDRESS NO BROWSER CAN BE AT IS REFUSED before anything is asked: the
// run would otherwise hold the Site URL to nothing and call it a mismatch.
func TestABrowsedAddressMustBeOneABrowserCanBeAt(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"chat.example.com", "/team", "ftp://chat.example.com"} {
		_, err := doctorRun(t, newChatServer(), func(o *mattermost.DoctorOptions) {
			o.BrowsedAt = raw
		})
		if err == nil {
			t.Errorf("BrowsedAt %q was accepted", raw)
		}
	}
}

// NOT TOLD WHERE PEOPLE BROWSE, THE BROWSER PROBE CARRIES THE ORIGIN A BROWSER
// AT THE SITE URL SENDS — not the origin of the address this company dials,
// which nobody browses — so it asks the server's rule the question a browser
// at the Site URL asks it.
func TestTheBrowserProbeSendsTheOriginOfTheSiteURL(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.siteURL = "https://chat.example.com:8443/team"
	d := &dials{}
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Dial = d.dial
		o.Config.URL += "/team"
	})
	if f := finding(t, report, "browser socket"); !f.OK {
		t.Fatalf("browser socket = %q", f.Detail)
	}
	if len(d.origins) == 0 {
		t.Fatal("nothing was dialled")
	}
	if got, want := d.origins[0], "https://chat.example.com:8443"; got != want {
		t.Errorf("the probe sent Origin %q, want %q: a browser at the Site URL "+
			"sends its scheme and host, never a path", got, want)
	}
}

// A REFUSED UPGRADE IS THE FINDING THAT MATTERS, because the real symptom
// is a chat client that just seems slow. Here the Site URL is written with
// capitals and AllowCorsFrom is set, which makes the server compare a
// browser's Origin with the Site URL string for string — a refusal the
// public configuration cannot show, because AllowCorsFrom is not in it, so
// the setting passes and only the live probe finds it.
func TestARefusedBrowserUpgradeIsReported(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.siteURL = "https://Chat.Example.com"
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Dial = (&dials{site: srv.siteURL, allowCorsFrom: "https://admin.example.com"}).dial
	})
	if f := finding(t, report, "site url"); !f.OK {
		t.Fatalf("site url = %q, though the setting alone is sound", f.Detail)
	}
	f := finding(t, report, "browser socket")
	if f.OK {
		t.Fatal("a refused upgrade passed")
	}
	if !strings.Contains(f.Detail, "silent") {
		t.Errorf("the detail does not say the failure is silent: %q", f.Detail)
	}
}

// THE SEAT PROBES ARE THE ENGINE'S OWN HANDSHAKE: no Origin. A probe carrying
// one is judged by the browser rule the engine is not subject to, and would
// call a working seat deaf on any deployment that dials another address.
func TestTheSeatProbesCarryNoOrigin(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.siteURL = "https://chat.example.com"
	d := &dials{site: srv.siteURL}
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Dial = d.dial
		o.Org = &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			chatSeat("SWE", "${MM_TOKEN_SWE}"),
		}}
		o.SeatToken = func(*org.Role) (string, bool) { return "tok-swe", true }
		srv.issue("user-swe", "tok-swe")
		srv.join("eng", "user-swe")
	})
	if f := finding(t, report, "seat swe"); !f.OK {
		t.Errorf("seat swe = %q", f.Detail)
	}
	origins := d.seatOrigins("tok-swe")
	if len(origins) == 0 {
		t.Fatal("the seat was never dialled")
	}
	for _, origin := range origins {
		if origin != "" {
			t.Errorf("a seat probe sent Origin %q", origin)
		}
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
			chatSeat("SWE", "${MM_TOKEN_SWE}"),
			chatSeat("CEO", "${MM_TOKEN_CEO}"),
		}}
		o.SeatToken = func(seat *org.Role) (string, bool) {
			return "tok-" + seat.Handle(), true
		}
		srv.issue("user-swe", "tok-swe")
		srv.issue("user-ceo", "tok-ceo")
		// AND EACH IS IN A CHANNEL, which is a separate question from
		// authenticating: the fixture derives a bot's channel list from
		// what it actually joined, so a seat nobody joined is reported
		// deaf however healthy its credential is.
		srv.join("eng", "user-swe")
		srv.join("leadership", "user-ceo")
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
			chatSeat("SWE", "${MM_TOKEN_SWE}"),
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
			chatSeat("SWE", "${MM_TOKEN_SWE}"),
			chatSeat("CEO", "${MM_TOKEN_CEO}"),
		}}
		o.SeatToken = func(seat *org.Role) (string, bool) {
			return "tok-" + seat.Handle(), true
		}
		srv.issue("user-swe", "tok-swe")
		srv.issue("user-ceo", "tok-ceo")
		srv.join("eng", "user-swe")
		srv.join("leadership", "user-ceo")
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

	literal := chatSeat("PM", "written-out-by-hand")
	if got, ok := resolve(literal); !ok || got != "written-out-by-hand" {
		t.Errorf("a literal resolved to %q/%v", got, ok)
	}
	reference := chatSeat("SWE", "${MM_TOKEN_SWE}")
	if got, ok := resolve(reference); !ok || got != "resolved-token" {
		t.Errorf("a reference resolved to %q/%v", got, ok)
	}
	unset := chatSeat("QA", "${MM_TOKEN_QA}")
	if got, ok := resolve(unset); ok {
		t.Errorf("an unset reference resolved to %q", got)
	}
	none := chatSeat("CTO", "")
	if _, ok := resolve(none); ok {
		t.Error("a seat with no bot token resolved")
	}
}

// THE DOCTOR CHECKS THE TOKEN THE ENGINE SENDS, whatever shape the config
// wrote it in. It expanded a reference wherever one sat while the transport
// resolves only a whole one, so a token written as `tok-${VAR}` passed the
// doctor as the resolved token and reached the server as the literal text.
// Every shape is run through both readers here rather than each against an
// expected value, so a reader that changes alone is what fails.
func TestTheDoctorResolvesATokenAsTheTransportDoes(t *testing.T) {
	t.Parallel()
	vars := config.MapSource{"MM_TOKEN": "resolved-token", "MM_BLANK": ""}
	env := config.NewResolver(vars)
	for _, raw := range []string{
		"written-out-by-hand",
		"${MM_TOKEN}",
		"  ${MM_TOKEN}  ",
		"${MM_UNSET}",
		"${MM_BLANK}",
		"tok-${MM_TOKEN}",
		"${MM_TOKEN}${MM_TOKEN}",
	} {
		o := &org.Organization{Name: "nimbus", Roles: []*org.Role{chatSeat("SWE", raw)}}
		var engine string
		if seats := mattermost.SeatsFrom(o, env.LookupOK); len(seats) == 1 {
			engine = seats[0].Token
		}
		doctor, ok := mattermost.SeatTokens(env)(o.Roles[0])
		if doctor != engine || ok != (engine != "") {
			t.Errorf("bot_token %q: the doctor checks %q (ok=%v), the transport "+
				"authenticates with %q", raw, doctor, ok, engine)
		}
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
	srv.join("eng", "user-swe")
	client := chatClientWithout(t, srv)
	cfg := enabledChat()
	cfg.URL = client.URL()

	report, err := mattermost.Doctor(context.Background(), mattermost.DoctorOptions{
		Client: client, Config: cfg, Dial: (&dials{}).dial,
		Org: &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			chatSeat("SWE", "${MM_TOKEN_SWE}"),
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
	// NOTHING IS JOINED, which is the whole case: the fixture answers a
	// bot's channel list from what it actually joined, so a seat that
	// never joined anything is the bot this check exists to find.
	report := doctorAgainst(t, srv, func(o *mattermost.DoctorOptions) {
		o.Org = &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			chatSeat("SWE", "${MM_TOKEN_SWE}"),
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
			chatSeat("SWE", "${MM_TOKEN_SWE}"),
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
			chatSeat("SWE", "${MM_TOKEN_SWE}"),
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
