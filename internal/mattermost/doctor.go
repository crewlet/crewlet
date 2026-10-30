package mattermost

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/org"
)

// The health check for the things that fail SILENTLY.
//
// # Why this command exists at all
//
// Everything that fails loudly on Mattermost is already reported: a bad
// token 401s, a missing team 404s, a channel that is not there is a note in
// the provisioning report. What has no symptom is the Site URL.
//
// Mattermost admits a BROWSER's websocket only when its Origin matches the
// server's own ServiceSettings.SiteURL (or an AllowCorsFrom entry), so a
// Site URL that is not where people browse costs every person their live
// feed while the agents keep working — their sockets carry no Origin, and
// the check admits an upgrade with none. Nobody files a bug about a chat
// client that just seems slow.
//
// # Where people browse is the one fact only the person running this has
//
// The server reports where it BELIEVES it is browsed, its Site URL; the
// engine may reach it by any route, and an in-cluster Service dialled while
// people browse a public name is the ordinary deployment on Kubernetes. So
// this company's url says nothing about browsers, and comparing it with the
// Site URL — what this check once did — failed every such deployment while
// its browsers worked, and passed none it could not see. The address people
// type is an INPUT ([DoctorOptions.BrowsedAt], the CLI's -browser-url): given
// it, the Site URL is held to it and the browser probe goes there; without
// it, the Site URL is judged on its own ([JudgeSiteURL], the sentence the
// running transport logs too) and the report says what was not asked.
//
// A url the server does not serve its API at is not a comparison either: the
// client refuses the redirect, or names the web page, that it is answered
// with, and the first check to ask says so.
//
// # So it dials, rather than only reading settings
//
// A setting can be read; what the server DOES with an Origin cannot,
// because AllowCorsFrom is not in its public configuration. The only answer
// that means anything is a real upgrade carrying a browser's Origin — and
// then a real authenticated socket per seat, shaped exactly as the engine's
// own, because "the server accepts sockets" and "this bot's token opens one"
// are different questions and only the second one wakes an agent.

// Finding is one thing the doctor checked.
type Finding struct {
	// Check names what was asked, for the report.
	Check string
	// OK is whether it passed.
	OK bool
	// Detail says what was found, and — when it failed — what to do.
	Detail string
	// Fatal marks a failure that STOPPED the checks after it, because
	// everything downstream would have reported a consequence rather
	// than a cause.
	//
	// Visible in the report, and that is the point: one failing line
	// with nothing after it reads as "one thing is wrong", when what it
	// means is "nothing else was even asked".
	Fatal bool
}

// Report is a whole run.
type Report struct{ Findings []Finding }

// Healthy reports a run with nothing wrong.
func (r *Report) Healthy() bool {
	for _, f := range r.Findings {
		if !f.OK {
			return false
		}
	}
	return true
}

// Stopped reports a run that gave up before checking everything.
func (r *Report) Stopped() bool {
	for _, f := range r.Findings {
		if f.Fatal {
			return true
		}
	}
	return false
}

func (r *Report) add(check string, ok bool, format string, args ...any) {
	r.Findings = append(r.Findings, Finding{
		Check: check, OK: ok, Detail: fmt.Sprintf(format, args...),
	})
}

func (r *Report) fatal(check string, format string, args ...any) {
	r.Findings = append(r.Findings, Finding{
		Check: check, Detail: fmt.Sprintf(format, args...), Fatal: true,
	})
}

// Dialer opens a websocket: the way the fleet does when origin is empty, and
// the way a browser does when it is not.
//
// Injected so the suite can drive the refusals a real server produces
// without one — an origin check is exactly the behaviour a fake HTTP server
// can model faithfully, and exactly the behaviour a mock cannot.
type Dialer func(ctx context.Context, target, origin, token string) error

// DoctorOptions are one run's inputs.
type DoctorOptions struct {
	// Client talks to the instance. Its token may be empty: the checks
	// that need one borrow a SEAT's, because those are the credentials
	// the engine actually uses and an operator should not have to mint
	// anything to ask whether their company works.
	Client *Client

	// Config is the company's mattermost block, resolved.
	Config *config.Mattermost

	// Org is the company, for the per-seat socket checks.
	Org *org.Organization

	// SeatToken resolves a seat's bot token. A seat whose token is
	// missing is reported rather than skipped: it is an agent that will
	// never wake, which is the whole question this command answers.
	SeatToken func(seat *org.Role) (string, bool)

	// BrowsedAt is the address people type into their browser to reach the
	// server, or empty when the person running the doctor did not say.
	//
	// Only a person knows it: the server reports where it believes it is
	// browsed, and this company's url may be any route to it. Given, the
	// Site URL is held to it and the browser probe is sent there, through
	// whatever stands in front of the server; empty, the Site URL is judged
	// on its own and the probe stands in for a browser at the Site URL.
	BrowsedAt string

	// Dial opens a socket. Nil takes the real one.
	Dial Dialer
}

