package mattermost

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// The health check for the things that fail SILENTLY.
//
// # Why this command exists at all
//
// Everything that fails loudly on Mattermost is already reported: a bad
// token 401s, a missing team 404s, a channel that is not there is a note in
// the provisioning report. What has no symptom is the Site URL.
//
// Mattermost accepts a websocket only from a client whose Origin matches
// ServiceSettings.SiteURL. The engine sends the Origin a browser at the
// CONFIGURED url would send — so when the two disagree, agents keep working
// (their sockets are refused, they reconnect, and the REST side is fine)
// while every human's browser silently loses its live feed. Nobody files a
// bug about a chat client that just seems slow.
//
// # So it dials, rather than only comparing strings
//
// A string comparison catches the common case and gets the rest wrong: a
// reverse proxy, a subpath deployment, an allowed-origins override. The
// only answer that means anything is a real upgrade, shaped exactly as a
// browser's, against the real server — and then a real authenticated socket
// per seat, because "the server accepts sockets" and "this bot's token
// opens one" are different questions and only the second one wakes an agent.

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

// Dialer opens a websocket the way the fleet does.
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
			"%s did not answer: %v — nothing below could be asked", opts.Client.URL(), err)
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
	checkSiteURL(report, opts.Config.URL, conf)

	reader, seats := credentialed(ctx, report, opts)
	if reader == nil {
		// EVERYTHING BELOW NEEDS A TOKEN, and none resolved. Reporting
		// each of them as a separate failure would send an operator
		// after faults nobody observed.
		return report, nil
	}
	team := checkTeam(ctx, report, opts, reader)
	checkBrowserSocket(ctx, report, opts, dial, reader.Token())
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

// checkSiteURL compares what the server thinks it is against what the
// company config says.
//
// TWO FINDINGS, not one, because the consequences differ and so do the
// fixes: an origin mismatch blinds every browser's live feed, while a path
// mismatch breaks the absolute links the server builds and leaves the
// socket working. Reporting them together would send an operator to fix a
// socket that is fine.
func checkSiteURL(report *Report, configured string, conf map[string]string) {
	reported := SiteURL(conf)
	if reported == "" {
		report.add("site url", false,
			"ServiceSettings.SiteURL is unset. Mattermost refuses every "+
				"websocket whose Origin does not match it, so with no value "+
				"set every browser loses its live feed while agents keep "+
				"working. Set it in System Console → Environment → Web Server")
		return
	}
	switch {
	case !OriginMatches(configured, reported):
		report.add("site url", false,
			"the server reports SiteURL %s and this company is configured "+
				"for %s. Mattermost accepts a websocket only from an Origin "+
				"matching its SiteURL, so every human's live feed fails while "+
				"agents keep working — the failure has no symptom anyone "+
				"reports. Make the two agree", reported, NormalizeURL(configured))
	case !PathMatches(configured, reported):
		report.add("site url", false,
			"the server reports SiteURL %s and this company is configured "+
				"for %s. The origin matches, so websockets work — but the "+
				"paths differ, and the server builds its absolute links and "+
				"plugin URLs from its own value",
			reported, NormalizeURL(configured))
	default:
		report.add("site url", true, "the server agrees it is served at %s", reported)
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

// checkBrowserSocket proves the upgrade a browser performs.
//
// SHAPED EXACTLY AS A BROWSER'S — a credential and the Origin a browser at
// the configured url would send — because that is the request whose refusal
// nobody sees. A probe that sent no Origin, or one carrying a path, would
// pass a check every real browser fails.
func checkBrowserSocket(ctx context.Context, report *Report, opts DoctorOptions, dial Dialer, token string) {
	origin := BrowserOrigin(opts.Config.URL)
	if origin == "" {
		report.add("browser socket", false,
			"%q is not a usable base URL, so no Origin can be derived from it",
			opts.Config.URL)
		return
	}
	target := WebsocketURL(opts.Config.URL)
	if err := dial(ctx, target, origin, token); err != nil {
		report.add("browser socket", false,
			"a browser-shaped upgrade to %s with Origin %s was refused: %v. "+
				"This is what every human's live feed does, and its failure "+
				"is silent — the client simply stops updating",
			target, origin, err)
		return
	}
	report.add("browser socket", true,
		"a browser-shaped upgrade to %s was accepted", target)
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
	origin := BrowserOrigin(opts.Config.URL)
	for _, seat := range seats {
		check := "seat " + seat.handle
		if seat.token == "" {
			report.add(check, false,
				"no bot token resolved for this seat, so it can never open a "+
					"socket and will never receive a message. Check that its "+
					"mattermost.bot_token variable is set in this deployment's "+
					"environment, and run `crewlet mattermost provision` if it "+
					"has never been minted")
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
		if err = dial(ctx, target, origin, seat.token); err != nil {
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
					"so it will only ever hear direct messages. Name channels "+
					"under integrations.mattermost.provisioning or on the seat "+
					"itself, and run `crewlet mattermost provision`", who.Username)
		default:
			report.add(check, true, "%s authenticates, opens a socket, and is in %d channel(s)",
				who.Username, len(channels))
		}
	}
}

// chatSeats is every agent seat that declares a Mattermost bot, in a stable
// order.
func chatSeats(o *org.Organization) []*org.Role {
	var out []*org.Role
	for seat := range o.AllRoles() {
		if seat.IsAgent() && !seat.Mattermost.IsZero() {
			out = append(out, seat)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle() < out[j].Handle() })
	return out
}

// SeatTokens resolves each seat's bot token through the environment, the
// way the engine does.
func SeatTokens(env *config.Resolver) func(*org.Role) (string, bool) {
	return func(seat *org.Role) (string, bool) {
		raw := seat.Mattermost.BotToken
		if strings.TrimSpace(raw) == "" {
			return "", false
		}
		// A LITERAL IS A VALUE, not a reference: an operator managing a
		// seat's credential by hand is a supported choice, and refusing
		// to check it would report a working seat as unconfigured.
		if _, ok := provision.SoleVar(raw); !ok && len(provision.ReferencedVars(raw)) == 0 {
			return raw, true
		}
		value := strings.TrimSpace(env.Value(raw))
		return value, value != ""
	}
}

// RealDial opens a websocket against the instance and closes it.
//
// The connection is made and dropped: what is being asked is whether the
// upgrade is accepted and the credential authenticates, and holding it open
// would only add ways for the check itself to fail.
func RealDial(ctx context.Context, target, origin, token string) error {
	ctx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()
	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
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
