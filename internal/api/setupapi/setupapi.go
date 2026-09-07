// Package setupapi serves /setup: connecting an integration from the
// dashboard rather than from a shell.
//
// # Why it is not part of /integrations
//
// `GET /integrations` is registered as an ordinary read, so on the default
// posture it serves without a token. What this surface answers is a different
// class of thing: the NAMES of the credentials a company holds, which are
// unset, the third-party app pages an administrator would visit, and the fields a
// caller can write. Adding that to the anonymous-read answer would hand an
// unauthenticated reader a map of what to attack. So it is its own prefix,
// added to the always-guarded list beside /config and /secrets, and every
// call here needs an operator token, reads included.
//
// # It never returns a value
//
// A requirement says whether a value is present and whether it resolved. It
// never carries the value, and no route here reads one: the single route in
// this binary that can return a secret needs an explicit flag and logs the
// access, and a dashboard anybody holding the token can open is not where
// that trade gets made. What IS safe to show, and what makes the screen
// useful, is the `${VAR}` name a field points at.
package setupapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/slack"
)

var log = logging.Get("api.setup")

// PassDeadline bounds one provisioning pass.
//
// Just inside the fleet lease a pass holds (internal/engine's
// setupLeaseTTL, 5 minutes), because the lease is what stops two operators
// minting at the same third-party app at once and it is not renewed mid-pass. A pass
// that outlived it would still be writing at the third-party app with nothing left
// holding anyone else off.
const PassDeadline = 4 * time.Minute

// MaxBody bounds one submission.
//
// Small on purpose: the largest thing a submission carries is a third-party app API
// token, and every one of those is under a kilobyte. A cap this size makes
// the route uninteresting as a way to spend memory, and a caller that hits it
// has sent the wrong thing rather than a large one.
const MaxBody = 64 << 10

// The refusals this surface answers with, beyond the shared ones.
const (
	codeUnknownKind      = httpjson.Code("unknown_kind")
	codeNoControlPlane   = httpjson.Code("no_control_plane")
	codeNoActiveRevision = httpjson.Code("no_active_revision")
	codeNoAppFlow        = httpjson.Code("no_app_flow")
	codeBadBody          = httpjson.Code("bad_body")
	codeSeatRequired     = httpjson.Code("seat_required")
	codeNoSuchSeat       = httpjson.Code("no_such_seat")
	codeNoPublicURL      = httpjson.Code("no_public_url")
	codeRevisionAdvanced = httpjson.Code("revision_advanced")
	codeLiteralInConfig  = httpjson.Code("literal_in_config")
	codeValidationError  = httpjson.Code("validation_error")
	codeInvalidInput     = httpjson.Code("invalid_input")
	codeNoKeyring        = httpjson.Code("no_keyring")
	// codeNoStatusStore is a node with no fleet row to record a disconnect
	// on. Distinct from no_keyring, which is about sealing a credential:
	// the two are different missing pieces and lead to different advice.
	codeNoStatusStore = httpjson.Code("no_status_store")
)

// Options wire the service.
type Options struct {
	// Company reads the ACTIVE document. Nil serves no surface: with no
	// company there is nothing to describe and nothing to patch.
	Company func() *config.Company

	// Config is the write path, the same one PATCH /config drives.
	Config *configapi.Service

	// Secrets seals a submitted credential. Nil is a node with no
	// keyring, which every secret write refuses rather than storing
	// plaintext.
	Secrets setup.Secrets

	// Resolve turns a ${VAR} name into what this process actually
	// resolved. Nil is a process that cannot say, and every requirement
	// then answers `resolved: null` rather than claiming false.
	Resolve func(string) (string, bool)

	// Passes are the third-party apps this build can provision over the API. Nil
	// serves the three pass routes as "not provisionable", which is the
	// honest answer on a node with no secret store to mint into.
	Passes *setup.Runner

	// Sink builds the recorder a pass writes minted credentials through.
	Sink func(operator string) (provision.TokenSink, error)

	// Status is where a pass records what it found: the SAME fleet row the
	// reconcile loop writes, so the two cannot disagree.
	Status Status

	// Now is injectable so a test can pin a secret row's timestamp.
	Now func() time.Time
}

// Service serves /setup.
type Service struct {
	company func() *config.Company
	config  *configapi.Service
	writer  setup.Writer
	resolve func(string) (string, bool)
	secrets setup.Secrets
	passes  *setup.Runner
	sink    sinkFactory
	status  Status
	clock   func() time.Time
	appFlow *AppFlow
}

// New builds the service, or nil when this process has no company to set up.
func New(opts Options) *Service {
	if opts.Company == nil {
		return nil
	}
	return &Service{
		company: opts.Company,
		config:  opts.Config,
		resolve: opts.Resolve,
		secrets: opts.Secrets,
		passes:  opts.Passes,
		sink:    opts.Sink,
		status:  opts.Status,
		clock:   opts.Now,
		writer: setup.Writer{
			Secrets: opts.Secrets,
			Config:  configWriter{opts.Config},
			Now:     opts.Now,
		},
	}
}

