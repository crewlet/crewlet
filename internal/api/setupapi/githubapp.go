package setupapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
)

// Giving one agent its own GitHub App, in the two acts a person performs.
//
// # Why this is not a reconcile pass
//
// Everything else this engine provisions is unattended: an organization key
// creates a Datadog account or an Atlassian service account with no browser
// anywhere. GitHub has no equivalent. An app is created by POSTing a manifest
// from a page carrying the operator's own GitHub session, so a person who may
// create apps on that organization confirms what is being created. There is
// no server-to-server path, and a reconcile loop cannot open a browser.
//
// So the flow is two routes and two clicks:
//
//	POST /setup/integrations/github/app   the engine hands back a manifest
//	                                      and the URL to POST it to
//	GET  /webhooks/github-app             GitHub returns the one-time code,
//	                                      the engine converts and seals it
//
// and then one more click, at GitHub, to INSTALL the app that now exists.
// The reconcile loop takes over from there and never needs a browser again.
//
// # The state is verified, not merely carried
//
// The callback is unauthenticated, because a browser redirect from GitHub
// carries no engine credential. So the only thing standing between it and
// anybody who can reach the engine is the state, and it is a SEALED token
// naming the seat, its expiry and who began the creation, minted here and
// opened there. Slack's landing carries a handle for display and checks
// nothing, which is fine for a page that only prints a code and would not be
// fine here: this callback writes a credential into a seat.
//
// # And it says who is doing this
//
// The begin route is the one request in the flow that carries a credential,
// and the callback's writes — the app's private key sealed into the store, the
// app recorded on the seat — are the continuation of that person's gesture. So
// the state carries them: the author, their kind and the credential they began
// through, which is what both writes record. They used to record `setup`, a
// name that is nobody, so "who gave this agent its GitHub identity" had no
// answer in the company's revisions or on the key's row.
//
// SEALED AND NOT SIGNED: the state goes to GitHub and comes back in a URL —
// through GitHub's logs, a browser history and every ingress access log — and
// who began a creation is this company's business rather than any of theirs,
// which a signature would leave readable to every one of them. It is sealed under the fleet keyring
// every node holds, so the node GitHub returns the browser to opens what
// another node minted, and rotating the keyring leaves a state minted under
// the previous key valid while that key is still in the ring.

// completeDeadline bounds the detached half of a conversion.
//
// Generous against three HTTP calls and two seals, because it is a BACKSTOP
// for a GitHub that stopped answering rather than a deadline for the work: the
// window it protects is the one where the app exists and its key is not sealed
// yet, and cutting that short is the failure it exists to prevent. Shorter
// than [manifestTTL], so a conversion cannot still be running when the state
// that authorized it would have expired.
const completeDeadline = 2 * time.Minute

// manifestTTL is how long a begun app creation stays valid.
//
// IT IS [coord.SetupOnceRetention] RATHER THAN A NUMBER BESIDE IT. The state
// is spent by a claim in the fleet's registry taken at this TTL, and the claim
// lapses at the deadline its own record carries — so a state that outlived
// the claim spending it would still open after the thing that spends it had
// stopped holding anybody off, which is a bearer credential that works as
// often as it is presented. Written as two numbers it was exactly that: a
// fifteen-minute claim that lapsed with a five-minute bucket, replayable for
// the last ten.
//
// The value's own reasons live at that constant: a quarter of an hour is the
// span of the task (a browser round trip to GitHub and two clicks), and it is
// under GitHub's one-hour life for the one-time code a conversion returns, so
// a link that expires says so rather than failing later on a dead code.
const manifestTTL = coord.SetupOnceRetention

// stateAAD binds a sealed state to this flow, so no other envelope the
// keyring seals — a login flight, a stored secret — opens as one.
const stateAAD = "setup/github_app_state"

// appState is what a begun creation carries through GitHub and back: the seat,
// when the link lapses, and who began it.
type appState struct {
	Seat      string    `json:"seat"`
	ExpiresAt time.Time `json:"expires_at"`

	// By, ByKind and OperatorID are the party that began the creation, as
	// [iam.ActorFor] names it — who the callback's writes record.
	By         string `json:"by"`
	ByKind     string `json:"by_kind"`
	OperatorID string `json:"operator_id,omitempty"`
}

