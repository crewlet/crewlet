package setupapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/runtoken"
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
// anybody who can reach the engine is the state, and it is a signed token
// naming the seat and its expiry, minted here and validated there. Slack's
// landing carries a handle for display and checks nothing, which is fine for
// a page that only prints a code and would not be fine here: this callback
// writes a credential into a seat.

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
// GitHub expires the one-time code at one hour, so a state that outlived it
// would let somebody complete a flow whose code is already dead: the operator
// sees a failure naming the code rather than the wait. Fifteen minutes is the
// span of the actual task, which is a browser round trip and two clicks.
const manifestTTL = 15 * time.Minute

// tokenDomain separates these tokens from every other signed URL this engine
// issues, so a token minted for the OTLP receiver cannot be replayed here.
const tokenDomain = "github-app-manifest"

// AppFlow is what the callback needs to finish an app creation.
//
// A SEPARATE TYPE from the service because the callback is served by the
// webhooks mux, which is unauthenticated and knows nothing about setup. It is
// handed this and nothing else.
type AppFlow struct {
	service *Service
	signer  *runtoken.Signer
	spent   StateClaims
}

// StateClaims is what spends a callback state, so no state is ever accepted
// twice.
//
// The consumer's own interface, one method wide: this package needs
// first-claim-wins and nothing else. The fleet's [coord.Claims] satisfies it,
// and reading it here INVERTS that type's documented policy on purpose.
// Webhook dedupe fails OPEN, because a push suppressed by a store blip is a
// wake nobody notices. This is an authorization check, so it fails CLOSED: a
// store that cannot answer is not evidence that a state is unspent, and the
// cost of refusing is that an operator clicks again.
type StateClaims interface {
	// Claim records key and reports whether THIS caller was first.
	Claim(ctx context.Context, key string, ttl time.Duration, now time.Time) (bool, error)
}

// NewAppFlow builds the completer the webhook mux serves.
//
// Material keys the state signer, and MUST be the same in every node that
// mints or validates one: a fleet where the begin and the callback land on
// different nodes would otherwise refuse every completion. Empty material
// takes a per-process key, which is correct for one node and cannot work
// across two, and the caller logs what that costs.
//
// spent is where a used state is recorded, and it has the SAME fleet
// requirement for the same reason: a callback landing on a node that cannot
// see the first one's record would accept a replay. Nil takes a per-process
// set, correct for one node and no more — exactly the trade the key material
// above makes.
func NewAppFlow(s *Service, material []string, spent StateClaims) *AppFlow {
	if s == nil {
		return nil
	}
	if spent == nil {
		spent = &localClaims{seen: map[string]time.Time{}}
	}
	return &AppFlow{
		service: s,
		spent:   spent,
		signer: runtoken.New(runtoken.Options{
			Key: runtoken.KeyFrom(tokenDomain, material),
			Now: s.clock,
		}),
	}
}

// localClaims is the single-node stand-in for the fleet's registry.
//
// Bounded by the same TTL the fleet row carries, swept on write rather than
// on a timer: a state is spent at most once per app creation, so the map
// holds one entry per creation for fifteen minutes and there is no loop worth
// running to keep it smaller.
type localClaims struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (l *localClaims) Claim(_ context.Context, key string, ttl time.Duration, now time.Time) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, expiry := range l.seen {
		if !now.Before(expiry) {
			delete(l.seen, k)
		}
	}
	if expiry, held := l.seen[key]; held && now.Before(expiry) {
		return false, nil
	}
	l.seen[key] = now.Add(ttl)
	return true, nil
}

// spend records a state as used, and reports whether this caller may use it.
//
// KEYED ON THE DIGEST, never the token: the fleet's registry is shared state
// and the state IS the credential on this route, so writing it there would
// put a live bearer token in a store read by every node.
//
// The claim outlives the token deliberately — the same TTL the state was
// minted for, measured from the moment it is spent — so a token cannot be
// replayed at any point while it would still validate.
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