// Routes registers the surface, or says why it did not.
func (s *Service) Routes(mux *http.ServeMux) {
	if s == nil {
		log.Warn("setup_surface_disabled",
			"hint", "this process serves no company configuration, so /setup is not served here")
		return
	}
	mux.HandleFunc("GET /setup/integrations", s.list)
	mux.HandleFunc("GET /setup/integrations/{kind}", s.one)
	mux.HandleFunc("POST /setup/integrations/{kind}/inputs", s.inputs)
	mux.HandleFunc("DELETE /setup/integrations/{kind}", s.disconnect)
	// The two that RUN something at the third-party app, and the read that follows
	// one. See pass.go for why check and provision are one function.
	mux.HandleFunc("POST /setup/integrations/{kind}/provision", s.provision)
	mux.HandleFunc("POST /setup/integrations/{kind}/check", s.check)
	// ONE AGENT'S OWN APP. Not a company-wide connect: a GitHub App is one
	// bot identity, so an app per agent is the only way each acts as itself.
	mux.HandleFunc("POST /setup/integrations/github/app", s.beginApp)
	mux.HandleFunc("GET /setup/integrations/{kind}/runs/{id}", s.runByID)
}

// configWriter adapts the config surface to what setup.Writer needs.
//
// The interface is the CONSUMER's — three strings and a patch — so the setup
// package does not import an HTTP service to perform a write, and a test can
// drive it with something that is not one.
type configWriter struct{ svc *configapi.Service }

func (c configWriter) Apply(
	ctx context.Context, patch []byte, summary, operator, expect string,
) (string, int64, error) {
	applied, err := c.svc.Apply(ctx, configapi.ApplyRequest{
		Patch: patch, Summary: summary, Operator: operator, Expect: expect,
	})
	return applied.RevisionID, applied.Epoch, err
}

func (c configWriter) Current(ctx context.Context) (string, error) {
	return c.svc.ActiveRevision(ctx)
}

func (c configWriter) Reload(ctx context.Context, summary, operator string) (string, int64, error) {
	applied, err := c.svc.Reload(ctx, summary, operator)
	return applied.RevisionID, applied.Epoch, err
}

// Seat and SetSeat are the per-seat write, through the entity route: a seat
// is addressed by its handle, because a merge patch cannot reach one element
// of a list without replacing the list.
func (c configWriter) Seat(ctx context.Context, handle string) ([]byte, error) {
	entity, err := c.svc.Entity(ctx, "roles", handle)
	if err != nil {
		return nil, err
	}
	return json.Marshal(entity)
}

func (c configWriter) SetSeat(
	ctx context.Context, handle string, body []byte, summary, operator, expect string,
) (string, int64, error) {
	applied, err := c.svc.ApplyEntity(ctx, configapi.ApplyEntityRequest{
		Kind: "roles", ID: handle, Body: body,
		Summary: summary, Operator: operator, Expect: expect,
	})
	return applied.RevisionID, applied.Epoch, err
}

// ToolState is one integration's setup state.
//
// The reconcile half is deliberately NOT here. That answer already exists on
// GET /integrations, built from the fleet's own status rows, and a second
// surface deriving it from the same inputs is how two screens start
// disagreeing about whether an integration is healthy. This one answers the
// question only it can: what is still missing, and where does it go.
type ToolState struct {
	Kind         integration.Kind    `json:"key"`
	Configured   bool                `json:"configured"`
	Enabled      bool                `json:"enabled"`
	Requirements []setup.Requirement `json:"requirements"`

	// Summary is one sentence saying what connecting this app DOES, which
	// the connect form opens with. It lives with the app rather than on
	// the screen for the reason every other word here does: the dashboard
	// knows nothing about any app, so adding one is a Go change and no
	// screen work.
	Summary string `json:"summary,omitempty"`

	// Satisfied reports that nothing REQUIRED is outstanding. It is not a
	// health claim: a satisfied integration can still be refusing every
	// delivery for a reason no input fixes.
	Satisfied bool `json:"satisfied"`

	// InboundPath is where this third-party app's deliveries arrive, and PublicURL
	// is that path on the address third-party apps reach this deployment at. Empty
	// when the surface has no inbound route, or when no base is set.
	InboundPath string `json:"inbound_path,omitempty"`
	PublicURL   string `json:"public_url,omitempty"`

	// CanProvision reports that this build runs a provisioning pass for
	// this third-party app, so the screen offers the button rather than discovering
	// on a press that there is nothing behind it.
	CanProvision bool `json:"can_provision"`

	// SeatsRequired is whether a seat without its own credential makes this
	// app unfinished.
	//
	// TRUE FOR SLACK ALONE, because an agent with no Slack app cannot post
	// at all: the roster IS the integration there. Everywhere else a seat
	// credential is an upgrade on a working app — Datadog routes alerts with
	// no agent accounts, a Confluence seat without one searches as the org
	// account — so a roster with nothing in it is a company's choice rather
	// than an unfinished setup, and reporting it as work left to do put a
	// Continue button on every connected card.
	SeatsRequired bool `json:"seats_required,omitempty"`

	// NeedsOperator is the transient third-party app administrator credential the
	// pass asks for on every run, or null. Never stored.
	NeedsOperator *setup.Requirement `json:"needs_operator,omitempty"`

	// Seats are the per-seat requirement lists, for a third-party app whose
	// credentials live on the seat rather than on the company. Slack is
	// the one: each agent has its own app, so each has its own bot token
	// and signing secret, and a submission names the seat it is for.
	Seats []SeatState `json:"seats,omitempty"`
}