// party is the state's beginner as a write records one.
func (s appState) party() iam.Actor {
	return iam.Actor{Name: s.By, Kind: iam.ActorKind(s.ByKind), OperatorID: s.OperatorID}
}

// mintState seals a state for one seat, begun by by.
//
// URL-SAFE, because it travels as a query parameter to GitHub and back: the
// keyring's own envelope is standard base64 behind a colon-separated prefix,
// and a client that concatenated it into a URL without escaping would have
// every `+` read back as a space.
func (f *AppFlow) mintState(handle string, by iam.Actor) (string, error) {
	body, err := json.Marshal(appState{
		Seat: handle, ExpiresAt: f.service.now().Add(manifestTTL),
		By: by.Name, ByKind: string(by.Kind), OperatorID: by.OperatorID,
	})
	if err != nil {
		return "", fmt.Errorf("setupapi: encode the app state: %w", err)
	}
	sealed, err := f.cipher.Encrypt(string(body), stateAAD)
	if err != nil {
		return "", fmt.Errorf("setupapi: seal the app state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(sealed)), nil
}

// openState opens a state this fleet minted and reports whether it is still
// good.
//
// EVERY FAILURE IS ONE ANSWER: a state that will not decode, that another
// keyring or another purpose sealed, that somebody edited, that has lapsed,
// or that names nobody is refused alike — the caller's only move for any of
// them is to begin again, and telling the caller which is telling an attacker
// which.
func (f *AppFlow) openState(state string) (appState, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(state))
	if err != nil {
		return appState{}, ErrStateRefused
	}
	body, err := f.cipher.Decrypt(string(sealed), stateAAD)
	if err != nil {
		return appState{}, ErrStateRefused
	}
	var opened appState
	if err := json.Unmarshal([]byte(body), &opened); err != nil {
		return appState{}, ErrStateRefused
	}
	// A STATE THAT NAMES NOBODY, or no seat, is not one this flow mints:
	// begin always writes both, so either missing is a state from a build
	// that is not this one's, and its writes would record nobody.
	if opened.Seat == "" || opened.By == "" ||
		!f.service.now().Before(opened.ExpiresAt) {
		return appState{}, ErrStateRefused
	}
	return opened, nil
}

// AppFlow is what the callback needs to finish an app creation.
//
// A SEPARATE TYPE from the service because the callback is served by the
// webhooks mux, which is unauthenticated and knows nothing about setup. It is
// handed this and nothing else.
type AppFlow struct {
	service *Service
	cipher  secrets.Cipher
	spent   StateClaims
}

// StateClaims is what spends a callback state, so no state is ever accepted
// twice.
//
// The consumer's own interface, one method wide: this package needs
// first-claim-wins and nothing else. The fleet's [coord.Claims] satisfies it,
// and reading it here INVERTS that type's documented polarity on purpose.
// Webhook dedupe fails OPEN, because a push suppressed by a store blip is a
// wake nobody notices. This is an authorization check, so it fails CLOSED: a
// store that cannot answer is not evidence that a state is unspent, and the
// cost of refusing is that an operator clicks again.
//
// THE DELIVERY REGISTRY, at this flow's own TTL. A claim carries its
// deadline in its record, so a quarter-hour claim shares the bucket with a
// five-minute delivery claim and lapses at its own deadline rather than the
// bucket's; a TTL past the registry's ceiling ([coord.MaxClaimTTL]) is an
// error, which this flow refuses on like any other.
type StateClaims interface {
	// Claim records key and reports whether THIS caller was first. It
	// lapses ttl after now, judged against the now the next caller passes.
	Claim(ctx context.Context, key string, ttl time.Duration, now time.Time) (bool, error)
}

// newAppFlow builds the completer the webhook mux serves. See
// [Options.StateCipher] and [Options.StateClaims] for why both have to be the
// fleet's rather than this process's.
func newAppFlow(s *Service, cipher secrets.Cipher, spent StateClaims) *AppFlow {
	return &AppFlow{service: s, cipher: cipher, spent: spent}
}

// AppFlow is the completer the webhook mux serves for a GitHub App creation
// this service began.
func (s *Service) AppFlow() *AppFlow { return s.appFlow }

// spend records a state as used, and reports whether this caller may use it.
//
// KEYED ON THE DIGEST, never the token: the fleet's registry is shared state
// and the state IS the credential on this route, so writing it there would
// put a live bearer token in a store read by every node.
//
// The claim outlives the token deliberately — taken at the TTL the state was
// minted for, measured from the moment it is spent — so a token cannot be
// replayed at any point while it would still open.
func (f *AppFlow) spend(ctx context.Context, state string) error {
	digest := sha256.Sum256([]byte(state))
	key := "github-app-state:" + hex.EncodeToString(digest[:])
	first, err := f.spent.Claim(ctx, key, manifestTTL, f.service.now())
	if err != nil {
		// CLOSED. See [StateClaims]: a registry that could not answer has
		// not told us this state is unspent.
		return fmt.Errorf("%w: this engine could not check whether the link "+
			"had already been used: %w", ErrStateRefused, err)
	}
	if !first {
		return ErrStateRefused
	}
	return nil
}

// beginApp serves POST /setup/integrations/github/app.
//
// It answers with what a browser needs to create the app and nothing it could
// not work out itself: the manifest, the address to POST it to, and the state
// that ties the answer back to this seat.
func (s *Service) beginApp(w http.ResponseWriter, r *http.Request) {
	// `secrets.write` ON TOP OF THE ROUTE'S VERB: the app this begins has
	// its private key and webhook secret sealed on the way back, and the
	// return is a browser redirect carrying no credential of ours — so
	// this is the one request that can ask whether the caller may write
	// them.
	if !s.mayWriteCredentials(w, r) {
		return
	}
	// A MANAGED DOCUMENT IS REFUSED HERE, before GitHub is asked for
	// anything: the app this begins is recorded on the seat, and a refusal
	// at the callback would come after GitHub had created the app and
	// issued its key, once. After the grants, so a caller who could never
	// connect anything is told the grant they lack rather than who manages
	// the document.
	if err := s.config.Authorize(attributionOf(r)); err != nil {
		s.refuse(w, r, err, setup.Result{})
		return
	}

	// THROUGH THE PACKAGE'S OWN CAP, like every other route here. A decoder
	// straight off r.Body reads whatever is sent: this route names one seat,
	// so the body is tens of bytes, and streaming an unbounded one into a
	// decoder is a route that can be made to consume memory by anyone who
	// can reach it.
	body, err := httpjson.ReadBody(w, r, MaxBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	var in struct {
		Seat string `json:"seat"`
	}
	if err = json.Unmarshal(body, &in); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": plainjson.Unreadable("the body", err)})
		return
	}
	handle := strings.TrimSpace(in.Seat)
	if handle == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeSeatRequired, map[string]string{
			"hint": "name the seat this app belongs to: one app is one agent's " +
				"identity, so there is no company-wide app to create",
		})
		return
	}

	fc, ok := s.companyFor(w, r, "no company configuration is active")
	if !ok {
		return
	}
	company := fc.company
	seat := seatByHandle(fc.roster, handle)
	if seat == nil {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchSeat, map[string]string{
			"hint": fmt.Sprintf("this company has no agent seat %q", handle),
		})
		return
	}

	tier, _ := github.ParseTier(seatTier(seat))
	base := s.publicBase()
	if base == "" {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNoExternalURL, map[string]string{
			"config_path": s.webhookBaseKey,
			"hint": "set " + s.webhookBaseKey + " first: the app is created " +
				"with its delivery address baked in, and only the operator can " +
				"change that afterwards, so creating one now would need doing again",
		})
		return
	}

	// WHO BEGAN IT RIDES THE STATE: the callback's writes are this
	// caller's gesture, and the callback carries no credential of ours.
	state, err := s.appFlow.mintState(handle, attributionOf(r))
	if err != nil {
		s.refuse(w, r, err, setup.Result{})
		return
	}
	// THE BROWSER COMES BACK HERE WHATEVER GITHUB CAN REACH. The redirect
	// and the setup page are opened by the operator's own browser, which
	// reaches a local engine perfectly well; only the DELIVERY address is
	// GitHub's to reach. So an address GitHub cannot deliver to creates the
	// app with no webhook rather than refusing it: the agent acts on
	// GitHub through it from the first install, and the card says that
	// nothing comes back ([github.Deliverable]).
	base = strings.TrimRight(base, "/")
	deliverable := github.Deliverable(base, s.enterpriseOf(company))
	manifest := github.BuildManifest(github.ManifestOptions{
		Seat:        handle,
		Name:        github.AppName(company.Name, seat.Name),
		DeliveryURL: base + "/webhooks/github/" + handle,
		DeliveryOff: !deliverable,
		RedirectURL: base + "/webhooks/github-app",
		SetupURL:    base + "/webhooks/github-app?installed=" + handle,
		Access:      github.Access{Tier: tier, Workflows: seatWorkflows(seat)},
	})
	httpjson.Write(w, http.StatusOK, map[string]any{
		"seat": handle,
		"tier": string(tier),
		// THE ACCOUNT THAT WILL OWN THE APP. An app registered under a
		// person's own account cannot be installed on the organization
		// that owns the repositories.
		"action_url": github.ActionURL(s.webBaseOf(company), orgOf(company)),
		"manifest":   manifest,
		"state":      state,
		// WHETHER THE APP WILL DELIVER EVENTS HERE, so a client can say
		// so before the operator confirms anything at GitHub.
		"webhook_active": deliverable,
	})
}