// Doctor runs the checks, in the order things break.
func Doctor(ctx context.Context, opts DoctorOptions) (*Report, error) {
	if opts.Client == nil {
		return nil, errors.New("mattermost: no client")
	}
	if opts.Config == nil || !opts.Config.Enabled {
		return nil, errors.New("mattermost: the company config does not enable mattermost")
	}
	browsedAt := NormalizeURL(opts.BrowsedAt)
	if browsedAt != "" && BrowserOrigin(browsedAt) == "" {
		return nil, fmt.Errorf("mattermost: %q is not an address a browser can be at: "+
			"give the absolute http or https address people type", opts.BrowsedAt)
	}
	dial := opts.Dial
	if dial == nil {
		dial = RealDial
	}
	report := &Report{}

	// FIRST AND UNAUTHENTICATED. A bad credential must not make a healthy
	// server look dead: the two have completely different remedies, and
	// an operator sent to restart their instance over a revoked token has
	// been actively misled.
	//
	// This inherits the client's retry budget, so a server mid-restart is
	// waited out rather than reported as down — which is slower on a
	// genuinely dead host and correct, because a doctor that called a
	// restarting server dead would disagree with the engine about the one
	// word they both use.
	if err := opts.Client.Ping(ctx); err != nil {
		report.fatal("reachable",
			"%s did not answer as Mattermost's API: %v — nothing below could be "+
				"asked", opts.Client.URL(), err)
		return report, nil
	}
	report.add("reachable", true, "%s answers", opts.Client.URL())

	// ALSO UNAUTHENTICATED: the client configuration is public, and the
	// one setting whose failure has no error message is in it.
	conf, err := opts.Client.ClientConfig(ctx)
	if err != nil {
		report.fatal("site url",
			"could not read the server's own configuration: %v — the one "+
				"setting whose failure has no error message is in it", err)
		return report, nil
	}
	checkSiteURL(report, browsedAt, opts.Config.URL, conf)

	reader, seats := credentialed(ctx, report, opts)
	if reader == nil {
		// EVERYTHING BELOW NEEDS A TOKEN, and none resolved. Reporting
		// each of them as a separate failure would send an operator
		// after faults nobody observed.
		return report, nil
	}
	team := checkTeam(ctx, report, opts, reader)
	checkBrowserSocket(ctx, report, dial, reader.Token(), browsedAt, opts.Config.URL, SiteURL(conf))
	checkSeats(ctx, report, opts, dial, seats, team)
	return report, nil
}

// credentialed picks the client the authenticated checks run as, and
// resolves every seat's own credential once.
//
// # No operator credential is required
//
// The seat tokens in the config are the credentials the engine authenticates
// with, so they are also the honest thing to check with — and asking an
// operator to mint an admin token to find out whether their company works is
// a step that exists only to be skipped. One is used if given.
func credentialed(ctx context.Context, report *Report, opts DoctorOptions) (*Client, []seatCredential) {
	seats := resolveSeats(opts)
	reader := opts.Client
	if strings.TrimSpace(reader.Token()) == "" {
		reader = nil
		for _, seat := range seats {
			if seat.token != "" {
				reader = seat.client(opts.Client)
				break
			}
		}
	}
	if reader == nil {
		report.fatal("credential",
			"no credential resolved: neither an operator token nor any seat's "+
				"bot token. Pass -admin-token, or make this deployment's "+
				"environment carry the variables the seats' mattermost."+
				"bot_token fields reference")
		return nil, seats
	}
	who, err := reader.Me(ctx)
	if err != nil {
		report.fatal("credential",
			"the credential the checks run as does not authenticate: %v — "+
				"nothing below could be asked", err)
		return nil, seats
	}
	report.add("credential", true, "authenticates as %s", who.Username)
	return reader, seats
}

// seatCredential is one agent seat and the credential it would use.
type seatCredential struct {
	handle string
	token  string
}