// SeatState is one seat's setup for a per-seat third-party app.
type SeatState struct {
	Handle       string              `json:"handle"`
	Name         string              `json:"name,omitempty"`
	Requirements []setup.Requirement `json:"requirements"`
	Satisfied    bool                `json:"satisfied"`

	// InboundPath is where this SEAT's deliveries arrive, which on a
	// per-seat third-party app differs per seat, and PublicURL is that path on the
	// address third-party apps reach this deployment at.
	InboundPath string `json:"inbound_path,omitempty"`
	PublicURL   string `json:"public_url,omitempty"`

	// Present is whether this seat has STARTED: something is written down
	// for it, whether or not it works.
	//
	// An explicit field because the roll-up asked `Requirements[0].Present`,
	// which is an index into a list only Slack fills and panicked the moment
	// a second app grew a roster. What it wanted to know was never about the
	// first requirement; it was this.
	Present bool `json:"present"`

	// Detail is the one line the roster shows under a seat's name: where its
	// credential is kept, or what is missing.
	//
	// Only an app with an inbound route per seat has a path to show, and
	// Slack is the only one. Every other app's roster said "no inbound path
	// yet" against every agent, which is true and says nothing about the
	// thing the row exists to report: whether this agent can act as itself
	// on this app.
	Detail string `json:"detail,omitempty"`
}

// list serves GET /setup/integrations.
func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	company := s.company()
	if company == nil {
		httpjson.FailWith(w, http.StatusConflict, codeNoActiveRevision, map[string]string{
			"hint": "no company configuration is active; import one before connecting an integration",
		})
		return
	}
	tools := make([]ToolState, 0, len(integration.Kinds))
	for _, kind := range integration.Kinds {
		state, ok := s.state(company, kind)
		if !ok {
			continue
		}
		tools = append(tools, state)
	}
	base := company.Integrations.WebhookBase()
	present, resolved := setup.Resolution(company.Integrations.PublicBaseURL, s.resolve)
	httpjson.Write(w, http.StatusOK, map[string]any{
		"tools": tools,
		// THE ADDRESS EVERY INBOUND VENDOR IS BUILT ON, answered once
		// rather than repeated in each tool: it is one setting, and a
		// screen that asked for it seven times would be asking the
		// operator to keep seven copies consistent.
		"public_base_url": map[string]any{
			"value": base, "present": present, "resolved": resolved,
			"config_path": "integrations.public_base_url",
		},
	})
}

// one serves GET /setup/integrations/{kind}.
func (s *Service) one(w http.ResponseWriter, r *http.Request) {
	kind := integration.Kind(r.PathValue("kind"))
	company := s.company()
	if company == nil {
		httpjson.FailWith(w, http.StatusConflict, codeNoActiveRevision, map[string]string{
			"hint": "no company configuration is active",
		})
		return
	}
	state, ok := s.state(company, kind)
	if !ok {
		httpjson.FailWith(w, http.StatusNotFound, codeUnknownKind, map[string]string{
			"detail": "this build serves no setup for " + string(kind),
			"hint":   "one of " + kindList(),
		})
		return
	}
	httpjson.Write(w, http.StatusOK, state)
}