// ErrStateRefused reports a callback whose state was forged, expired or spent.
var ErrStateRefused = errors.New(
	"setupapi: this app-creation link is not one this engine issued, or it has expired")

// Complete finishes an app creation from GitHub's redirect.
//
// THE SEAL COMES FIRST, and everything else follows it. GitHub returns the
// private key and the webhook secret exactly once and reissues neither, so
// anything that can fail has to happen after they are durable. A failure
// after the seal costs a retry; a failure before it costs the app.
func (f *AppFlow) Complete(ctx context.Context, code, state string) (string, error) {
	opened, err := f.openState(state)
	if err != nil {
		return "", err
	}
	handle := opened.Seat
	// SPENT HERE, BEFORE THE EXCHANGE, which is what makes [ErrStateRefused]
	// mean what it has always said it means.
	//
	// The state is the ONLY authorization on this route — it is served by the
	// unauthenticated webhooks mux — and opening it is a pure seal and
	// expiry check, so without this it is a bearer credential that works as
	// many times as it is presented for a full [manifestTTL]. It travels in a
	// query string, which is where browser history and every ingress access
	// log keep it.
	//
	// BEFORE the exchange rather than after, and that costs nothing: GitHub's
	// manifest code is itself one-time, so a conversion that fails needs a
	// fresh code and therefore a fresh creation either way. Spending first
	// means a replay cannot race a slow exchange.
	if err = f.spend(ctx, state); err != nil {
		return handle, err
	}
	s := f.service
	fc, err := s.readCompany(ctx)
	if errors.Is(err, configapi.ErrNoActiveRevision) || (err == nil && seatByHandle(fc.roster, handle) == nil) {
		return handle, fmt.Errorf("setupapi: this company has no agent seat %q", handle)
	}
	if err != nil {
		return handle, fmt.Errorf("setupapi: read the company the app is for: %w", err)
	}
	company := fc.company
	// ASKED AGAIN BEFORE THE EXCHANGE, which is the last moment a refusal
	// costs nothing: the begin route asked on the node that minted the
	// state, and the callback may land on a node whose Tier A names the
	// writers differently, or on this one after a restart that changed them.
	// Asked of the party the SEALED state names, because the callback
	// carries no credential of its own and its writes are that party's.
	by := opened.party()
	if err = s.config.Authorize(by); err != nil {
		return handle, err
	}

	// DETACHED FROM THE BROWSER, which is the same move [Service.runPass]
	// makes and for a sharper reason. Everything below is irreversible: the
	// conversion spends GitHub's one-time code, and the two values it returns
	// are issued once and never reissued. Run on the request's own context, a
	// person closing the tab — or a proxy timing the request out — cancels
	// the engine between GitHub creating the app and the key being sealed,
	// and that app is then unusable and unrecoverable, deletable only by hand
	// at GitHub.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), completeDeadline)
	defer cancel()

	app, err := github.ExchangeManifest(ctx, f.service.apiBaseOf(company), code)
	if err != nil {
		return handle, err
	}

	// SEALED BEFORE ANYTHING ELSE CAN FAIL. See the note above: these two
	// values do not exist anywhere else and cannot be asked for again.
	keyVar := github.SeatVar(handle, github.FieldAppKey)
	hookVar := github.SeatVar(handle, github.FieldAppWebhookSecret)
	// THROUGH s.now(), which is the guarded reading. Calling s.clock()
	// straight panicked here on the one path where a panic costs an app:
	// GitHub had created it, and the crash landed before its key was
	// sealed, so the key was gone for good.
	now := s.now()
	// THE BEGINNER'S, both writes: the key's row and the seat's record name
	// who began the creation and the credential they began it through.
	if err := s.secrets.Set(ctx, keyVar, app.PEM, authorOf(by), secrets.SourceSetup, now); err != nil {
		return handle, fmt.Errorf("setupapi: seal the app key for %s: %w", handle, err)
	}
	sealedHook := strings.TrimSpace(app.WebhookSecret) != ""
	if sealedHook {
		if err := s.secrets.Set(ctx, hookVar, app.WebhookSecret, authorOf(by), secrets.SourceSetup, now); err != nil {
			return handle, fmt.Errorf("setupapi: seal the webhook secret for %s: %w", handle, err)
		}
	}

	// THE POINTER ONLY WHERE THERE IS SOMETHING TO POINT AT. A `${VAR}`
	// naming a secret nothing sealed resolves to nothing, and the webhook
	// route reads an empty secret as "cannot verify" and answers 503, so
	// writing it unconditionally would turn an app GitHub gave no secret
	// into a seat whose deliveries are refused rather than one that falls
	// back to the organization's.
	hookRef := ""
	if sealedHook {
		hookRef = "${" + hookVar + "}"
	}
	if err := s.recordSeatApp(ctx, handle, app, keyVar, hookRef, by); err != nil {
		return handle, err
	}
	return handle, nil
}