// client builds a client authenticating as this seat.
func (s seatCredential) client(base *Client) *Client {
	out, err := NewClient(ClientOptions{
		URL: base.URL(), Token: s.token, HTTP: base.HTTP(),
	})
	if err != nil {
		return nil
	}
	return out
}

// resolveSeats reads every chat seat's credential once.
func resolveSeats(opts DoctorOptions) []seatCredential {
	if opts.Org == nil {
		return nil
	}
	var out []seatCredential
	for _, seat := range chatSeats(opts.Org) {
		credential := seatCredential{handle: seat.Handle()}
		if opts.SeatToken != nil {
			if token, ok := opts.SeatToken(seat); ok {
				credential.token = strings.TrimSpace(token)
			}
		}
		out = append(out, credential)
	}
	return out
}

// checkSiteURL judges the address the server believes it is served at.
//
// THE SAME JUDGEMENT THE TRANSPORT LOGS ([JudgeSiteURL]) first, so a doctor
// run and a running node never disagree about one server; then, when the
// person running this said where people browse, the Site URL is held to it —
// the one comparison whose premise holds, because that is the address the
// origin check and every link the server builds have to be. This company's
// url is never compared: people browse the one and the engine dials the
// other.
func checkSiteURL(report *Report, browsedAt, configured string, conf map[string]string) {
	reported := SiteURL(conf)
	if fault, ok := JudgeSiteURL(reported); ok {
		report.add("site url", false, "%s", fault.Detail)
		return
	}
	site := BrowserOrigin(reported)
	var dialled string
	if BrowserOrigin(configured) != site {
		dialled = fmt.Sprintf(" This company reaches it at %s, which is fine: the "+
			"engine's sockets carry no Origin, and nothing a person opens is built "+
			"from the address it dials", NormalizeURL(configured))
	}
	browser := BrowserOrigin(browsedAt)
	switch {
	case browser == "":
		report.add("site url", true,
			"the server can use its Site URL, %s. Whether people browse it there "+
				"was not asked: pass -browser-url, or set MATTERMOST_PUBLIC_URL, "+
				"to the address they type.%s", reported, dialled)
	case browser != site:
		// The server compares the Origin's scheme and host with the Site
		// URL's, case-insensitively; both are lower-cased here, and a Site
		// URL spelling a default port was judged above.
		report.add("site url", false,
			"people browse the server at %s, and its Site URL is %s. It admits a "+
				"browser's websocket only when the Origin's scheme and host are the "+
				"Site URL's, so unless AllowCorsFrom lists that origin every person "+
				"loses their live feed — and every link the server and its plugins "+
				"build points at %s whatever AllowCorsFrom holds. Set "+
				"ServiceSettings.SiteURL to the address people type",
			browser, reported, reported)
	default:
		report.add("site url", true, "people browse the server at %s, and its Site URL, "+
			"%s, agrees.%s", browser, reported, dialled)
	}
}

// checkTeam resolves the configured team.
//
// A bot in no team receives nothing, and the provisioner's own report is
// the only other place this shows up — so an operator debugging silence
// after the fact needs it here. The resolved team is also what the per-seat
// channel check hangs off, which is why it comes back.
func checkTeam(ctx context.Context, report *Report, opts DoctorOptions, reader *Client) string {
	team, found, err := reader.TeamByName(ctx, opts.Config.Team)
	switch {
	case err != nil:
		report.add("team", false, "could not resolve team %q: %v", opts.Config.Team, err)
	case !found:
		report.add("team", false,
			"this server has no team %q. Channels are team-scoped, so no bot "+
				"can be placed and no agent will ever receive a message",
			opts.Config.Team)
	default:
		report.add("team", true, "team %q resolves (%s)", opts.Config.Team, team.ID)
		return team.ID
	}
	return ""
}