// state builds one tool's answer, or reports that this build has no setup for
// it yet.
//
// AN EXPLICIT ABSENCE rather than an empty requirement list: a third-party app whose
// requirements nobody has written down would otherwise answer "nothing is
// missing" and the screen would show a Connect button that collects nothing.
func (s *Service) state(company *config.Company, kind integration.Kind) (ToolState, bool) {
	var reqs []setup.Requirement
	var seats []SeatState
	var configured, enabled bool
	var summary string
	switch kind {
	case integration.KindDatadog:
		block := company.Integrations.Datadog
		summary = datadog.Summary()
		reqs = datadog.Requirements(block, s.resolve)
		seats = credentialSeats(company, s.resolve,
			[]string{datadog.SeatEnv}, datadog.CredentialKeys, "Datadog", s.passes.Serves(kind))
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindGitHub:
		block := company.Integrations.GitHub
		summary = github.Summary()
		reqs = github.Requirements(block, s.resolve)
		seats = credentialSeats(company, s.resolve,
			[]string{github.SeatEnv}, github.CredentialKeys, "GitHub", false)
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindJira:
		block := company.Integrations.Jira
		summary = jira.Summary()
		reqs = jira.Requirements(block, company.Integrations.Atlassian.IsCloud(), s.resolve)
		// THE FORGE APP ID IS NOT ASKED FOR ANY MORE.
		//
		// It was the only way a Cloud site's events could reach this engine,
		// on the understanding that Atlassian serves the webhook API to
		// Connect and OAuth apps alone. It serves the DYNAMIC one that way;
		// webhook administration is /rest/webhooks/1.0/webhook on both
		// deployments and takes an API token, which is what this engine
		// registers through and what a live Cloud site was measured
		// delivering over.
		//
		// The route and the config field stay: a company already relaying
		// through Forge keeps working, and it is the fallback if Atlassian
		// ever retires the admin API. What goes is asking every operator to
		// install an app they do not need.
		seats = credentialSeats(company, s.resolve,
			jira.SeatEnvs, jira.CredentialKeys, "Jira", false)
		// THE ATLASSIAN BLOCKS HAVE NO `enabled` FIELD. Their presence IS
		// the switch, which is why a disconnect removes the block rather
		// than flipping a flag, and why enabled tracks configured here
		// rather than being invented.
		configured, enabled = block != nil, block != nil
	case integration.KindConfluence:
		block := company.Integrations.Confluence
		summary = confluence.Summary()
		reqs = confluence.Requirements(block, company.Integrations.Atlassian.IsCloud(), s.resolve)
		seats = credentialSeats(company, s.resolve,
			confluence.SeatEnvs, confluence.CredentialKeys, "Confluence", false)
		configured, enabled = block != nil, block != nil
	case integration.KindAtlassian:
		block := company.Integrations.Atlassian
		summary = atlassian.Summary()
		reqs = atlassian.Requirements(block, s.resolve)
		seats = credentialSeats(company, s.resolve,
			atlassian.SeatEnvs, atlassian.CredentialKeys, "Atlassian", s.passes.Serves(kind))
		configured, enabled = block != nil, block != nil
	case integration.KindGitLab:
		block := company.Integrations.GitLab
		summary = gitlab.Summary()
		reqs = gitlab.Requirements(block, s.resolve)
		seats = credentialSeats(company, s.resolve,
			[]string{gitlab.SeatEnv}, gitlab.CredentialKeys, "GitLab", s.passes.Serves(kind))
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindMattermost:
		block := company.Integrations.Mattermost
		summary = mattermost.Summary()
		reqs = mattermost.Requirements(block, s.resolve)
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindSlack:
		// THE ONLY PER-SEAT VENDOR. Its company block carries the working
		// indicator and nothing that authenticates; every credential is on
		// a seat, because every agent has its own Slack app.
		block := company.Integrations.Slack
		summary = slack.Summary()
		reqs = slack.CompanyRequirements(block)
		seats = slackSeats(company, s.resolve)
		// CONFIGURED WHEN ANY SEAT IS, not when the company block exists:
		// the block is optional settings, and a company with seven working
		// Slack apps and no block is fully configured.
		for _, seat := range seats {
			if len(seat.Requirements) > 0 && seat.Requirements[0].Present {
				configured, enabled = true, true
				break
			}
		}
	default:
		return ToolState{}, false
	}
	// A HANDLE FIELD IS A PICKER, so it needs the roster to pick from.
	//
	// It rendered as a select with one option, "Choose one": the seat a
	// Datadog alert falls back to could not be displayed when it was set and
	// could not be chosen when it was not, and the field is required. The
	// roster is the engine's own — deriving it in each app would be the same
	// list written six times.
	seatChoices(company, reqs)
	// WHAT A REFERENCE CURRENTLY READS AS, for the links a form draws out of
	// these values. Credentials are skipped inside. See [setup.FillEffective].
	setup.FillEffective(reqs, s.resolve)

	satisfied := len(setup.Outstanding(reqs)) == 0
	// A TOOL IS SATISFIED WHEN EVERY SEAT THAT HAS STARTED IS. A seat nobody
	// has set up does not make the tool unfinished, because a company
	// running Slack for three of its ten agents chose that.
	//
	// And only where the seats are load-bearing at all: see SeatsRequired.
	// An informational roster must not be able to report an app unfinished,
	// or listing a company's agents would turn every connected card into one
	// with work outstanding.
	seatsRequired := kind == integration.KindSlack
	if seatsRequired {
		for _, seat := range seats {
			if seat.Present && !seat.Satisfied {
				satisfied = false
				break
			}
		}
	}
	state := ToolState{
		Kind: kind, Configured: configured, Enabled: enabled, Summary: summary,
		Requirements:  reqs,
		Seats:         seats,
		Satisfied:     satisfied,
		InboundPath:   inboundPath(kind),
		CanProvision:  s.passes.Serves(kind),
		SeatsRequired: seatsRequired,
		NeedsOperator: s.passes.Needs(kind),
	}
	if base := company.Integrations.WebhookBase(); base != "" && state.InboundPath != "" {
		state.PublicURL = base + state.InboundPath
	}
	return state, true
}

// slackSeats is every agent seat's own Slack setup.
//
// EVERY AGENT, not only the ones already configured: the list is what a
// screen renders a form from, so leaving out the seats that have no app yet
// would leave an operator no way to give one to them. Human seats are
// excluded, because a person's Slack account is not something this engine
// provisions or holds a token for.
// credentialSeats is the roster of agents for an app whose seats each hold
// their own credential.
//
// ONE BUILDER over every app, fed by the app's OWN list of where it keeps a
// seat credential, because those spellings already exist in the package that
// authenticates with them: writing them again here would be a second list
// that stops matching the first, silently, since a seat whose credential was
// not found looks exactly like a seat that has none.
//
// The roster exists because a company's agents are the point of these
// integrations. A card that says Connected over no agents is telling an
// operator the half that cannot be acted on: the question is which of their
// people can work in this app, and only a per-seat answer has it.
func credentialSeats(company *config.Company, resolve func(string) (string, bool),
	envs, keys []string, app string, provisions bool,
) []SeatState {
	out := []SeatState{}
	for role := range company.EachRole() {
		// THROUGH THE SEAT, the same derivation slackSeats uses: a handle
		// defaults from the name, and "is this a person" is the org model's
		// question rather than the config's.
		seat := role.Seat()
		if !seat.IsAgent() {
			continue
		}
		state := SeatState{Handle: seat.Handle(), Name: role.Name, Requirements: []setup.Requirement{}}
		stored, where := seatCredential(role.MCPEnv, envs, keys)
		state.Present = stored != ""
		switch {
		case stored == "":
			// WHAT HAPPENS NEXT, not just what is absent, and the two apps
			// differ honestly: the reconcile loop creates the account where
			// this build has a pass for it, and where the app issues no
			// credential on a provisioner's behalf the next step is a
			// person's.
			if provisions {
				state.Detail = "not in " + app + " yet, created on the next sync"
				break
			}
			state.Detail = "no " + app + " credential yet, and " + app +
				" issues none on request: add one to this seat's mcp_env"
		default:
			// RESOLVED, not merely written down. A ${VAR} naming a secret
			// the store does not hold is the state that reads as configured
			// everywhere else while the agent authenticates with nothing.
			if _, ok := setup.Resolution(stored, resolve); ok != nil && !*ok {
				state.Detail = where + " did not resolve, so this agent authenticates with nothing"
				break
			}
			state.Satisfied = true
			state.Detail = where
		}
		out = append(out, state)
	}
	return out
}