// AttachAppFlow gives the service the signer its begin route needs.
//
// SET AFTER CONSTRUCTION because the flow holds the service: the callback has
// to reach the writer and the secret store, and the service has to know
// whether a flow exists at all so its begin route can refuse honestly rather
// than mint a state nothing will validate.
func (s *Service) AttachAppFlow(f *AppFlow) {
	if s != nil {
		s.appFlow = f
	}
}

// beginApp serves POST /setup/integrations/github/app.
//
// It answers with what a browser needs to create the app and nothing it could
// not work out itself: the manifest, the address to POST it to, and the state
// that ties the answer back to this seat.
func (s *Service) beginApp(w http.ResponseWriter, r *http.Request) {
	if s.appFlow == nil {
		httpjson.FailWith(w, http.StatusServiceUnavailable, codeNoAppFlow, map[string]string{
			"hint": "this process has no signing material for the callback, so a " +
				"browser returning from GitHub could not be tied back to the seat " +
				"that started",
		})
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
	if err := json.Unmarshal(body, &in); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, codeBadBody, map[string]string{"hint": err.Error()})
		return
	}
	handle := strings.TrimSpace(in.Seat)
	if handle == "" {
		httpjson.FailWith(w, http.StatusBadRequest, codeSeatRequired, map[string]string{
			"hint": "name the seat this app belongs to: one app is one agent's " +
				"identity, so there is no company-wide app to create",
		})
		return
	}

	company := s.company()
	if company == nil {
		httpjson.FailWith(w, http.StatusConflict, codeNoActiveRevision, map[string]string{
			"hint": "no company configuration is active",
		})
		return
	}
	seat := seatByHandle(company, handle)
	if seat == nil {
		httpjson.FailWith(w, http.StatusNotFound, codeNoSuchSeat, map[string]string{
			"hint": fmt.Sprintf("this company has no agent seat %q", handle),
		})
		return
	}

	tier, _ := github.ParseTier(seatTier(seat))
	base := s.publicBase()
	if base == "" {
		httpjson.FailWith(w, http.StatusConflict, codeNoPublicURL, map[string]string{
			"hint": "set integrations.public_base_url first: the app is created " +
				"with its delivery address baked in, and only the operator can " +
				"change that afterwards, so creating one now would need doing again",
		})
		return
	}

	state := s.appFlow.signer.Mint(handle, manifestTTL)
	manifest := github.BuildManifest(github.ManifestOptions{
		Seat:        handle,
		Name:        github.AppName(company.Name, seat.Name),
		DeliveryURL: strings.TrimRight(base, "/") + "/webhooks/github/" + handle,
		RedirectURL: strings.TrimRight(base, "/") + "/webhooks/github-app",
		SetupURL:    strings.TrimRight(base, "/") + "/webhooks/github-app?installed=" + handle,
		Tier:        tier,
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
	handle := f.signer.Validate(strings.TrimSpace(state))
	if handle == "" {
		return "", ErrStateRefused
	}
	// SPENT HERE, BEFORE THE EXCHANGE, which is what makes [ErrStateRefused]
	// mean what it has always said it means.
	//
	// The state is the ONLY authorization on this route — it is served by the
	// unauthenticated webhooks mux — and validating it is a pure signature
	// and expiry check, so without this it is a bearer credential that works
	// as many times as it is presented for a full [manifestTTL]. It travels
	// in a query string, which is where browser history and every ingress
	// access log keep it.
	//
	// BEFORE the exchange rather than after, and that costs nothing: GitHub's
	// manifest code is itself one-time, so a conversion that fails needs a
	// fresh code and therefore a fresh creation either way. Spending first
	// means a replay cannot race a slow exchange.
	if err := f.spend(ctx, state); err != nil {
		return handle, err
	}
	s := f.service
	company := s.company()
	if company == nil || seatByHandle(company, handle) == nil {
		return handle, fmt.Errorf("setupapi: this company has no agent seat %q", handle)
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
	keyVar := secretNameFor(handle, "APP_KEY")
	hookVar := secretNameFor(handle, "APP_WEBHOOK_SECRET")
	// THROUGH s.now(), which is the guarded reading. Calling s.clock()
	// straight panicked here on the one path where a panic costs an app:
	// GitHub had created it, and the crash landed before its key was
	// sealed, so the key was gone for good.
	now := s.now()
	if err := s.secrets.Set(ctx, keyVar, app.PEM, "setup", "setup", now); err != nil {
		return handle, fmt.Errorf("setupapi: seal the app key for %s: %w", handle, err)
	}
	sealedHook := strings.TrimSpace(app.WebhookSecret) != ""
	if sealedHook {
		if err := s.secrets.Set(ctx, hookVar, app.WebhookSecret, "setup", "setup", now); err != nil {
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
	if err := s.recordSeatApp(ctx, handle, app, keyVar, hookRef); err != nil {
		return handle, err
	}
	return handle, nil
}

// recordSeatApp writes the app onto the seat, through the entity route.
//
// THE ENTITY ROUTE, not a merge patch: a patch replaces an array wholesale,
// so patching `roles` to change one seat would delete every other one. The
// handle is the seat's identity rather than its position, which is exactly
// what this route addresses by.
func (s *Service) recordSeatApp(
	ctx context.Context, handle string, app *github.CreatedApp, keyVar, hookRef string,
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
		"give "+handle+" its own GitHub App", "setup", "")
	if err != nil {
		return fmt.Errorf("setupapi: record the app for %s: %w", handle, err)
	}
	return nil
}

// InstallURL is where the operator installs the app a seat now has.
//
// BUILT FROM THE SLUG GITHUB RETURNED, never from the name that was asked
// for: GitHub slugifies a name and disambiguates a collision, so the app that
// exists may not be the one whose name was requested.
func (f *AppFlow) InstallURL(handle string) string {
	company := f.service.company()
	if company == nil {
		return ""
	}
	seat := seatByHandle(company, handle)
	if seat == nil || seat.Integrations.GitHub == nil {
		return ""
	}
	slug := strings.TrimSpace(seat.Integrations.GitHub.AppSlug)
	if slug == "" {
		return ""
	}
	return github.InstallURL(f.service.webBaseOf(company), orgOf(company), slug)
}

// secretNameFor is the sealed-store name one seat's value lives under.
//
// PER SEAT, because these are per-seat credentials: one shared name would
// have the second agent's app key overwrite the first's, and both seats would
// then authenticate as whichever app was created last.
//
// THROUGH THE SHARED GRAMMAR, and the ORDER is why. This built the name as
// `<HANDLE>_<FIELD>`, so a handle beginning with a digit — `7th-engineer`,
// which the org model accepts — produced `7TH_ENGINEER_GITHUB_APP_KEY`. That
// is not a name a `${VAR}` can reference: envref's whole-reference grammar
// requires a leading letter or underscore. The pointer written beside it
// therefore resolved to nothing, and the value it pointed at was the app's
// private key, which GitHub issues exactly once and never reissues — so the
// app was unusable and unrecoverable the moment it was created.
//
// [setup.SecretNameFor] puts the constant first, which makes a leading letter
// structural rather than something each caller has to remember.
func secretNameFor(handle, field string) string {
	return setup.SecretNameFor(integration.KindGitHub, setup.Requirement{
		Field: field, Seat: handle,
	})
}

// seatByHandle finds one agent seat.
func seatByHandle(company *config.Company, handle string) *config.Role {
	for role := range company.EachRole() {
		if role.Seat().Handle() == handle && role.Seat().IsAgent() {
			return role
		}
	}
	return nil
}

// seatTier is the access tier this seat runs at, as written down.
func seatTier(seat *config.Role) string {
	if seat.Integrations.GitHub == nil {
		return ""
	}
	return seat.Integrations.GitHub.TierOrDefault()
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
// RESOLVED, because every address built from it here is baked into an app at
// GitHub — the delivery URL, the redirect and the setup URL — and only a
// person can change those afterwards. A `${PUBLIC_URL}` copied in literally
// creates an app nothing can ever deliver to.
func (s *Service) publicBase() string {
	company := s.company()
	if company == nil {
		return ""
	}
	return company.Integrations.WebhookBase(s.resolve)
}