// recordSeatApp writes the app onto the seat, through the entity route.
//
// THE ENTITY ROUTE, not a merge patch: a patch replaces an array wholesale,
// so patching `roles` to change one seat would delete every other one. The
// entity route addresses a seat by its handle.
func (s *Service) recordSeatApp(
	ctx context.Context, handle string, app *github.CreatedApp, keyVar, hookRef string,
	by iam.Actor,
) error {
	body, err := s.writer.Config.Seat(ctx, handle)
	if err != nil {
		return fmt.Errorf("setupapi: read the seat %s: %w", handle, err)
	}
	var role map[string]any
	if decodeErr := json.Unmarshal(body, &role); decodeErr != nil {
		return fmt.Errorf("setupapi: decode the seat %s: %w", handle, decodeErr)
	}
	integrations, _ := role["integrations"].(map[string]any)
	if integrations == nil {
		integrations = map[string]any{}
	}
	block, _ := integrations["github"].(map[string]any)
	if block == nil {
		block = map[string]any{}
	}
	block["app_id"] = app.ID
	block["app_slug"] = app.Slug
	block["private_key"] = "${" + keyVar + "}"
	// THE APP'S OWN SIGNING SECRET, and the route needs it to accept a
	// single delivery: GitHub signs this app's deliveries with the secret
	// it generated for THIS app, which is not the organization's.
	if hookRef != "" {
		block["webhook_secret"] = hookRef
	}
	// THE INSTALLATION IS NOT KNOWN YET, and saying so is the point:
	// creating an app and installing it are two acts, and the second can
	// be a day after the first. Zero is the state the screen reports as
	// "installed nowhere yet", which is a thing an operator can act on.
	block["installation_id"] = 0
	integrations["github"] = block
	role["integrations"] = integrations

	updated, err := json.Marshal(role)
	if err != nil {
		return fmt.Errorf("setupapi: encode the seat %s: %w", handle, err)
	}
	_, _, err = s.writer.Config.SetSeat(ctx, handle, updated,
		"give "+handle+" its own GitHub App", by, "")
	if err != nil {
		return fmt.Errorf("setupapi: record the app for %s: %w", handle, err)
	}
	return nil
}