// seatCredential finds a seat's credential for one app, and says where it is.
//
// The location is what the roster shows, and it is the mcp_env address rather
// than the value: a credential's value has no business on this wire, and the
// address is what an operator edits.
func seatCredential(env map[string]map[string]string, envs, keys []string) (stored, where string) {
	for _, name := range envs {
		block := env[name]
		if len(block) == 0 {
			continue
		}
		for _, key := range keys {
			if value := strings.TrimSpace(block[key]); value != "" {
				return value, "mcp_env." + name + "." + key
			}
		}
	}
	return "", ""
}

// discoverSite fills in the Atlassian address a submission left blank.
//
// ONLY WHEN THREE THINGS HOLD: the surface is Jira or Confluence, neither the
// submission nor the document names an address, and the company has an
// Atlassian organization key to ask with. Anything else is left exactly as it
// was — a company that typed its own site has made a decision, and this
// fills a blank rather than overriding one.
//
// It returns a sentence when the organization could not be read, because the
// alternative is writing a block that is refused for the reason this exists
// to prevent and reporting it as a validation error against a field the
// operator deliberately left empty.
func (s *Service) discoverSite(
	ctx context.Context, company *config.Company, kind integration.Kind,
	reqs []setup.Requirement, values map[string]string,
) string {
	if kind != integration.KindJira && kind != integration.KindConfluence {
		return ""
	}
	if named(values, reqs) {
		return ""
	}
	org := company.Integrations.Atlassian
	if org == nil {
		return ""
	}
	// THE NAME, NOT THE REFERENCE. resolve takes the variable's name — a
	// document holds `${ATLASSIAN_ORG_API_KEY}` and the store is keyed on
	// what is inside the braces — so passing the whole reference resolves
	// nothing, silently, and this read as a company with no key at all.
	// BOTH THROUGH THE RESOLVER. The key was already read this way; the
	// organization id was not, so a company keeping it in the sealed store
	// asked Atlassian about an organization literally named
	// "${ATLASSIAN_ORG_ID}".
	key := setup.Deref(org.APIKey, s.resolve)
	orgID := setup.Deref(org.OrgID, s.resolve)
	if orgID == "" || key == "" {
		return ""
	}
	site, err := atlassian.NewClient(atlassian.ClientOptions{}).
		DiscoverSite(ctx, key, orgID)
	if err != nil {
		return "the Atlassian organization could not be read, so the site this " +
			"integration works in is not known and none was given: " + err.Error()
	}
	values["cloud_id"] = site.CloudID
	values["site_url"] = site.HostURL
	if kind == integration.KindConfluence {
		values["site_url"] = site.HostURL + "/wiki"
	}
	return ""
}

// named reports whether an address is already known, from the submission or
// from what the company already holds.
func named(values map[string]string, reqs []setup.Requirement) bool {
	for _, field := range []string{"url", "cloud_id"} {
		if strings.TrimSpace(values[field]) != "" {
			return true
		}
		for _, r := range reqs {
			if r.Field == field && strings.TrimSpace(r.Stored) != "" {
				return true
			}
		}
	}
	return false
}

// seatChoices fills every handle requirement with the company's agent seats.// seatChoices fills every handle requirement with the company's agent seats.
//
// In place, on the app's own list, because a requirement is what the form
// renders and the choices belong to the field rather than beside it. Human
// seats are left out for the reason the provisioners leave them out: an alert
// routed to a person is a person's own notification, not an agent's work.
//
// An app that already named its own choices keeps them; nothing does today,
// and one that does knows something this does not.
func seatChoices(company *config.Company, reqs []setup.Requirement) {
	var choices []setup.Choice
	for role := range company.EachRole() {
		// THROUGH THE SEAT, the same derivation slackSeats uses: a handle
		// defaults from the name, and "is this a person" is the org model's
		// question. A second implementation here would eventually disagree.
		seat := role.Seat()
		if !seat.IsAgent() {
			continue
		}
		label := seat.Handle()
		if name := strings.TrimSpace(role.Name); name != "" && name != label {
			label = name + " (" + seat.Handle() + ")"
		}
		choices = append(choices, setup.Choice{Value: seat.Handle(), Label: label})
	}
	if len(choices) == 0 {
		return
	}
	// APPENDED, not filled only when empty. A handle field may declare a
	// choice of its own that is not a seat — Datadog's fallback offers
	// "None", which is an answer rather than a person — and the roster
	// belongs after it rather than instead of it.
	for i := range reqs {
		if reqs[i].Kind == setup.KindHandle {
			reqs[i].Choices = append(slices.Clone(reqs[i].Choices), choices...)
		}
	}
}

