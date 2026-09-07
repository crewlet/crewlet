package setupapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/runtoken"
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
}

// NewAppFlow builds the completer the webhook mux serves.
//
// Material keys the state signer, and MUST be the same in every node that
// mints or validates one: a fleet where the begin and the callback land on
// different nodes would otherwise refuse every completion. Empty material
// takes a per-process key, which is correct for one node and cannot work
// across two, and the caller logs what that costs.
func NewAppFlow(s *Service, material []string) *AppFlow {
	if s == nil {
		return nil
	}
	return &AppFlow{
		service: s,
		signer: runtoken.New(runtoken.Options{
			Key: runtoken.KeyFrom(tokenDomain, material),
			Now: s.clock,
		}),
	}
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
	var in struct {
		Seat string `json:"seat"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
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
		"action_url": github.ActionURL(webBaseOf(company), orgOf(company)),
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
	s := f.service
	company := s.company()
	if company == nil || seatByHandle(company, handle) == nil {
		return handle, fmt.Errorf("setupapi: this company has no agent seat %q", handle)
	}

	app, err := github.ExchangeManifest(ctx, apiBaseOf(company), code)
	if err != nil {
		return handle, err
	}

	// SEALED BEFORE ANYTHING ELSE CAN FAIL. See the note above: these two
	// values do not exist anywhere else and cannot be asked for again.
	keyVar := secretNameFor(handle, "GITHUB_APP_KEY")
	hookVar := secretNameFor(handle, "GITHUB_APP_WEBHOOK_SECRET")
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
	// route reads an empty secret as "cannot verify" and answers 503 — so
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
	return github.InstallURL(webBaseOf(company), orgOf(company), slug)
}

// RecordInstall adopts an installation GitHub named on its redirect.
//
// THE SAME WRITE THE LOOP MAKES, through the same entity route, so the two
// paths cannot disagree about where an installation is recorded. What it buys
// is timing: the loop would find this id by listing the app's installations
// on its next pass, and doing it here means the Integrations screen is right
// when the operator gets back to it.
//
// It refuses an id for a seat with no app, because that pairing cannot be
// true: an installation belongs to an app, so a seat without one has nothing
// to install.
func (f *AppFlow) RecordInstall(ctx context.Context, handle string, installationID int64) error {
	if installationID <= 0 {
		return fmt.Errorf("setupapi: %q is not an installation id", handle)
	}
	s := f.service
	company := s.company()
	if company == nil {
		return errors.New("setupapi: no company configuration is active")
	}
	seat := seatByHandle(company, handle)
	if seat == nil {
		return fmt.Errorf("setupapi: this company has no agent seat %q", handle)
	}
	if seat.Integrations.GitHub == nil || seat.Integrations.GitHub.AppID == 0 {
		return fmt.Errorf(
			"setupapi: %s has no GitHub App, so there is nothing for an "+
				"installation to belong to", handle)
	}
	return s.recordSeatInstallation(ctx, handle, installationID)
}

// recordSeatInstallation writes the installation onto the seat.
func (s *Service) recordSeatInstallation(ctx context.Context, handle string, id int64) error {
	body, err := s.writer.Config.Seat(ctx, handle)
	if err != nil {
		return fmt.Errorf("setupapi: read the seat %s: %w", handle, err)
	}
	var role map[string]any
	if decodeErr := json.Unmarshal(body, &role); decodeErr != nil {
		return fmt.Errorf("setupapi: decode the seat %s: %w", handle, decodeErr)
	}
	integrations, _ := role["integrations"].(map[string]any)
	block, _ := integrations["github"].(map[string]any)
	if block == nil {
		return fmt.Errorf("setupapi: the seat %s has no github block", handle)
	}
	block["installation_id"] = id
	integrations["github"] = block
	role["integrations"] = integrations

	updated, err := json.Marshal(role)
	if err != nil {
		return fmt.Errorf("setupapi: encode the seat %s: %w", handle, err)
	}
	_, _, err = s.writer.Config.SetSeat(ctx, handle, updated,
		"record "+handle+"'s GitHub installation", "setup", "")
	if err != nil {
		return fmt.Errorf("setupapi: record the installation for %s: %w", handle, err)
	}
	return nil
}

// secretNameFor is the sealed-store name one seat's value lives under.
//
// PER SEAT, because these are per-seat credentials: one shared name would
// have the second agent's app key overwrite the first's, and both seats would
// then authenticate as whichever app was created last.
func secretNameFor(handle, suffix string) string {
	return setupSlug(handle) + "_" + suffix
}

// setupSlug upper-snakes a handle into the reference grammar, so the name it
// produces is one a `${VAR}` can actually resolve through.
func setupSlug(handle string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(handle)) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
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
func webBaseOf(company *config.Company) string {
	gh := company.Integrations.GitHub
	if gh == nil {
		return ""
	}
	return strings.TrimSpace(gh.URL)
}

// apiBaseOf is the REST base the conversion is POSTed to.
func apiBaseOf(company *config.Company) string {
	return webBaseOf(company)
}

// publicBase is the address a third-party app reaches this engine on.
func (s *Service) publicBase() string {
	company := s.company()
	if company == nil {
		return ""
	}
	return strings.TrimSpace(company.Integrations.PublicBaseURL)
}