// checkBrowserSocket proves the upgrade a person's browser performs.
//
// GIVEN WHERE PEOPLE BROWSE, IT IS A BROWSER THERE, whole: sent to that
// address, under the Site URL's path as the web app's own socket is, carrying
// that Origin — so it passes through whatever stands in front of the server,
// a proxy that drops the Upgrade header included, and asks the server's
// origin rule, AllowCorsFrom included, the question a person's browser asks.
//
// NOT GIVEN, it can stand in only for a browser the server itself names. With
// a Site URL that is one at the Site URL, and the rule compares the Origin
// with the Site URL rather than with the host an upgrade arrives at, so the
// probe is sent to this company's url, which this run already reaches: it
// finds an AllowCorsFrom that refuses the server's own browsers, and nothing
// in front of the Site URL. With no Site URL the rule compares the Origin with
// the host the upgrade arrives at, so the only browser to stand in for is one
// at the address dialled, written as a browser writes it (no default port, in
// the Origin or the Host).
func checkBrowserSocket(ctx context.Context, report *Report, dial Dialer,
	token, browsedAt, configured, reported string,
) {
	var origin, target, asked string
	switch {
	case browsedAt != "":
		origin = BrowserOrigin(browsedAt)
		target = WebsocketURL(origin + urlPath(reported))
		asked = "what a browser at " + origin + " sends, sent where it sends it"
	case reported != "":
		origin = BrowserOrigin(reported)
		if origin == "" {
			report.add("browser socket", false,
				"no browser can be at %q, so there is no Origin to send — the "+
					"server refuses every person's websocket that AllowCorsFrom "+
					"does not admit; see the site url finding", reported)
			return
		}
		target = WebsocketURL(browserAddress(configured))
		asked = "the Site URL's own: this run was not told where people browse, " +
			"so it asks the server's rule and nothing in front of the server"
	default:
		origin = BrowserOrigin(configured)
		target = WebsocketURL(browserAddress(configured))
		asked = "the address this company dials, which a server with no Site URL " +
			"compares with the host an upgrade arrives at"
	}
	if err := dial(ctx, target, origin, token); err != nil {
		report.add("browser socket", false,
			"an upgrade carrying Origin %s — %s — failed at %s: %v. This is every "+
				"person's live feed, and its failure is silent: the web app simply "+
				"stops updating. A 403 is the server's origin check, which admits "+
				"that Origin when its scheme and host are the Site URL's, or when "+
				"AllowCorsFrom lists it exactly; anything else stopped it on the "+
				"way — a proxy that does not forward the Upgrade header, or an "+
				"address this run cannot reach",
			origin, asked, target, err)
		return
	}
	report.add("browser socket", true,
		"an upgrade carrying Origin %s — %s — was accepted at %s", origin, asked, target)
}

// browserAddress is raw as a browser addresses it: a scheme's default port is
// never written, so neither the Origin nor the Host a browser sends carries
// one — and a server with no Site URL compares the two as strings.
func browserAddress(raw string) string {
	u, err := url.Parse(NormalizeURL(raw))
	if err != nil {
		return raw
	}
	if port := u.Port(); port != "" && port == defaultPort(strings.ToLower(u.Scheme)) {
		host := u.Hostname()
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		u.Host = host
	}
	return u.String()
}

// checkSeats asks, per agent seat, whether that seat would actually wake.
//
// # "The server accepts sockets" is not "this bot wakes"
//
// A seat fails on its own while every other check passes: a token that was
// revoked, a bot that was deactivated, a `${VAR}` that never made it into
// this deployment's environment, a bot in no channel. Each leaves ONE agent
// permanently deaf while the company looks healthy, so each seat is checked
// with ITS OWN credential.
//
// # A seat that fails early is not asked the later questions
//
// A seat whose token did not resolve never gets a socket dialled, and one
// whose credential is refused is never asked about its channels. Reporting
// those as separate failures would send an operator after faults nobody
// observed — the answer is already in the first line.
func checkSeats(ctx context.Context, report *Report, opts DoctorOptions, dial Dialer,
	seats []seatCredential, teamID string,
) {
	target := WebsocketURL(opts.Config.URL)
	for _, seat := range seats {
		check := "seat " + seat.handle
		if seat.token == "" {
			report.add(check, false,
				"no bot token resolved for this seat, so it can never open a "+
					"socket and will never receive a message. Check that its "+
					"mattermost.bot_token variable is set in this deployment's "+
					"environment, and that the agent is turned on in the "+
					"Mattermost integration's Agents tab, which mints it")
			continue
		}
		client := seat.client(opts.Client)
		if client == nil {
			report.add(check, false, "this seat's credential could not be used")
			continue
		}
		who, err := client.Me(ctx)
		if err != nil {
			report.add(check, false,
				"this seat's own credential is refused: %v. Its agent cannot "+
					"do anything at all, while everything else about the "+
					"company looks healthy", err)
			continue
		}
		// NO ORIGIN, as the engine's own dial sends none ([socketHeader]):
		// a seat probe carrying one would be judged by the browser rule
		// the engine is not subject to.
		if err = dial(ctx, target, "", seat.token); err != nil {
			report.add(check, false,
				"this seat authenticates but cannot open a socket: %v. A token "+
					"valid for REST can still fail here, and the socket is the "+
					"engine's only inbound path — the agent is deaf", err)
			continue
		}
		if teamID == "" {
			// The team did not resolve, so the channel question has no
			// answer. The socket check above is still worth having.
			report.add(check, true, "%s authenticates and opens a socket", who.Username)
			continue
		}
		channels, err := client.Channels(ctx, who.ID, teamID)
		switch {
		case err != nil:
			report.add(check, false,
				"%s authenticates and opens a socket, but its channels could "+
					"not be read: %v", who.Username, err)
		case len(channels) == 0:
			// A BOT HEARS ONLY WHAT ITS CHANNELS DELIVER. One in none
			// is an agent that wakes for direct messages and nothing
			// else — and its account looks perfectly healthy.
			report.add(check, false,
				"%s authenticates and opens a socket but has joined no channel, "+
					"so it will only ever hear direct messages. Give the agent "+
					"its channels in the Mattermost integration's Agents tab", who.Username)
		default:
			report.add(check, true, "%s authenticates, opens a socket, and is in %d channel(s)",
				who.Username, len(channels))
		}
	}
}