func slackSeats(company *config.Company, resolve func(string) (string, bool)) []SeatState {
	base := company.Integrations.WebhookBase()
	out := []SeatState{}
	for role := range company.EachRole() {
		// THROUGH THE SEAT, which is where the derivation lives: a handle
		// defaults from the name, and "is this a person" is the org
		// model's question rather than the config's. Deriving either here
		// would be a second implementation of an identity rule.
		seat := role.Seat()
		if !seat.IsAgent() {
			continue
		}
		handle := seat.Handle()
		reqs := slack.Requirements(handle, role, resolve)
		setup.FillEffective(reqs, resolve)
		state := SeatState{
			Handle: handle, Name: role.Name,
			Requirements: reqs,
			Present:      len(reqs) > 0 && reqs[0].Present,
			Satisfied:    len(setup.Outstanding(reqs)) == 0,
			InboundPath:  "/webhooks/slack/" + handle,
		}
		if base != "" {
			state.PublicURL = base + state.InboundPath
		}
		out = append(out, state)
	}
	return out
}

// inboundPath is where a third-party app's deliveries arrive.
//
// Only the surfaces this build serves setup for. It is the same path the
// integrations answer reports, and the two are checked against each other by
// a test rather than by a reader's memory.
func inboundPath(kind integration.Kind) string {
	switch kind {
	case integration.KindDatadog:
		return "/webhooks/datadog"
	case integration.KindGitHub:
		return "/webhooks/github"
	case integration.KindJira:
		return "/webhooks/jira"
	case integration.KindGitLab:
		return "/webhooks/gitlab"
	case integration.KindConfluence:
		// The Data Center route. Cloud registers one hook per event under
		// this prefix, which is why the URL an operator copies is not a
		// single path and the setup surface shows the base instead.
		return "/webhooks/confluence"
	default:
		return ""
	}
}

func kindList() string {
	names := make([]string, 0, len(integration.Kinds))
	for _, k := range integration.Kinds {
		names = append(names, string(k))
	}
	return strings.Join(names, ", ")
}

// inputs serves POST /setup/integrations/{kind}/inputs.
func (s *Service) inputs(w http.ResponseWriter, r *http.Request) {
	kind := integration.Kind(r.PathValue("kind"))
	company := s.company()
	if company == nil {
		httpjson.FailWith(w, http.StatusConflict, codeNoActiveRevision, map[string]string{
			"hint": "no company configuration is active",
		})
		return
	}
	state, ok := s.state(company, kind)
	if !ok {
		httpjson.FailWith(w, http.StatusNotFound, codeUnknownKind, map[string]string{
			"hint": "one of " + kindList(),
		})
		return
	}

	body, err := httpjson.ReadBody(w, r, MaxBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	var req struct {
		IfMatch string            `json:"if_match"`
		Summary string            `json:"summary"`
		Seat    string            `json:"seat"`
		Values  map[string]string `json:"values"`
		// Generate names the MINTABLE fields the caller wants the engine
		// to produce. Separate from Values so a client cannot ask for a
		// mint and supply a value in the same breath, and so an empty
		// string in Values is never mistaken for one.
		Generate []string `json:"generate"`
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := decode(body, &req); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": err.Error(),
		})
		return
	}

	// A SUBMISSION IS FOR A SEAT OR FOR THE COMPANY, and the requirement
	// list it is checked against differs. Slack's credentials live on the
	// seat, so a submission naming one is measured against that seat's
	// own list rather than the company block's.
	against := state.Requirements
	if req.Seat != "" {
		found := false
		for _, seat := range state.Seats {
			if seat.Handle == req.Seat {
				against, found = seat.Requirements, true
				break
			}
		}
		if !found {
			httpjson.FailWith(w, http.StatusNotFound, codeInvalidInput, map[string]string{
				"detail": "no seat called " + req.Seat + " takes per-seat setup for " + string(kind),
			})
			return
		}
	}

	values := map[string]string{}
	for field, value := range req.Values {
		values[field] = value
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := mintInto(values, against, req.Generate); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, codeInvalidInput, map[string]string{
			"detail": err.Error(),
		})
		return
	}
	if len(values) == 0 {
		httpjson.FailWith(w, http.StatusBadRequest, codeInvalidInput, map[string]string{
			"detail": "the submission carried no values",
		})
		return
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := refuseEmpty(values, against); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, codeInvalidInput, map[string]string{
			"detail": err.Error(),
		})
		return
	}

	// THE SITE, WHERE THE ORGANIZATION KNOWS IT AND THE SUBMISSION DOES NOT.
	//
	// Jira and Confluence are refused by the config with neither url nor
	// cloud_id, and the pass that discovers the site can only fill a block
	// that already exists — so a connect leaving the site blank would be
	// rejected before anything could discover anything. Asking Atlassian
	// here closes that loop: the address arrives in the SAME write as the
	// credentials, which is what makes the block valid the moment it exists.
	if note := s.discoverSite(r.Context(), company, kind, against, values); note != "" {
		httpjson.FailWith(w, http.StatusBadGateway, codeInvalidInput, map[string]string{
			"detail": note,
		})
		return
	}

	summary := auditSummary(kind, req.Summary)
	result, err := s.writer.Write(r.Context(), against, setup.Submission{
		Kind: kind, Values: values, Seat: req.Seat,
		Summary: summary, Operator: operatorOf(r), Expect: req.IfMatch,
	})
	if err != nil {
		s.refuse(w, r, err, result)
		return
	}

	// NAMES, NEVER VALUES, in the log and in the answer alike. The names
	// are a fact an operator needs; a submitted value must not reach a log
	// line, an error detail or a response body.
	log.InfoContext(r.Context(), "setup_inputs_written",
		"kind", kind, "revision", result.RevisionID, "epoch", result.Epoch,
		"secrets", strings.Join(result.Secrets, ","), "reloaded", result.Reloaded,
		"operator", operatorOf(r))

	after := s.company()
	fresh := state
	if after != nil {
		if refreshed, ok := s.state(after, kind); ok {
			fresh = refreshed
		}
	}
	httpjson.Write(w, http.StatusCreated, map[string]any{
		"revision_id": result.RevisionID, "epoch": result.Epoch,
		"wrote_secrets": result.Secrets, "reloaded": result.Reloaded,
		"state": fresh,
	})
}