// authorOf is a party as the secret store records an author.
func authorOf(by iam.Actor) secrets.Author {
	return secrets.Author{Name: by.Name, Kind: string(by.Kind), OperatorID: by.OperatorID}
}

// InstallURL is where the operator installs the app a seat now has.
//
// BUILT FROM THE SLUG GITHUB RETURNED, never from the name that was asked
// for: GitHub slugifies a name and disambiguates a collision, so the app that
// exists may not be the one whose name was requested.
func (f *AppFlow) InstallURL(ctx context.Context, handle string) string {
	fc, err := f.service.readCompany(ctx)
	if err != nil {
		return ""
	}
	company := fc.company
	seat := seatByHandle(fc.roster, handle)
	if seat == nil || seat.GitHub == nil {
		return ""
	}
	slug := strings.TrimSpace(seat.GitHub.AppSlug)
	if slug == "" {
		return ""
	}
	return github.InstallURL(f.service.webBaseOf(company), orgOf(company), slug)
}

// seatByHandle finds one agent seat in the fleet's company ([fleetCompany]).
//
// AN INDEX ON THE ORG, not a walk of the document: a stored revision carries
// no seats at all, so the walk this replaced answered "no such seat" for every
// agent — which this route reports as a handle nobody holds.
func seatByHandle(roster *org.Organization, handle string) *org.Role {
	if roster == nil {
		return nil
	}
	if role := roster.Role(handle); role != nil && role.IsAgent() {
		return role
	}
	return nil
}