// SeatTokenReads is every variable [SeatTokens] reads for the seats the doctor
// checks ([chatSeats]), each once, sorted: a bot token that is one whole
// ${VAR} reads that variable, and any other value is a literal the transport
// sends as written, reading none.
//
// For `crewlet mattermost doctor`, which asks a running node for exactly
// these, each a reveal logged by name.
func SeatTokenReads(o *org.Organization) []string {
	var names []string
	for _, seat := range chatSeats(o) {
		if name, ok := envref.Whole(seat.Mattermost.BotToken); ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// chatSeats is every agent seat that declares a Mattermost bot, in a stable
// order.
func chatSeats(o *org.Organization) []*org.Role {
	if o == nil {
		return nil
	}
	var out []*org.Role
	for seat := range o.AllRoles() {
		if seat.IsAgent() && !seat.Mattermost.IsZero() {
			out = append(out, seat)
		}
	}
	slices.SortFunc(out, func(a, b *org.Role) int { return cmp.Compare(a.Handle(), b.Handle()) })
	return out
}

// SeatTokens resolves each seat's bot token EXACTLY as the engine's transport
// does ([SeatsFrom], through [seatToken]): a literal is the token, a whole
// ${VAR} is its variable's value, and a reference inside other text is the
// literal the transport would send.
//
// One function rather than a second reading, because a doctor that resolved
// more than the engine does would report a seat healthy that the engine
// authenticates as nobody — this one expanded a reference wherever it sat,
// while the transport resolves only a whole one. A literal is still a value:
// an operator managing a seat's credential by hand is a supported choice, and
// refusing to check it would report a working seat as unconfigured.
func SeatTokens(env *config.Resolver) func(*org.Role) (string, bool) {
	return func(seat *org.Role) (string, bool) {
		token := seatToken(seat, env.LookupOK)
		return token, token != ""
	}
}

// RealDial opens a websocket against the instance and closes it.
//
// With an empty origin the handshake is exactly the engine's own
// ([socketHeader]); with one, it is a browser's.
//
// The connection is made and dropped: what is being asked is whether the
// upgrade is accepted and the credential authenticates, and holding it open
// would only add ways for the check itself to fail.
func RealDial(ctx context.Context, target, origin, token string) error {
	ctx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()
	header := http.Header{}
	if token != "" {
		header = socketHeader(token)
	}
	if origin != "" {
		header.Set("Origin", origin)
	}
	return dialOnce(ctx, target, header)
}

// DialTimeout bounds one probe.
//
// SHORT. Every check here is a single round trip against an instance the
// operator is sitting in front of, and the failure this command exists to
// find is a refusal — which arrives immediately. A long timeout would turn
// a down server into a command that appears to hang.
const DialTimeout = 10 * time.Second

// dialOnce performs the upgrade and closes it.
func dialOnce(ctx context.Context, target string, header http.Header) error {
	//nolint:bodyclose // On a successful upgrade the library has already
	// closed the handshake response; on a failure there is no body to close.
	conn, _, err := websocket.Dial(ctx, target, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return err
	}
	return conn.Close(websocket.StatusNormalClosure, "")
}