// disconnect serves DELETE /setup/integrations/{kind}.
//
// It removes the BLOCK from the company document and nothing else. The sealed
// values stay: they are named by nothing now, which is inert, and deleting a
// credential an operator may be sharing with another deployment is not a
// decision a disconnect button gets to make on its own. `crewlet secrets
// unset` is the deliberate path, and the setup answer names what is orphaned.
func (s *Service) disconnect(w http.ResponseWriter, r *http.Request) {
	kind := integration.Kind(r.PathValue("kind"))
	company := s.company()
	if company == nil {
		httpjson.FailWith(w, http.StatusConflict, codeNoActiveRevision, map[string]string{
			"hint": "no company configuration is active",
		})
		return
	}
	state, ok := s.state(company, kind)
	if !ok {
		httpjson.FailWith(w, http.StatusNotFound, codeUnknownKind, map[string]string{
			"hint": "one of " + kindList(),
		})
		return
	}
	if !state.Configured {
		// Already absent. Answering 200 rather than 404 because the
		// caller's goal is a company without this integration, and it
		// has one.
		httpjson.Write(w, http.StatusOK, map[string]any{"key": kind, "removed": false})
		return
	}
	var req disconnectRequest
	if body, err := httpjson.ReadBody(w, r, MaxBody); err != nil {
		httpjson.Refuse(w, err)
		return
	} else if len(body) > 0 {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := decode(body, &req); err != nil {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
				map[string]string{"detail": err.Error()})
			return
		}
	}

	// ASKED FOR, NOT DONE HERE. The block stays in the document until the
	// third-party app teardown has run, because that block carries the credential
	// the teardown authenticates with: removing it now would strand every
	// webhook and account the integration still holds, with nothing left
	// to authenticate a second attempt.
	//
	// FORCE is the exception, and it is the operator saying they will
	// clean up at the third-party app themselves. One that will never accept
	// the delete (a revoked token, an instance that is gone) would
	// otherwise hold the integration in Disconnecting for ever.
	if !req.Force {
		if s.status == nil {
			httpjson.FailWith(w, http.StatusServiceUnavailable, codeNoStatusStore,
				map[string]string{
					"detail": "this node has no fleet status store, so a disconnect " +
						"cannot be recorded for the loop to act on",
					"hint": "retry against a node with coordination, or force the " +
						"disconnect and remove what the third-party app holds by hand",
				})
			return
		}
		if err := s.markDisconnecting(r.Context(), kind, req.RemoveSeats); err != nil {
			httpjson.FailWith(w, http.StatusServiceUnavailable, httpjson.CodeInternalError,
				map[string]string{"detail": err.Error()})
			return
		}
		log.InfoContext(r.Context(), "setup_disconnect_requested",
			"integration", kind, "remove_seats", req.RemoveSeats,
			"operator", operatorOf(r))
		httpjson.Write(w, http.StatusAccepted, map[string]any{
			"key": kind, "removed": false, "disconnecting": true,
			"remove_seats": req.RemoveSeats,
			"detail": "the engine is removing what this integration holds at the " +
				"third-party app; the block is dropped when that finishes",
		})
		return
	}

	patch := []byte(`{"integrations":{"` + string(kind) + `":null}}`)
	applied, err := s.config.Apply(r.Context(), configapi.ApplyRequest{
		Patch:    patch,
		Summary:  "disconnect " + string(kind),
		Operator: operatorOf(r),
		Expect:   strings.TrimSpace(r.Header.Get("If-Match")),
	})
	if err != nil {
		s.refuse(w, r, err, setup.Result{})
		return
	}
	// FORCED, so whatever the third-party app still holds is now the operator's to
	// remove. The status row goes with the block: leaving one would report
	// a surface that is no longer configured.
	if s.status != nil {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := s.status.ForgetIntegration(r.Context(), kind); err != nil {
			log.WarnContext(r.Context(), "setup_status_not_forgotten",
				"integration", kind, "error", err)
		}
	}
	// WHAT IS ACTUALLY ORPHANED, which is only what was actually stored.
	// This walked every secret REQUIREMENT, so an integration with
	// optional credentials named ones nobody had ever set and told an
	// operator to unset something that does not exist. A list to act on
	// has to be a list of things that are there.
	orphaned := []string{}
	for _, req := range state.Requirements {
		if req.Kind != setup.KindSecret || !req.Present {
			continue
		}
		if name, _, err := setup.PointerFor(kind, req, req.Stored); err == nil {
			orphaned = append(orphaned, name)
		}
	}
	log.InfoContext(r.Context(), "setup_disconnected",
		"kind", kind, "revision", applied.RevisionID, "operator", operatorOf(r))
	httpjson.Write(w, http.StatusOK, map[string]any{
		"key": kind, "removed": true,
		"revision_id": applied.RevisionID, "epoch": applied.Epoch,
		// NAMED, not deleted. An operator who wants them gone runs
		// `crewlet secrets unset`, having read this list.
		"orphaned_secrets": orphaned,
	})
}