// seatTier is the access tier this seat runs at, as written down.
//
// THE RAW VALUE, unparsed. Every caller passes it to [github.ParseTier], which
// already answers the default for an empty string and for a typo and is the
// package that owns the closed set — so a second "or default" here would be a
// place for the two to disagree about what an unrecognised tier runs at.
func seatTier(seat *org.Role) string {
	if seat == nil || seat.GitHub == nil {
		return ""
	}
	return seat.GitHub.Tier
}

// seatWorkflows is whether this seat opts in to workflow writes, as written
// down. [github.Access] decides what that grants at the seat's tier, so the
// raw value is all a caller needs.
func seatWorkflows(seat *org.Role) bool {
	return seat != nil && seat.GitHub != nil && seat.GitHub.Workflows
}

// orgOf is the organization whose repositories these agents work in.
func orgOf(company *config.Company) string {
	gh := company.Integrations.GitHub
	if gh == nil || gh.Provisioning == nil {
		return ""
	}
	return strings.TrimSpace(gh.Provisioning.Org)
}

// webBaseOf is the host a person's browser opens, which is github.com unless
// this company runs Enterprise Server.
func (s *Service) webBaseOf(company *config.Company) string {
	_, web := company.Integrations.GitHub.Bases(s.resolve)
	return web
}

// enterpriseOf reports a company on Enterprise Server rather than
// github.com, which decides what [github.Deliverable] counts as reachable.
func (s *Service) enterpriseOf(company *config.Company) bool {
	gh := company.Integrations.GitHub
	if gh == nil {
		return false
	}
	url, _ := envref.Expand(gh.URL, s.resolve)
	return strings.TrimSpace(url) != ""
}

// apiBaseOf is the REST base the conversion is POSTed to.
//
// NOT THE BROWSER BASE, which is what this returned. They are the same string
// only on github.com: Enterprise Server serves its REST API under `/api/v3`,
// so the manifest conversion was POSTed to a path that answers 404 — and
// GitHub's manifest code is one-time, so the operator was told their code was
// already spent and had to create the app again, every time.
func (s *Service) apiBaseOf(company *config.Company) string {
	api, _ := company.Integrations.GitHub.Bases(s.resolve)
	return api
}

// publicBase is the address a third-party app reaches this engine on.
//
// EVERY ADDRESS BUILT FROM IT HERE IS BAKED INTO AN APP AT GITHUB — the
// delivery URL, the redirect and the setup URL — and only a person can change
// those afterwards, so a `${PUBLIC_URL}` copied in literally creates an app
// nothing can ever deliver to. That was reachable while the address was a
// Tier B pointer; Tier A is resolved before it is decoded, so the literal has
// nowhere to come from. The VENDOR's address ([Options.WebhookBase]), since
// GitHub delivers to it and redirects the browser back onto it.
func (s *Service) publicBase() string { return s.webhookBase }