// refuse maps a write failure onto this surface's answers.
func (s *Service) refuse(w http.ResponseWriter, r *http.Request, err error, partial setup.Result) {
	var literal *setup.ErrLiteralInConfig
	var stale *setup.ErrStaleBase
	var raced *configapi.RacedError
	var invalid *configapi.ValidationError
	var patchErr *configapi.PatchError
	switch {
	case errors.As(err, &literal):
		httpjson.FailWith(w, http.StatusConflict, codeLiteralInConfig, map[string]string{
			"path": literal.Path,
			"detail": "this field holds a value rather than a ${VAR} reference, so " +
				"there is no variable to write the credential into",
			"hint": "replace it with a whole ${VAR} reference, or clear it and submit again",
		})
	case errors.As(err, &stale):
		// REFUSED BEFORE ANYTHING WAS WRITTEN, which is the difference
		// from the config surface's own raced answer: there is no stored
		// revision to name, because nothing was stored.
		httpjson.FailWith(w, http.StatusConflict, codeRevisionAdvanced, map[string]string{
			"your_base": stale.Base, "current_revision_id": stale.Current,
			"hint": "the configuration changed since you read it; re-read the " +
				"setup state and submit again",
		})
	case errors.As(err, &raced):
		extra := map[string]string{
			"your_base": raced.Base,
			"hint": "another write activated first; re-read the setup state and " +
				"submit again",
		}
		if raced.Current != "" {
			extra["current_revision_id"] = raced.Current
		}
		if raced.Stored != "" {
			extra["stored_revision_id"] = raced.Stored
		}
		httpjson.FailWith(w, http.StatusConflict, codeRevisionAdvanced, extra)
	case errors.As(err, &invalid):
		httpjson.FailWith(w, http.StatusBadRequest, codeValidationError, map[string]string{
			"detail": invalid.Err.Error(),
			"hint": "the values are checked as the whole company document they " +
				"produce, so a field that is fine on its own is still refused " +
				"when it leaves the company invalid",
		})
	case errors.As(err, &patchErr):
		httpjson.FailWith(w, http.StatusBadRequest, codeValidationError, map[string]string{
			"detail": patchErr.Err.Error(),
		})
	case errors.Is(err, configapi.ErrNoControlPlane):
		httpjson.FailWith(w, http.StatusServiceUnavailable, codeNoControlPlane, map[string]string{
			"hint": "this process has no coordination store, so it cannot activate a revision",
		})
	case errors.Is(err, configapi.ErrNoActiveRevision):
		httpjson.FailWith(w, http.StatusConflict, codeNoActiveRevision, map[string]string{
			"hint": "import a company configuration first",
		})
	case s.secrets == nil || errors.Is(err, secrets.ErrNoKeyring):
		// TWO WAYS TO HAVE NO KEYRING, and only the first was caught. A
		// node with no secret store WIRED is `s.secrets == nil`; a node
		// with one whose bootstrap names no key fails at the seal, with
		// this sentinel, which exists to be recognised. It fell through
		// to the generic case, so a screen that could have said "set
		// secrets.keys" said internal_error and left the operator
		// reading engine logs to find a one-line fix.
		httpjson.FailWith(w, http.StatusServiceUnavailable, codeNoKeyring, map[string]string{
			"detail": "this node has no secrets.keys, so a credential cannot be sealed",
			"hint": "run `crewlet secrets keygen`, put the key in secrets.keys in " +
				"crewlet.yaml, and restart the engine",
		})
	default:
		// THE DETAIL GOES TO THE LOG, never to the caller: a third-party app's own
		// refusal can quote the config value it was given, which on a
		// company holding a literal would echo the credential.
		log.ErrorContext(r.Context(), "setup_write_failed",
			"error", err.Error(), "secrets", strings.Join(partial.Secrets, ","))
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
	}
}

// operatorOf is who the guard authenticated, or empty.
func operatorOf(r *http.Request) string {
	operator, _ := auth.OperatorFrom(r.Context())
	return operator
}

// disconnectRequest is what the Disconnect dialog sends.
type disconnectRequest struct {
	// RemoveSeats is the checkbox: also remove the accounts this engine
	// created at the third-party app. False leaves them and removes only what the
	// engine registered for itself.
	RemoveSeats bool `json:"remove_seats"`

	// Force drops the block without waiting for the third-party app teardown.
	//
	// The way out of a teardown that can never succeed: a revoked
	// credential, an instance that no longer exists. It is the operator
	// saying they will remove what the third-party app holds themselves, so the
	// answer names what was left behind.
	Force bool `json:"force"`
}

// markDisconnecting records the intent on the fleet row, so the loop picks it
// up and the screen stops showing a connected integration.
//
// The row is written even when there is none: a surface nobody has reconciled
// yet still has to carry the intent, or a disconnect asked for before the
// first pass would be lost.
func (s *Service) markDisconnecting(
	ctx context.Context, kind integration.Kind, removeSeats bool,
) error {
	var state integration.State
	if states, err := s.status.LoadIntegrations(ctx); err == nil {
		for _, row := range states {
			if row.Kind == kind {
				state = row
				break
			}
		}
	}
	state.Kind = kind
	state.Disconnecting = true
	state.RemoveSeats = removeSeats
	// DUE NOW. The zero value is already in the past, but a row that has
	// been reconciled carries a future one, and inheriting it would leave
	// the disconnect waiting out a backoff nobody asked it to serve.
	state.NextAttemptAt = time.Time{}
	state.Attempts = 0
	return s.status.SaveIntegration(ctx, state)
}
