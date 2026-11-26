// Package setupapi serves /setup: connecting an integration from the
// dashboard rather than from a shell.
//
// # Why it is not part of /integrations
//
// `GET /integrations` is a question — what each surface's last reconcile
// reported — answered through the query registry like every other read. What
// this surface answers is a different class of thing: the NAMES of the
// credentials a company holds, which are unset, the third-party app pages an
// administrator would visit, and the fields a caller can write — a map of what
// to attack — beside the writes that change them. So it is its own prefix,
// mounted through the authority table route by route, and every route is
// admin's, reads included.
//
// # Connecting is a company write AND a credential write
//
// Each route is mounted through [authz.Router]: the listings and a pass's
// record are `setup.read`, and every route that changes the company is
// `setup.connect`, which asks a recent proof — both admin's (ADR-0033). A route
// that SEALS a credential asks `secrets.write` on top — a submission carrying a secret field, a
// provisioning pass (which is handed a sink that mints and seals), and a
// GitHub App (whose private key is sealed on the return) — because this
// surface performs no write of its own: the credential goes through the store
// /secrets serves, and a caller who could not write it there must not be
// able to write it here. The pattern cannot see whether a submission carries
// one, so that half is asked once the body is read, through the same guard.
//
// TODAY THE TWO ASK THE SAME PERMISSION, and the second is kept anyway: it is
// the surface stating that a sealed credential is a `/secrets` write, decided
// by that verb's own row (a step-up that row asks for cannot be skipped by
// choosing this door), so the day the table decides the two verbs apart
// this surface follows the table rather than a second copy of it.
//
// THE ROUTES USED TO DECIDE NOTHING, which was sound while the only credential
// was an operator token and stopped being sound the day a person could sign
// in holding a read-only permission alone.
//
// # A managed document is refused before every side effect
//
// Where Tier A names the company document's writers (ADR-0030), a change to
// it through any other credential is refused `config_managed` — by
// internal/api/configapi's own check, asked here EARLIER than the write that
// would ask it, because this surface acts before it writes: a connect seals a
// credential, a disconnect queues a teardown at the vendor, and a GitHub App
// is created once with a key GitHub never reissues. So it is asked after the
// route's grants and before each of those, and again at the GitHub callback,
// of the party the sealed state names, before the one-time code is spent. A
// rotation of a value the document already names changes no byte of it and
// stays open.
//
// # It describes the fleet's company, never what this node runs
//
// Every route reads the company the fleet's activation pointer names, with the
// revision it is, from one read (configapi's FleetCompany): what each field
// holds decides a write — a fresh `${VAR}`, a write through the one there, a
// refusal over a literal — and the write lands on the fleet's revision. Read
// from the company this node's engine runs, an answer on a node that refused
// the fleet's revision, or one mid-apply, described one document and named
// another as its `if_match`, and a write decided on the first patched the
// second. A submission with no `if_match` is held to the revision its own read
// returned, for the same reason — the patch and a rotation's reload alike, each
// to its compare-and-set, since a reload of a revision whose slot no longer
// names the rotated `${VAR}` delivers nothing. What IS this node's is said as
// such: whether a `${VAR}` resolves here, and what its own passes last
// reported.
//
// # A refusal after a seal names what was sealed
//
// Every refusal after a credential is sealed, the unknown outcome and a
// rotation's failed reload included, carries `wrote_secrets`
// (`setup.Result` holds them on every return after the first seal): a client
// told only "failed" would write the credential again blind. A seal the store
// did not answer is its own answer (`setup.SealError`): the retryable 503
// naming the credential whose write may have landed as the field
// `unanswered_secret`, with no `outcome`, because the document was not changed
// and a resubmission seals every credential again. It is a field, not only the
// `reason`, because a client branches on fields and read a 503 without
// `outcome` as one that wrote nothing. A rotation's reload is held to the
// revision the patch landed on, to its compare-and-set: reloading whatever the
// pointer named by then answers 201 for a rotation the running document never
// reads. A 409 from a write that lost at its activation names the revision it
// kept and its `node` (`configapi.KeptRevision`); a forced disconnect's
// `If-Match` is its base, a stale one `412 revision_advanced`
// (`configapi.ApplyRequest.Stated`), while a connect's body `if_match` is
// `409`. A node's own condition is answered through `operator.AnswerCondition`.
//
// # The GitHub App return carries no credential of ours
//
// The state the begin route mints is sealed under the fleet keyring, not
// signed, because it travels through GitHub, a browser history and every
// ingress log, and it names who began the creation: the key, the webhook secret
// and the seat's revision are written as that person, not as `setup`, which is
// nobody. A seat write whose activation went unanswered is read back once
// (`AppFlow.unconfirmed`) and answered as finished where the seat records the
// app, otherwise told, under a heading of its own, that whether the app was
// added is not known yet, with a refresh of the spent link as the answer
// (`AppFailure`, `AppFlow.usedLink`). Told as "couldn't add the app, delete it
// on GitHub", it sent a person to delete the app the agent was running.
//
// # Every access sync ends in the check
//
// A save, an action at the vendor and a keep each start a sync that answers
// only for its seats, so each carries the check route's own read-only pass
// (`AccessSync.Then`, `Service.recheck`), run through the held pass
// (`runHeld`) and recorded through the loop's fold while the integration still
// reads as syncing. Without it the integration's issues, notes and card stay as
// the last pass left them until somebody presses Check now. One agent's lane
// (`accessview.go`) is the Agents tab's answer narrowed and worded by
// `seataccess.ViewOf`, for a reader showing one agent's access, Ask Crewlet's
// `get_seat_access` among them.
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
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/admob"
	"github.com/crewlet/crewlet/internal/adsense"
	"github.com/crewlet/crewlet/internal/appleads"
	"github.com/crewlet/crewlet/internal/appstoreconnect"
	"github.com/crewlet/crewlet/internal/metaads"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/authz"
	awsint "github.com/crewlet/crewlet/internal/aws"
	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/digitalocean"
	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/googleads"
	"github.com/crewlet/crewlet/internal/googleanalytics"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/googleplay"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/scaleway"
	"github.com/crewlet/crewlet/internal/searchconsole"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/slack"
	sshint "github.com/crewlet/crewlet/internal/ssh"
	"github.com/crewlet/crewlet/internal/words"
)

var log = logging.Get("api.setup")

// MaxBody bounds one submission. The largest thing one carries is a vendor API
// token, under a kilobyte, so a caller that hits the cap has sent the wrong
// thing.
const MaxBody = 64 << 10

// The refusals this surface answers with are all on internal/api/httpjson's
// table, none declared here.

// RetryBusy is the Retry-After on [httpjson.CodeSurfaceBusy]: the span the
// request already waited ([disconnectRetryFor]), not a second spelling of it.
// What holds a surface is a reconcile tick or an operator's pass, so one that
// outlived a wait is likely a moment from ending, and another wait is the
// soonest a retry could find it released without becoming load.
//
// The dashboard's disconnect dialog waits exactly this, read off the header.
// Exported for internal/api's gate over every fixed hint the dashboard waits
// out.
const RetryBusy = disconnectRetryFor

// Options wire the service.
//
// Every field but Now is required and [New] refuses a missing one by name: a
// nil is a wiring mistake, and a surface that quietly shrank around one would
// present it as a deliberate answer.
type Options struct {
	// Config is the write path PATCH /config drives, and the read of the
	// company every route describes ([Service.readCompany]).
	Config *configapi.Service

	// Secrets seals a submitted credential under the fleet keyring.
	Secrets setup.Secrets

	// Resolve turns a ${VAR} name into what this process resolved, through the
	// engine's own chain.
	Resolve func(string) (string, bool)

	// CloudIDs reads the cloud id of a Cloud site a Jira or Confluence block
	// names by url: the engine's one reader, so a write's answer and the
	// integrations answer read the same result.
	CloudIDs interface {
		Resolve(ctx context.Context, site string) (string, error)
	}

	// WebhookBase is where a third-party app reaches this deployment, without
	// its trailing slash: [config.API.WebhookBase] (`api.public.external_url`
	// where configured, else `api.external_url`).
	//
	// The vendor's address, never the person's: every address built on it is
	// one a vendor follows under `/webhooks/`, which the person's listener
	// answers 404 once there are two.
	//
	// A plain string, because it is Tier A. Empty is a deployment with no such
	// address, which every gesture that would hand one to a vendor refuses by
	// name.
	WebhookBase string

	// WebhookBaseKey is the Tier A key WebhookBase is read from
	// ([config.API.WebhookBaseKey]), named by the listing and every refusal so
	// an operator edits the setting in force. Required: a deployment with no
	// address still has the key that would hold one.
	WebhookBaseKey string

	// Passes are the third-party apps this build can provision over the API.
	Passes *setup.Runner

	// Probes check a typed credential at its vendor before anything stores it,
	// one per integration kind: what answers
	// POST /setup/integrations/{kind}/verify. [New] refuses a set missing any
	// kind in [integration.Kinds], since a missing probe is a Test button that
	// fails on that form.
	Probes map[integration.Kind]setup.Probe

	// Drawers redraw a live view from what a form holds, for the kinds whose
	// requirements carry one: what answers POST
	// /setup/integrations/{kind}/draw. Nil draws none.
	Drawers map[integration.Kind]setup.Drawer

	// Sink builds the recorder a pass writes minted credentials through, under
	// the party the pass runs for.
	Sink func(by iam.Actor) (provision.TokenSink, error)

	// Status is where a pass records what it found: the same fleet row the
	// reconcile loop writes, so the two cannot disagree.
	Status Status

	// SlackApps is which Slack app each agent seat authenticates as, by handle.
	// A live fact from the engine and the only source: the company document
	// names no app, since the app issues the token. It returns nil where no
	// Slack transport runs, so the roster says only where the credential
	// lives, which is what it can prove.
	SlackApps func() map[string]string

	// Consents starts, reads and gives up an integration's consent grant
	// (consent.go). Nil answers those routes 501.
	Consents ConsentSignIns

	// StateCipher seals the GitHub App state and MUST be the fleet's own
	// keyring, the same on every node: where the begin and the callback land on
	// different nodes, any other refuses every completion. Required; config
	// refuses to serve the API without `secrets.keys`. A per-process fallback
	// once let a node begin a creation and then fail to seal the app's key,
	// which GitHub issues once.
	StateCipher secrets.Cipher

	// StateClaims records a spent callback state so none is accepted twice. The
	// same fleet requirement as StateCipher: a node that cannot see the first
	// record would accept a replay. The fleet's claim registry is open on every
	// node.
	StateClaims StateClaims

	// SSHTests is the fleet's record of each SSH server's latest Test
	// connection, shown on its row.
	SSHTests SSHTests

	// SSH is the engine's half of Test connection and the SSH audit.
	SSH SSHAccess

	// Access is the engine's per-seat access: each seat's setting, account and
	// last sync on every integration managing one account per agent, the plan
	// a draft would take, and the sync a save starts ([Access]).
	Access Access

	// Now is injectable so a test can pin a secret row's timestamp.
	Now func() time.Time
}

// Service serves /setup.
type Service struct {
	config   *configapi.Service
	writer   setup.Writer
	resolve  func(string) (string, bool)
	cloudIDs interface {
		Resolve(ctx context.Context, site string) (string, error)
	}
	// webhookBase and webhookBaseKey are the vendor's address and the Tier
	// A key it is read from. See [Options.WebhookBase].
	webhookBase, webhookBaseKey string
	// slackApps is which Slack app each seat authenticates as, from the
	// engine. See [Options.SlackApps].
	slackApps func() map[string]string
	consents  ConsentSignIns
	secrets   setup.Secrets
	passes    *setup.Runner
	// access is the engine's per-seat access. See [Options.Access].
	access  Access
	probes  map[integration.Kind]setup.Probe
	drawers map[integration.Kind]setup.Drawer
	sink    sinkFactory
	status  Status
	// sshTests is [Options.SSHTests].
	sshTests SSHTests
	// ssh is [Options.SSH].
	ssh     SSHAccess
	clock   func() time.Time
	appFlow *AppFlow

	// guard decides every route and the credential write one adds. No chart: no
	// verb here asks a relation.
	guard authz.Guard
}

// New builds the service and the GitHub App flow its begin route and the
// webhook callback share.
func New(opts Options) (*Service, error) {
	var missing []string
	for _, field := range []struct {
		name   string
		absent bool
	}{
		{"Config", opts.Config == nil},
		{"Secrets", opts.Secrets == nil},
		{"Resolve", opts.Resolve == nil},
		{"CloudIDs", opts.CloudIDs == nil},
		{"Passes", opts.Passes == nil},
		{"Probes", opts.Probes == nil},
		{"Sink", opts.Sink == nil},
		{"Status", opts.Status == nil},
		{"SlackApps", opts.SlackApps == nil},
		{"StateCipher", opts.StateCipher == nil},
		{"StateClaims", opts.StateClaims == nil},
		{"Access", opts.Access == nil},
		{"SSHTests", opts.SSHTests == nil},
		{"SSH", opts.SSH == nil},
		{"WebhookBaseKey", opts.WebhookBaseKey == ""},
	} {
		if field.absent {
			missing = append(missing, "Options."+field.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("setupapi: %s required: the setup surface is "+
			"built beside the engine, which supplies every one of them",
			strings.Join(missing, ", "))
	}
	if gaps := probesComplete(opts.Probes); len(gaps) > 0 {
		return nil, fmt.Errorf("setupapi: Options.Probes has no probe for %s: "+
			"every integration this build sets up takes a credential a person "+
			"types, and its Test button needs one", strings.Join(gaps, ", "))
	}
	// Defaulted here, not at each use: an unguarded call in the GitHub App
	// callback would panic after GitHub created the app and before its key
	// (issued once) was sealed.
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	s := &Service{
		config:         opts.Config,
		resolve:        opts.Resolve,
		cloudIDs:       opts.CloudIDs,
		webhookBase:    opts.WebhookBase,
		webhookBaseKey: opts.WebhookBaseKey,
		secrets:        opts.Secrets,
		passes:         opts.Passes,
		access:         opts.Access,
		sshTests:       opts.SSHTests,
		ssh:            opts.SSH,
		probes:         maps.Clone(opts.Probes),
		drawers:        maps.Clone(opts.Drawers),
		sink:           opts.Sink,
		status:         opts.Status,
		slackApps:      opts.SlackApps,
		consents:       opts.Consents,
		clock:          now,
		guard:          authz.ContextGuard(authz.NoChart{}),
		writer: setup.Writer{
			Secrets: opts.Secrets,
			Config:  configWriter{svc: opts.Config},
			Now:     now,
		},
	}
	// Built here, not attached afterwards: no window in which the begin route
	// has a service with no flow to validate the state it would mint.
	s.appFlow = newAppFlow(s, opts.StateCipher, opts.StateClaims)
	return s, nil
}

// Routes registers the surface.
func (s *Service) Routes(mux authz.Mux) error {
	router := authz.NewRouter(mux, s.guard)
	var failures []error
	mount := func(pattern string, a authz.Action, h http.HandlerFunc) {
		if err := router.Handle(pattern, authz.Policy{Action: a}, h); err != nil {
			failures = append(failures, err)
		}
	}
	// Every POST is the connect verb and says what it does, which its method
	// does not ([authz.Effect]).
	post := func(pattern string, does authz.Effect, h http.HandlerFunc) {
		p := authz.Policy{Action: authz.ActionSetupConnect, Effect: does}
		if err := router.Handle(pattern, p, h); err != nil {
			failures = append(failures, err)
		}
	}
	mount("GET /setup/integrations", authz.ActionSetupRead, s.list)
	mount("GET /setup/integrations/{kind}", authz.ActionSetupRead, s.one)
	post("POST /setup/integrations/{kind}/inputs", authz.EffectRewrites, s.inputs)
	mount("DELETE /setup/integrations/{kind}", authz.ActionSetupConnect, s.disconnect)
	// A typed credential checked at its vendor, nothing stored: the connect
	// verb, since it spends a credential on a person's hand (verify.go).
	post("POST /setup/integrations/{kind}/verify", authz.EffectReads, s.verify)
	// The two that run something at the app (pass.go: why check and provision
	// are one function). Both are the connect verb: a check writes nothing at
	// the app but is still a pass run on the company's credentials at a
	// person's hand.
	post("POST /setup/integrations/{kind}/provision", authz.EffectRewrites, s.provision)
	post("POST /setup/integrations/{kind}/check", authz.EffectReads, s.check)
	// An integration's consent grant (consent.go): a sign-in makes a
	// credential the engine seals, so a start and a sign-out add
	// `secrets.write` to the connect verb in the handler.
	post("POST /setup/integrations/{kind}/consent", authz.EffectCreates, s.startConsent)
	mount("DELETE /setup/integrations/{kind}/consent", authz.ActionSetupConnect, s.signOutConsent)
	// OUTSIDE /setup/integrations/{kind}/: the id is a sign-in's.
	mount("GET /setup/consent-sign-ins/{id}", authz.ActionSetupRead, s.consentSignIn)
	// One agent's own app: a GitHub App is one bot identity, so an app per
	// agent is the only way each acts as itself.
	post("POST /setup/integrations/github/app", authz.EffectCreates, s.beginApp)
	mount("GET /setup/integrations/{kind}/runs/{id}", authz.ActionSetupRead, s.runByID)
	// The listing: nothing else could name a run id.
	mount("GET /setup/integrations/{kind}/runs", authz.ActionSetupRead, s.runs)
	// What each agent may do on one integration (access.go). The read and the
	// plan are the read verb. The save is its own verb, carrying no credential
	// so a person's assistant may prepare it, and answers `dry_run=true` as its
	// check (ADR-0055). Seat actions are the connect verb: a rotate seals a
	// credential, a remove takes an account away at the vendor.
	mount("GET /setup/integrations/{kind}/access", authz.ActionSetupRead, s.accessRead)
	// An agent's request for one grant, named by its question's options by id:
	// the Inbox builds the grant's draft from it (accessgrants.go).
	mount("GET /setup/access/requests/{id}", authz.ActionSetupRead, s.accessRequest)
	// One agent's lane, in words.
	mount("GET /setup/integrations/{kind}/access/seats/{handle}/{lane}", authz.ActionSetupRead,
		s.accessSeatRead)
	// A live view redrawn from a form's values is a read sent as a POST, as
	// the plan is: it holds public names and public halves alone (draw.go).
	if err := router.Handle("POST /setup/integrations/{kind}/draw",
		authz.Policy{Action: authz.ActionSetupRead, Effect: authz.EffectReads},
		http.HandlerFunc(s.draw)); err != nil {
		failures = append(failures, err)
	}
	// The plan is a read sent as a POST (a draft is a body), so it keeps the
	// read verb.
	if err := router.Handle("POST /setup/integrations/{kind}/access/plan",
		authz.Policy{Action: authz.ActionSetupRead, Effect: authz.EffectReads},
		http.HandlerFunc(s.accessPlan)); err != nil {
		failures = append(failures, err)
	}
	if err := router.Handle("PUT /setup/integrations/{kind}/access", accessSavePolicy(),
		http.HandlerFunc(s.accessSave)); err != nil {
		failures = append(failures, err)
	}
	// A seat action acts on the account its path names at the vendor (rotate,
	// remove, restore), so it rewrites.
	post("POST /setup/integrations/{kind}/access/seats/{handle}/{lane}/{action}",
		authz.EffectRewrites, s.accessAct)
	// A Slack app's install link, minted on request and never stored: the one
	// seat action that answers something other than the tab.
	post("POST /setup/integrations/slack/access/seats/{handle}/{lane}/install",
		authz.EffectCreates, s.accessInstall)
	// A credential a person created at the vendor, checked and sealed for one
	// agent.
	post("POST /setup/integrations/{kind}/access/seats/{handle}/{lane}/credential",
		authz.EffectRewrites, s.accessCredential)
	// An agent's own identity at the vendor (accessidentity.go): kept, or given
	// up for the company's. Each may seal or clear a credential.
	post("POST /setup/integrations/{kind}/access/seats/{handle}/{lane}/identity",
		authz.EffectRewrites, s.accessIdentity)
	mount("DELETE /setup/integrations/{kind}/access/seats/{handle}/{lane}/identity", authz.ActionSetupConnect,
		s.accessIdentity)
	// The SSH servers (ssh.go). A scan dials an address an administrator typed,
	// so it is the connect verb, reading; a server's fields are a revision; the
	// setup is a read of what it renders.
	post("POST /setup/integrations/ssh/hosts/scan", authz.EffectReads, s.sshScan)
	mount("PUT /setup/integrations/ssh/hosts/{id}", authz.ActionSetupConnect, s.sshPutHost)
	mount("DELETE /setup/integrations/ssh/hosts/{id}", authz.ActionSetupConnect, s.sshDeleteHost)
	mount("PUT /setup/integrations/ssh/hosts/{id}/inventory", authz.ActionSetupConnect, s.sshPutInventory)
	mount("GET /setup/integrations/ssh/hosts/{id}/setup", authz.ActionSetupRead, s.sshSetup)
	// A test signs certificates and dials the server, so it is the connect
	// verb; the audit is an administrator's read.
	post("POST /setup/integrations/ssh/hosts/{id}/test", authz.EffectReads, s.sshTest)
	mount("GET /setup/integrations/ssh/audit", authz.ActionSSHAuditRead, s.sshAudit)
	// A rotation seals a new authority or replaces the current one.
	post("POST /setup/integrations/ssh/rotation", authz.EffectRewrites, s.sshRotation)
	return errors.Join(failures...)
}

// mayWriteCredentials asks the credential write a route performs on top of its
// verb, answering the refusal when there is none, and reports whether the
// handler may go on. It asks the route's own guard, and before anything is
// sealed or minted, so a refused caller leaves nothing behind.
func (s *Service) mayWriteCredentials(w http.ResponseWriter, r *http.Request) bool {
	return authz.Admit(w, r, s.guard, authz.Policy{Action: authz.ActionSecretWrite})
}

// carriesCredential reports whether a submission writes a value the sealed
// store will hold: any field its requirements declare a [setup.KindSecret],
// whether the caller supplied it or asked the engine to mint it.
func carriesCredential(values map[string]string, against []setup.Requirement) bool {
	for _, req := range against {
		if req.Kind != setup.KindSecret {
			continue
		}
		if _, present := values[req.Field]; present {
			return true
		}
	}
	return false
}

// configWriter adapts the config surface to what setup.Writer needs, so setup
// imports no HTTP service to write and a test can drive it with something else.
type configWriter struct{ svc *configapi.Service }

func (c configWriter) Apply(
	ctx context.Context, patch []byte, summary string, by iam.Actor, expect string,
) (string, int64, error) {
	applied, err := c.svc.Apply(ctx, configapi.ApplyRequest{
		Patch: patch, Summary: summary, By: by, Expect: expect,
		// What setup records (an app GitHub created, a site Atlassian named)
		// is the engine's to write.
		Engine: true,
	})
	return applied.RevisionID, applied.Epoch, err
}

// Authorize is [configapi.Service.Authorize], so /setup refuses exactly the
// credentials /config does, before any side effect.
func (c configWriter) Authorize(by iam.Actor) error {
	return c.svc.Authorize(by)
}

// Current is the fleet's revision ([configapi.Service.ActiveRevision]).
func (c configWriter) Current(ctx context.Context) (string, error) {
	return c.svc.ActiveRevision(ctx)
}

// Reload is [configapi.Service.Reload], held to the revision the submission was
// read from, as Apply is.
func (c configWriter) Reload(ctx context.Context, summary string, by iam.Actor,
	expect string) (string, int64, error) {

	applied, err := c.svc.Reload(ctx, summary, by, expect)
	return applied.RevisionID, applied.Epoch, err
}

// Seat and SetSeat are the per-seat write through the entity route: a merge
// patch cannot reach one list element without replacing the list.
func (c configWriter) Seat(ctx context.Context, handle string) ([]byte, error) {
	entity, err := c.svc.Entity(ctx, "roles", handle)
	if err != nil {
		return nil, err
	}
	return json.Marshal(entity)
}

func (c configWriter) SetSeat(
	ctx context.Context, handle string, body []byte, summary string, by iam.Actor,
	expect string,
) (string, int64, error) {
	applied, err := c.svc.ApplyEntity(ctx, configapi.ApplyEntityRequest{
		Kind: "roles", ID: handle, Body: body, Summary: summary, By: by,
		Expect: expect, Engine: true,
	})
	return applied.RevisionID, applied.Epoch, err
}

// ToolState is one integration's setup state: what is still missing, and where
// it goes. The reconcile half is deliberately absent; GET /integrations already
// answers it from the fleet's status rows, and a second derivation is how two
// screens disagree about health.
type ToolState struct {
	Kind         integration.Kind    `json:"key"`
	Configured   bool                `json:"configured"`
	Enabled      bool                `json:"enabled"`
	Requirements []setup.Requirement `json:"requirements"`

	// SSH is the SSH integration's servers and authority lines, on its own
	// answer alone.
	SSH *SSHServers `json:"ssh,omitempty"`

	// Guide is the app's intro, setup steps and caution, flattened onto this
	// answer; it lives in Go so the screen knows nothing about any app.
	setup.Guide

	// Satisfied reports that nothing required is outstanding. Not a health
	// claim: a satisfied integration can still refuse every delivery.
	Satisfied bool `json:"satisfied"`

	// FormComplete is whether every box the dialog draws is answered (the
	// company block's requirements and every seat's). Separate from Satisfied:
	// a GitHub agent with no app is unsatisfied yet has nothing to type, since
	// both acts producing an app happen at GitHub, so a Continue button there
	// would open a form that could do nothing.
	FormComplete bool `json:"form_complete"`

	// InboundPath is where this app's deliveries arrive, PublicURL that path on
	// the address apps reach this deployment at. Empty with no inbound route or
	// no base.
	InboundPath string `json:"inbound_path,omitempty"`
	PublicURL   string `json:"public_url,omitempty"`

	// CanProvision reports that this build runs a provisioning pass for the
	// app, so the screen offers the button only where something is behind it.
	CanProvision bool `json:"can_provision"`

	// SeatsRequired is whether a seat without its own credential leaves the app
	// unfinished. True for Slack alone: an agent with no Slack app cannot post.
	// Elsewhere a seat credential upgrades a working app (Datadog routes alerts
	// with no agent accounts), so an empty roster is a choice, and reporting it
	// as work left put a Continue button on every connected card.
	SeatsRequired bool `json:"seats_required,omitempty"`

	// ManagePath is what a person clicks at the app to delete one agent's app,
	// once a seat's ManageURL has opened it. The same for every seat, so stated
	// once. Empty where the engine removes what it made, which is every app but
	// the two whose per-agent apps only their owner can delete.
	ManagePath string `json:"manage_path,omitempty"`

	// HeldBy is the Crewlet service the document declares in this surface's
	// place ("doclet" for Confluence, "worklet" for Jira), so its block cannot
	// be written beside it; empty where it can. The connect form leaves such a
	// surface out and says why, rather than meeting the validator's conflict.
	HeldBy string `json:"held_by,omitempty"`

	// NeedsOperator is the transient administrator credential the pass asks for
	// on every run, or null. Never stored.
	NeedsOperator *setup.Requirement `json:"needs_operator,omitempty"`

	// Seats are the per-seat requirement lists for an app whose credentials
	// live on the seat, not the company. Slack is the one: each agent has its
	// own bot token and signing secret, and a submission names its seat.
	Seats []SeatState `json:"seats,omitempty"`
}

// SeatState is one seat's setup for a per-seat third-party app.
type SeatState struct {
	Handle       string              `json:"handle"`
	Name         string              `json:"name,omitempty"`
	Requirements []setup.Requirement `json:"requirements"`
	Satisfied    bool                `json:"satisfied"`

	// InboundPath is where this seat's deliveries arrive (per seat only on a
	// per-seat app), PublicURL that path on the address apps reach this
	// deployment at.
	InboundPath string `json:"inbound_path,omitempty"`
	PublicURL   string `json:"public_url,omitempty"`

	// Manifest is the app definition this seat's own app is created from, ready
	// to paste, for an app whose seats a person builds by hand. The engine's
	// own, byte for byte what the reconcile loop creates apps from: a wrong
	// scope, event or request URL yields a bot that reports success and sees an
	// empty workspace. Empty with no public address (the request URL is built
	// from it) or no manifest for the app.
	Manifest string `json:"manifest,omitempty"`

	// ManifestNote is why there is no manifest when there could have been one.
	// A silent absence is the worst answer, since the field help tells an
	// operator to paste one: a role name past Slack's app-name cap, or no
	// public address, would leave an instruction pointing at nothing.
	ManifestNote string `json:"manifest_note,omitempty"`

	// Secrets names, sorted, the variables this seat's own credentials live in.
	// Names, never values, as [setup.Requirement.SecretName]. It is what a
	// disconnect leaves behind: walking only company-level requirements never
	// named a seat's own minted token.
	Secrets []string `json:"secrets,omitempty"`

	// Present is whether this seat has started: something is written for it,
	// working or not. Explicit because the roll-up once indexed
	// `Requirements[0]`, which only Slack fills, and panicked when a second app
	// grew a roster.
	Present bool `json:"present"`

	// Enrolled is a seat this integration is meant to cover, holding anything
	// or not. Separate from Present: a GitHub seat with a `github` block and no
	// app has opted in with its first act still to do. Read as a choice it left
	// the card finished over an agent that could do nothing; reading every
	// listed seat as enrolled would make a roster into outstanding work for any
	// company running an integration for some agents.
	Enrolled bool `json:"enrolled,omitempty"`

	// Detail is the roster's one line under a seat's name: where its credential
	// is kept, or what is missing. Only Slack has a per-seat inbound path to
	// show; "no inbound path yet" on every other app said nothing about whether
	// the agent can act as itself.
	Detail string `json:"detail,omitempty"`

	// Step is what is outstanding for this seat, a closed set a screen renders
	// without reading Detail: "create_app" and "install_app". Empty means
	// nothing. Two steps because they are two acts by a person, minutes or days
	// apart, and one who did the first must be told the second is left.
	Step string `json:"step,omitempty"`

	// ActionURL is where a person does that step, when the engine can address
	// it. Empty for create_app by design: an app is created by POSTing a
	// manifest from a page carrying the operator's own GitHub session, so the
	// dashboard asks the begin route for a manifest and submits a form.
	ActionURL string `json:"action_url,omitempty"`

	// ManageURL is where a person DELETES what this seat holds at the app, when
	// the engine cannot. GitHub offers no endpoint for deleting an app
	// registration (only its owner, from settings), though a teardown can
	// uninstall it; the disconnect dialog hands over a link rather than claim
	// to have finished.
	ManageURL string `json:"manage_url,omitempty"`
}

// The steps a seat can be waiting on, as [SeatState.Step] spells them.
const (
	// StepCreateApp means this agent has no app of its own yet.
	StepCreateApp = "create_app"

	// StepInstallApp means the app exists and nothing has installed it, so
	// it can see no repository and mints no usable token.
	StepInstallApp = "install_app"
)

// fleetCompany is what this surface describes and writes: the fleet's company,
// the org it derives and the revision it is, from one read
// ([configapi.Service.FleetCompany]) because reads apart can straddle an
// activation.
//
// The fleet's, never what this node runs: writes build on the fleet's revision,
// and [setup.Requirement.Stored] decides whether a credential mints a pointer,
// writes through one, or is refused over a literal. A node mid-apply would mint
// a pointer over a literal the fleet's document held (`literal_in_config`).
type fleetCompany struct {
	company  *config.Company
	roster   *org.Organization
	revision string
}

// readCompany reads the fleet's company ([fleetCompany]), its org built as the
// engine builds it; a revision that derives none is an error, since no node
// runs it and an answer about its seats would be a guess.
func (s *Service) readCompany(ctx context.Context) (fleetCompany, error) {
	company, revision, err := s.config.FleetCompany(ctx)
	if err != nil {
		return fleetCompany{}, err
	}
	roster, err := company.Organization()
	if err != nil {
		return fleetCompany{}, fmt.Errorf("setupapi: the fleet's revision %s derives no "+
			"organization: %w", revision, err)
	}
	return fleetCompany{company: company, roster: roster, revision: revision}, nil
}

// companyFor is [Service.readCompany] for a request, answering it itself when
// there is no company to describe: none activated (409, with hint), one this
// node's keyring does not open, or a pointer the coordination store could not
// answer for (503). ok is false when it answered.
func (s *Service) companyFor(w http.ResponseWriter, r *http.Request, hint string) (fleetCompany, bool) {
	fc, err := s.readCompany(r.Context())
	switch {
	case err == nil:
		return fc, true
	case errors.Is(err, configapi.ErrNoActiveRevision):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNoActiveRevision,
			map[string]string{"hint": hint})
	case configapi.RefuseUnreadable(w, err):
	case errors.Is(err, coord.ErrUnavailable):
		log.WarnContext(r.Context(), "setup_company_unavailable", "error", err.Error())
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, authz.RetryUndecidedSeconds,
			httpjson.Detail{"reason": "the fleet's activation could not be read " +
				"from the coordination store"})
	case operator.AnswerCondition(r.Context(), w, log, "setup_company_unavailable", err, nil):
		// Any other condition of the node (a store busy or closed, a request
		// out of time) is told as one, never as the engine having broken.
	default:
		log.ErrorContext(r.Context(), "setup_company_unread", "error", err.Error())
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
	}
	return fleetCompany{}, false
}

// list serves GET /setup/integrations.
func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	fc, ok := s.companyFor(w, r,
		"no company configuration is active; import one before connecting an integration")
	if !ok {
		return
	}
	tools := make([]ToolState, 0, len(integration.Kinds))
	for _, kind := range integration.Kinds {
		state, ok := s.state(fc.company, fc.roster, kind)
		if !ok {
			continue
		}
		tools = append(tools, withLoopFindings(state, s.loopFindings(r.Context(), kind)))
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"tools": tools,
		// The revision this answer describes, for the caller to send back as
		// `if_match` ([fleetCompany]). Every write takes that precondition and
		// `setup.ErrStaleBase` refuses a moved base, but without a revision in
		// the read a client could use neither.
		"revision_id": fc.revision,
		// The address every inbound vendor is built on, once, as it is one
		// setting. No `present` or `resolved`: it is Tier A and required on a
		// node serving this screen. The key it is read from rides beside it
		// (two once a public listener is set), and whether it is loopback
		// ([config.LoopbackURL]): an address only this machine reaches is one
		// no vendor can deliver to.
		"external_url": map[string]any{
			"value": s.webhookBase, "config_path": s.webhookBaseKey,
			"loopback": config.LoopbackURL(s.webhookBase),
		},
	})
}

// one serves GET /setup/integrations/{kind}.
func (s *Service) one(w http.ResponseWriter, r *http.Request) {
	kind := integration.Kind(r.PathValue("kind"))
	fc, ok := s.companyFor(w, r, "no company configuration is active")
	if !ok {
		return
	}
	state, ok := s.stateFor(r.Context(), fc.company, fc.roster, kind)
	if !ok {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeUnknownKind, map[string]string{
			"detail": "this build serves no setup for " + string(kind),
			"hint":   "one of " + words.Or(integration.Kinds),
		})
		return
	}
	if kind == integration.KindSSH {
		servers, err := s.sshServers(r.Context(), fc.company, fc.revision)
		if err != nil {
			log.WarnContext(r.Context(), "setup_ssh_tests_unavailable", "error", err.Error())
			httpjson.UnavailableWith(w, httpjson.CodeUnavailable, authz.RetryUndecidedSeconds,
				httpjson.Detail{"reason": "the servers' latest tests could not be read from the coordination store"})
			return
		}
		state.SSH = servers
	}
	httpjson.Write(w, http.StatusOK, state)
}

// state builds one tool's answer, or reports that this build has no setup for
// it. An explicit absence, not an empty requirement list, which would answer
// "nothing is missing" under a Connect button collecting nothing.
func (s *Service) state(company *config.Company, roster *org.Organization,
	kind integration.Kind,
) (ToolState, bool) {
	var reqs []setup.Requirement
	var seats []SeatState
	var configured, enabled bool
	var guide setup.Guide
	var managePath, heldBy string
	switch kind {
	case integration.KindDatadog:
		block := company.Integrations.Datadog
		guide = datadog.Guide()
		reqs = datadog.Requirements(block, s.resolve)
		at := mcpEnvAt([]string{datadog.SeatEnv}, datadog.CredentialKeys)
		at.Identity = derivedIdentity(func(handle string) string {
			if block == nil {
				return ""
			}
			return datadog.AccountEmail(block.Provisioning, handle)
		})
		at.Configured = block != nil
		// No role on the row: each seat names its own Datadog role by id,
		// shown on the Agents tab from the organization's live list.
		seats = credentialSeats(roster, s.resolve, at, "Datadog", s.passes.Serves(kind))
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindCloudflare:
		block := company.Integrations.Cloudflare
		guide = cloudflare.Guide()
		reqs = cloudflare.Requirements(block, s.resolve)
		at := cloudflareAt()
		at.Identity = derivedIdentity(func(handle string) string {
			return cloudflare.TokenName(company.Name, handle)
		})
		at.Configured = block != nil
		seats = credentialSeats(roster, s.resolve, at, "Cloudflare", s.passes.Serves(kind))
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindGitHub:
		block := company.Integrations.GitHub
		guide = github.Guide()
		reqs = github.Requirements(block, s.resolve)
		// Not credentialSeats: a GitHub seat holds an app, not a pasted
		// credential ([githubSeats]).
		seats = githubSeats(company, roster, s.resolve)
		managePath = github.ManagePath()
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindJira:
		block := company.Integrations.Jira
		reqs = jira.Requirements(block, company.Integrations.Atlassian.IsCloud(), s.resolve)
		// No Forge app id is asked for: webhook administration
		// (/rest/webhooks/1.0/webhook) takes an API token on both
		// deployments; only the dynamic webhook API needs a Connect or OAuth
		// app. The route and config field stay as the fallback if Atlassian
		// retires the admin API.
		at := atlassianAt(atlassian.ProductJira)
		at.Identity = atlassianIdentity(s.resolve)
		at.Configured = block != nil
		seats = credentialSeats(roster, s.resolve, at, "Jira", false)
		heldBy = string(company.JiraHeldBy())
		// The Atlassian blocks have no `enabled` field: presence is the
		// switch, so a disconnect removes the block and enabled tracks
		// configured.
		configured, enabled = block != nil, block != nil
	case integration.KindConfluence:
		block := company.Integrations.Confluence
		reqs = confluence.Requirements(block, company.Integrations.Atlassian.IsCloud(), s.resolve)
		at := atlassianAt(atlassian.ProductConfluence)
		at.Identity = atlassianIdentity(s.resolve)
		at.Configured = block != nil
		seats = credentialSeats(roster, s.resolve, at, "Confluence", false)
		heldBy = string(company.ConfluenceHeldBy())
		configured, enabled = block != nil, block != nil
	case integration.KindAtlassian:
		block := company.Integrations.Atlassian
		guide = atlassian.Guide(block)
		reqs = atlassian.Requirements(block, s.resolve)
		at := atlassianAt(atlassian.ProductAny)
		at.Identity = atlassianIdentity(s.resolve)
		at.Configured = block != nil
		seats = credentialSeats(roster, s.resolve, at, "Atlassian",
			s.passes.Serves(kind))
		configured, enabled = block != nil, block != nil
	case integration.KindGitLab:
		block := company.Integrations.GitLab
		guide = gitlab.Guide()
		reqs = gitlab.Requirements(block, s.resolve)
		at := mcpEnvAt([]string{gitlab.SeatEnv}, gitlab.CredentialKeys)
		at.Identity = derivedIdentity(func(handle string) string {
			if block == nil {
				return ""
			}
			return gitlab.Username(block.Provisioning, handle)
		})
		at.Configured = block != nil
		seats = credentialSeats(roster, s.resolve, at, "GitLab",
			s.passes.Serves(kind))
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindMattermost:
		block := company.Integrations.Mattermost
		guide = mattermost.Guide()
		reqs = mattermost.Requirements(block, s.resolve)
		// Every agent posts as itself, so the company block alone connects
		// nobody (the admin token provisions; an agent uses its own bot
		// token). Without the roster the card read Connected over agents
		// with no bots.
		at := mattermostAt
		at.Identity = derivedIdentity(func(handle string) string {
			if block == nil {
				return ""
			}
			return "@" + mattermost.BotUsername(block.Provisioning, handle)
		})
		at.Configured = block != nil
		seats = credentialSeats(roster, s.resolve, at, "Mattermost",
			s.passes.Serves(kind))
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindExpo:
		// No roster: no seat names an Expo credential in its document; each
		// robot is on the Agents tab from the access ledger.
		block := company.Integrations.Expo
		guide = expo.Guide()
		reqs = expo.Requirements(block, s.resolve)
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindMetaAds:
		// No roster: no seat holds a token (ADR-0170).
		block := company.Integrations.MetaAds
		guide = metaads.Guide()
		reqs = metaads.Requirements(block, s.resolve)
		configured = block != nil
		enabled = block.GrowthEnabled()
	case integration.KindAppStoreConnect:
		// No roster: no seat holds a key (ADR-0170).
		block := company.Integrations.AppStoreConnect
		guide = appstoreconnect.Guide()
		reqs = appstoreconnect.Requirements(block, s.resolve)
		configured = block != nil
		enabled = block.GrowthEnabled()
	case integration.KindAppleAds:
		// No roster: no seat holds a key (ADR-0170).
		block := company.Integrations.AppleAds
		guide = appleads.Guide()
		reqs = appleads.Requirements(block, s.resolve)
		configured = block != nil
		enabled = block.GrowthEnabled()
	case integration.KindDigitalOcean:
		// No roster, as for Expo.
		block := company.Integrations.DigitalOcean
		guide = digitalocean.Guide()
		reqs = digitalocean.Requirements(block, s.resolve)
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindScaleway:
		// No roster, as for Expo; the key reaches the agent through the
		// engine's own sealed names.
		block := company.Integrations.Scaleway
		guide = scaleway.Guide()
		reqs = scaleway.Requirements(block, s.resolve)
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindAWS:
		// No roster, as for Expo.
		block := company.Integrations.AWS
		guide = awsint.Guide()
		reqs = awsint.Requirements(block, company.Name, s.resolve)
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindGoogleCloud:
		// No roster, as for Expo; the token is issued at use, never named
		// in the document.
		block := company.Integrations.GoogleCloud
		guide = googlecloud.Guide()
		reqs = append(googlecloud.Requirements(company.Name, block, s.resolve),
			googlegrowth.SignInClient(block, s.resolve)...)
		// THE KEY ALONE IS NOT A CONNECTION ANYBODY STARTED: the form makes it
		// when it opens (MintFirst), and a form opened and closed is nothing
		// to pause or finish.
		configured = block.Started()
		enabled = block != nil && block.Enabled
	case integration.KindSSH:
		// No roster; certificates are issued at use, never named in the
		// document.
		block := company.Integrations.SSH
		guide = sshint.Guide()
		reqs = sshint.Requirements(block, s.resolve)
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindAdMob:
		// No roster: every agent reaches AdMob through the company's consent
		// grant, and no seat holds a token (ADR-0170).
		block := company.Integrations.AdMob
		guide = admob.Guide()
		reqs = admob.Requirements(company)
		configured = block != nil
		enabled = block.GrowthEnabled()
	case integration.KindAdSense:
		// AdMob's shape: every agent reaches AdSense through the company's
		// consent grant, and no seat holds a token (ADR-0170).
		block := company.Integrations.AdSense
		guide = adsense.Guide()
		reqs = adsense.Requirements(company)
		configured = block != nil
		enabled = block.GrowthEnabled()
	case integration.KindGoogleAnalytics:
		// No roster and no credential: agents sign in as their growth
		// identities, which the Google identity pass makes.
		block := company.Integrations.GoogleAnalytics
		guide = googleanalytics.Guide()
		reqs = googleanalytics.Requirements(company)
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindGoogleAds:
		// No roster and no credential: as Google Analytics, spend held to
		// each account's caps.
		block := company.Integrations.GoogleAds
		guide = googleads.Guide()
		reqs = googleads.Requirements(company)
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindSearchConsole:
		// No roster and no credential: agents sign in as their growth
		// identities, which the Google identity pass makes.
		block := company.Integrations.SearchConsole
		guide = searchconsole.Guide()
		reqs = searchconsole.Requirements(company)
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindGooglePlay:
		// No roster and no credential: as Google Analytics, publishing held
		// for an administrator.
		block := company.Integrations.GooglePlay
		guide = googleplay.Guide()
		reqs = googleplay.Requirements(company)
		configured = block != nil
		enabled = block != nil && block.Enabled
	case integration.KindSlack:
		// The only per-seat vendor: the company block authenticates
		// nothing; every agent has its own Slack app.
		block := company.Integrations.Slack
		guide = slack.Guide()
		reqs = slack.CompanyRequirements(block, s.resolve)
		seats = slackSeats(roster, s.resolve, s.apps(), s.webhookBase, s.webhookBaseKey)
		managePath = slack.ManagePath()
		// Configured when any seat is: the block is optional settings.
		for _, seat := range seats {
			if len(seat.Requirements) > 0 && seat.Requirements[0].Present {
				configured, enabled = true, true
				break
			}
		}
	default:
		return ToolState{}, false
	}
	// A handle field is a picker and needs the roster to pick from; without it
	// it rendered one option, "Choose one", on a required field.
	seatChoices(roster, reqs)
	// What a reference currently reads as, for the links a form draws.
	// Credentials are skipped ([setup.FillEffective]).
	setup.FillEffective(reqs, s.resolve)
	// The engine's own addresses and the state of what a step connects first,
	// which no vendor's requirements can read off the document.
	setup.FillRedirects(reqs, s.signInCallback(), s.webhookBaseKey)
	for i, step := range guide.Prepare {
		if step.Needs != "" {
			connected := s.connected(company, roster, step.Needs)
			guide.Prepare[i].Connected = &connected
		}
	}
	setup.MarkDone(guide.Prepare, reqs)

	satisfied := len(setup.Outstanding(reqs)) == 0
	// Whether this dialog has a box left to fill, the one question Continue
	// can answer (the form edits the company block and the seat credentials).
	// A GitHub seat's requirements are deliberately empty (the engine writes
	// the app id, slug and key from GitHub's answer), so a company whose only
	// work is an agent's two clicks gets no button for a form that could not
	// help.
	formComplete := satisfied
	for _, seat := range seats {
		if len(setup.Outstanding(seat.Requirements)) > 0 {
			formComplete = false
			break
		}
	}
	// A tool is satisfied when every seat that has started is. A seat nobody
	// set up is a company's choice, but only where seats are load-bearing
	// (SeatsRequired), or an informational roster would leave every connected
	// card unfinished. Those are the two apps where the roster is the
	// integration: each Slack agent posts as itself, and a GitHub App is one
	// bot identity.
	seatsRequired := kind == integration.KindSlack || kind == integration.KindGitHub
	if seatsRequired {
		// "No agent can act" differs from any one seat's state: the loop below
		// only asks about started seats, so a company where nobody has started
		// would pass vacuously, Connected over "no app of its own yet".
		working := false
		for _, seat := range seats {
			if seat.Satisfied {
				working = true
				break
			}
		}
		if len(seats) > 0 && !working {
			satisfied = false
		}
		for _, seat := range seats {
			// Enrolled or started: two ways of having work outstanding, and
			// Present catches only the second. A GitHub seat with a `github`
			// block and no working app is opted in and is the seat the
			// reconcile reports on; leaving it out split the tag from the
			// roster.
			if (seat.Present || seat.Enrolled) && !seat.Satisfied {
				satisfied = false
				break
			}
		}
	}
	state := ToolState{
		Kind: kind, Configured: configured, Enabled: enabled, Guide: guide,
		Requirements:  reqs,
		Seats:         seats,
		Satisfied:     satisfied,
		FormComplete:  formComplete,
		InboundPath:   inboundPath(kind),
		CanProvision:  s.passes.Serves(kind),
		SeatsRequired: seatsRequired,
		ManagePath:    managePath,
		HeldBy:        heldBy,
		NeedsOperator: s.passes.Needs(kind),
	}
	if base := s.webhookBase; base != "" && state.InboundPath != "" {
		state.PublicURL = base + state.InboundPath
	}
	return state, true
}

// signInCallback is the redirect URI a consent product's client registers, ""
// where this surface signs nothing in or the engine has no public address.
func (s *Service) signInCallback() string {
	if s.consents == nil {
		return ""
	}
	return s.consents.SignInCallback()
}

// connected reports whether kind is connected as a step needing it reads it:
// on, with nothing it requires outstanding.
func (s *Service) connected(company *config.Company, roster *org.Organization, kind integration.Kind) bool {
	state, ok := s.state(company, roster, kind)
	return ok && state.Enabled && state.Satisfied
}

// credentialSeats is the roster of agents for an app whose seats each hold
// their own credential: one builder over every app, fed by the app's own list
// of where it keeps one, since a second list would drift and a seat whose
// credential was not found looks like one with none.
func credentialSeats(roster *org.Organization, resolve func(string) (string, bool),
	at seatCredentialAt, app string, provisions bool,
) []SeatState {
	out := []SeatState{}
	for role := range roster.AllRoles() {
		// Through the seat, as slackSeats does: the handle defaults from the
		// name and "is this a person" is the org model's question.
		seat := role
		if !seat.IsAgent() {
			continue
		}
		state := SeatState{Handle: seat.Handle(), Name: role.Name, Requirements: []setup.Requirement{}}
		stored, where := at.Find(role)
		state.Present = stored != ""
		state.Secrets = seatSecretNames(at, role, stored)
		switch {
		case !at.Configured:
			// No integration, so nothing is satisfied: nothing reads what the
			// seat holds. Said first as the larger fact; the operator needs a
			// sentence about the integration, not a variable.
			state.Detail = "waiting for " + app + " to be connected: this " +
				"company declares no " + app + " integration, so nothing " +
				"reads what this agent holds for it"
		case stored == "":
			// What happens next: the reconcile loop creates the account where
			// this build has a pass; where the app issues no credential on a
			// provisioner's behalf the next step is a person's. A seat opts in
			// by being turned on (D9), which the pass cannot do: the plan is
			// built from seats whose `integrations.<lane>` says `enabled: true`,
			// so "created on the next sync" is promised only to a seat a sync
			// will look at.
			if provisions {
				state.Detail = "not on " + app + " yet: turn this agent on at " +
					app + " on the integration's Agents tab, and the next sync " +
					"creates its account"
				break
			}
			state.Detail = "no " + app + " credential yet, and " + app +
				" issues none on request: add one to this seat's " + at.Address
		default:
			// Resolved, not merely written down: a ${VAR} naming a secret the
			// store lacks reads as configured elsewhere while the agent
			// authenticates with nothing.
			if _, ok := setup.Resolution(stored, resolve); ok != nil && !*ok {
				// Unless the engine is about to fill it: on a provisioned app a
				// named variable with nothing behind it is a seat that opted in
				// and is waiting (it puts the seat in the plan; the pass seals
				// the credential under that name). Reporting a fault would read
				// doing the right thing as a mistake.
				if provisions {
					state.Detail = "waiting for the next sync to create the account " +
						"and seal " + where
					break
				}
				state.Detail = where + " did not resolve, so this agent authenticates with nothing"
				break
			}
			state.Satisfied = true
			state.Detail = where
			if at.Identity != nil {
				if who := at.Identity(role); who != "" {
					state.Detail = who
				}
			}
		}
		out = append(out, state)
	}
	return out
}

// withLoopFindings un-satisfies the seats the reconcile loop has a finding
// about: [credentialSeats] only knows a credential is sealed where the app
// looks, so a key deleted at the vendor leaves the pointer resolving while
// every call is refused, and only the loop has asked the vendor.
//
// Advisories are skipped on the finding's own verdict (phase-ready), not a list
// of kinds. A kind this build cannot name is not skipped: a newer peer can
// write one, and treating it as an advisory would hide a dead agent.
func withLoopFindings(state ToolState, loop loopFound) ToolState {
	faults := make(map[string]integration.Finding, len(loop.findings))
	for _, f := range loop.findings {
		if f.Subject == "" {
			continue
		}
		if phase, _ := f.Kind.Verdict(); phase == integration.PhaseReady {
			continue
		}
		if _, seen := faults[f.Subject]; !seen {
			faults[f.Subject] = f
		}
	}
	if len(faults) == 0 && loop.unsure == "" {
		return state
	}
	seats := make([]SeatState, 0, len(state.Seats))
	for _, seat := range state.Seats {
		if f, named := faults[seat.Handle]; named {
			seat.Satisfied = false
			// The finding's own sentence, or its kind's where the record
			// kept it without its words.
			seat.Detail = integration.Classify([]integration.Finding{f}).Detail
		} else if loop.unsure != "" && seat.Satisfied {
			// NEVER SATISFIED ON FINDINGS NOT IN HAND: what the loop found
			// about this seat may be among them.
			seat.Satisfied = false
			seat.Detail = loop.unsure
		}
		seats = append(seats, seat)
	}
	state.Seats = seats
	return state
}

// loopFound is what the loop last recorded about one surface's seats: its
// findings, and why some that may name a seat are not in hand, "" where all
// are: the status or the findings unread, or the record cut a finding that
// may name a seat. No seat they may name is then confirmed ([withLoopFindings]).
type loopFound struct {
	findings []integration.Finding
	unsure   string
}

// The reasons a seat is not confirmed: what a reader could not read, and what
// the record's writer cut.
const (
	unsureUnread = "this node could not read everything the reconcile loop last found, so this " +
		"agent's credential cannot be confirmed just now"
	unsureCut = "the reconcile loop's last pass found more than its findings record keeps, and the " +
		"record cut findings that may name this agent, so its credential cannot be confirmed"
)

// loopFindings is what the loop last recorded about one surface: its status by
// key, then the findings it names by theirs, given their crossing. Not failing
// the screen when either cannot be read: the roster answers, unsure.
func (s *Service) loopFindings(ctx context.Context, kind integration.Kind) loopFound {
	state, err := s.currentState(ctx, kind)
	if err == nil && state.FindingsRecord.Named() {
		state, err = s.status.LoadFindings(ctx, state)
	}
	if err != nil {
		log.WarnContext(ctx, "setup_roster_without_loop_findings",
			"integration", kind.String(), "error", err.Error(),
			"detail", "the roster confirms no seat the loop's findings may name until they can be read again")
		return loopFound{unsure: unsureUnread}
	}
	found := loopFound{findings: state.Findings}
	switch {
	case state.FindingsUnread:
		found.unsure = unsureUnread
	case state.FindingsRecord.NamesOmitted > 0:
		found.unsure = unsureCut
	}
	return found
}

// seatCredentialAt is where one app keeps a seat's credential, passed in rather
// than switched on because the apps disagree: most hand a seat's token to a
// child MCP server through mcp_env, while Mattermost keeps it on the seat's own
// integration block. A roster knowing only the first listed no Mattermost
// agents.
type seatCredentialAt struct {
	// Find reports the value this seat holds for the app and the address it
	// sits at, both empty when none.
	Find func(role *org.Role) (stored, where string)
	// Address is where a seat holding nothing writes one: what the opt-in
	// sentence names, so the address an operator edits.
	Address string

	// Vars names every variable this seat's credentials live in, which may
	// exceed what [seatCredentialAt.Find] returns: Atlassian seals the seat's
	// address beside its token (its products authenticate
	// base64(address:token)). Nil derives it from Find, right for every app
	// sealing one value per seat.
	Vars func(role *org.Role) []string

	// Configured reports whether the company declares this app at all. Without
	// it a seat whose `${VAR}` resolved was satisfied on a surface the company
	// does not have (every agent ready on a disconnected Datadog). Per call
	// site: the Atlassian blocks have no `enabled` field, so presence is the
	// switch.
	Configured bool

	// Identity is who this agent is at the app, in the app's words (service
	// account, bot, registration); empty where nothing can say. It answers what
	// the roster is for, since where the credential is kept is identical for
	// every agent. Only over a working seat: a derived name is what the engine
	// would create, not proof it did.
	Identity func(role *org.Role) string
}

// mcpEnvAt is the seat credential of an app whose token reaches a child MCP
// server through the environment.
func mcpEnvAt(envs, keys []string) seatCredentialAt {
	return seatCredentialAt{
		Find: func(role *org.Role) (string, string) {
			return seatCredential(role.MCPEnv, envs, keys)
		},
		Address: "mcp_env." + envs[0],
	}
}

// stillUsedBySibling reports the credential names this disconnect must not call
// orphaned because a surface that is staying still reads them: Jira and
// Confluence share the `mcp_env.atlassian` token and address.
//
// A card disconnects jira, confluence, then the organization, and each request
// only asks (the block leaves the document when the loop finishes), so "still
// in the document" holds for all three and none would name the shared
// credential. A sibling itself disconnecting is therefore not a user (the fleet
// row's phase). A row this node cannot read is a live sibling: over-naming
// costs a question, under-naming costs the product.
func (s *Service) stillUsedBySibling(
	ctx context.Context, kind integration.Kind, company *config.Company,
	roster *org.Organization,
) func(string) bool {
	keep := map[string]bool{}
	for _, sibling := range atlassianSiblings[kind] {
		if !s.surfaceStaying(ctx, sibling, company) {
			continue
		}
		for _, name := range atlassianSeatVars(roster, sibling) {
			keep[name] = true
		}
	}
	if len(keep) == 0 {
		return func(string) bool { return false }
	}
	return func(name string) bool { return keep[name] }
}

// atlassianSiblings is which other surfaces read a credential this one would
// otherwise report orphaned. The organization is listed against the products,
// not the reverse: `integrations.atlassian` provisions a seat's account, and a
// company removing it while keeping Jira leaves every seat authenticating with
// the token it has.
var atlassianSiblings = map[integration.Kind][]integration.Kind{
	integration.KindJira:       {integration.KindConfluence},
	integration.KindConfluence: {integration.KindJira},
	integration.KindAtlassian:  {integration.KindJira, integration.KindConfluence},
}

// surfaceStaying reports a surface this company still declares and has not
// asked to disconnect.
func (s *Service) surfaceStaying(
	ctx context.Context, kind integration.Kind, company *config.Company,
) bool {
	if company == nil {
		return false
	}
	var declared bool
	switch kind {
	case integration.KindJira:
		declared = company.Integrations.Jira != nil
	case integration.KindConfluence:
		declared = company.Integrations.Confluence != nil
	case integration.KindAtlassian:
		declared = company.Integrations.Atlassian != nil
	}
	if !declared {
		return false
	}
	state, err := s.currentState(ctx, kind)
	if err != nil {
		// Unreadable is a live sibling ([Service.stillUsedBySibling]).
		log.WarnContext(ctx, "setup_sibling_phase_unreadable",
			"integration", kind.String(), "error", err.Error(),
			"detail", "a shared Atlassian credential is treated as still in "+
				"use, so it is not reported orphaned")
		return true
	}
	// The intent, not the phase ([integration.State.TearingDown]): the flag is
	// written on Disconnect and the phase follows only after a pass, so the phase
	// would read the gap as a staying sibling.
	return !state.TearingDown()
}

// atlassianSeatVars is every `${VAR}` one Atlassian surface reads a seat's
// credential out of, across every seat.
func atlassianSeatVars(roster *org.Organization, kind integration.Kind) []string {
	product := atlassian.ProductAny
	switch kind {
	case integration.KindJira:
		product = atlassian.ProductJira
	case integration.KindConfluence:
		product = atlassian.ProductConfluence
	}
	at := atlassianAt(product)
	var out []string
	for role := range roster.AllRoles() {
		out = append(out, seatSecretNames(at, role, "")...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// orphanedSecrets is every credential this company would still hold in its
// secret store after this integration is disconnected. Named, never deleted:
// the store is the company's and a value may be shared with another deployment.
//
// Seats' own credentials count (a pass's minted token, Atlassian's address
// slot, Slack's per-seat secrets), not only company-level requirements. Only
// what is stored is named: a requirement nobody set is a list nobody can act on.
func orphanedSecrets(kind integration.Kind, state ToolState) []string {
	out := []string{}
	add := func(reqs []setup.Requirement) {
		for _, req := range reqs {
			if req.Kind != setup.KindSecret || !req.Present {
				continue
			}
			if name, _, err := setup.PointerFor(kind, req, req.Stored); err == nil {
				out = append(out, name)
			}
		}
	}
	add(state.Requirements)
	for _, seat := range state.Seats {
		// A per-seat form's fields (Slack's) and the mcp_env slots every
		// other app seals into.
		add(seat.Requirements)
		out = append(out, seat.Secrets...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// seatSecretNames is the variables one seat's credentials for an app live in,
// through provision.SoleVar: a literal rather than a whole `${VAR}` names
// nothing, having no store row to orphan.
func seatSecretNames(at seatCredentialAt, role *org.Role, stored string) []string {
	var raw []string
	if at.Vars != nil {
		raw = at.Vars(role)
	} else {
		raw = []string{stored}
	}
	var out []string
	for _, value := range raw {
		if name, ok := provision.SoleVar(value); ok {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// atlassianAt reads a seat's Atlassian credential through the one reader all
// three surfaces share ([atlassian.CredentialAt]); per-surface lists drifted
// (a seat with `mcp_env.atlassian.JIRA_API_TOKEN` was configured under Jira and
// credential-less under Confluence). Unresolved: the roster reports where a
// credential is declared and resolves it separately.
func atlassianAt(product atlassian.Product) seatCredentialAt {
	return seatCredentialAt{
		// The seat's credential as the products read it
		// ([atlassian.CredentialOf]): the engine's sealed names first, then
		// `mcp_env`.
		Find: func(role *org.Role) (stored, where string) {
			cred := atlassian.CredentialOf(product, role, verbatim)
			if cred.Token == "" {
				return "", ""
			}
			if _, at := atlassian.CredentialAt(product, role.MCPEnv, verbatim); at != "" {
				return cred.Token, "mcp_env." + at
			}
			return cred.Token, "the engine's sealed credential"
		},
		// The shared block: where a provisioned seat's credential lands; the
		// product-specific blocks exist only where somebody chose one.
		Address: "mcp_env.atlassian",
		// Two slots per seat: the pass seals the address Atlassian assigns and
		// the token, and a disconnect orphans both.
		Vars: func(role *org.Role) []string {
			cred := atlassian.CredentialOf(product, role, verbatim)
			return []string{cred.Token, atlassian.SeatEmail(role.MCPEnv)}
		},
	}
}

// verbatim is the resolver [atlassian.CredentialAt] takes when the caller
// wants the reference rather than the value.
func verbatim(v string) string { return v }

// atlassianIdentity is the account an Atlassian seat authenticates as: recorded,
// not derived, since Atlassian assigns the address at creation and the pass
// writes it onto the seat. Resolved, as the document holds a reference.
func atlassianIdentity(resolve func(string) (string, bool)) func(*org.Role) string {
	return func(role *org.Role) string {
		return setup.Deref(atlassian.SeatEmail(role.MCPEnv), resolve)
	}
}

// derivedIdentity is the account an app's provisioner names a seat by, for apps
// that derive it from the handle. The value alone, no noun in front: the row is
// matched against the app's user list. It is the pass's own function, so a
// second rule cannot name an account nothing created.
func derivedIdentity(name func(handle string) string) func(*org.Role) string {
	return func(role *org.Role) string {
		return name(role.Handle())
	}
}

// cloudflareAt is a seat's Cloudflare token where the engine binds it, the
// header Cloudflare's server reads, with the binding's prefix taken off so the
// sealed reference resolves as the reference it is.
func cloudflareAt() seatCredentialAt {
	at := mcpEnvAt([]string{cloudflare.SeatEnv}, cloudflare.CredentialKeys)
	prefix := ""
	if b, ok := config.SeatToolBindingFor(cloudflare.Backend); ok && len(b.Keys) > 0 {
		prefix = b.Keys[0].Prefix
	}
	find := at.Find
	at.Find = func(role *org.Role) (string, string) {
		stored, where := find(role)
		return strings.TrimSpace(strings.TrimPrefix(stored, prefix)), where
	}
	return at
}

// mattermostAt is the seat credential of a Mattermost bot, which lives on the
// seat's own block rather than in mcp_env: see [config.RoleMattermost].
var mattermostAt = seatCredentialAt{
	Find: func(role *org.Role) (string, string) {
		value := strings.TrimSpace(role.Mattermost.BotToken)
		if value == "" {
			return "", ""
		}
		return value, "integrations.mattermost.bot_token"
	},
	Address: "integrations.mattermost.bot_token",
}

// seatCredential finds a seat's credential for one app and says where it is.
// The roster shows the mcp_env address, never the value, which has no business
// on this wire.
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

// discoverSite fills in the Atlassian address a submission left blank, only when
// the surface is Jira or Confluence, neither submission nor document names an
// address, and the company has an organization key to ask with. A typed site is
// a decision, never overridden.
//
// It returns a sentence when the organization could not be read, since writing
// the block anyway would be refused as a validation error against a field the
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
	// Both through the resolver, which takes the variable's name (the store is
	// keyed on what is inside the braces): the whole reference resolves nothing,
	// silently, and an unresolved org id asked Atlassian about an organization
	// literally named "${ATLASSIAN_ORG_ID}".
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

// seatChoices fills every handle requirement, in place, with the company's
// agent seats. Human seats are left out as the provisioners leave them out: an
// alert routed to a person is their own notification, not an agent's work.
func seatChoices(roster *org.Organization, reqs []setup.Requirement) {
	var choices []setup.Choice
	for role := range roster.AllRoles() {
		// Through the seat, as slackSeats does, so the identity rule has one
		// implementation.
		seat := role
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
	// Appended: a handle field may declare a non-seat choice of its own
	// (Datadog's fallback offers "None"), which the roster follows.
	for i := range reqs {
		if reqs[i].Kind == setup.KindHandle {
			reqs[i].Choices = append(slices.Clone(reqs[i].Choices), choices...)
		}
	}
}

// apps is which Slack app each seat authenticates as, or nil where no Slack
// transport is running.
func (s *Service) apps() map[string]string { return s.slackApps() }

// slackSeats is every agent seat's own Slack setup, not only configured ones:
// the list is what a form renders from, so omitting seats with no app would
// leave no way to give them one. Humans are excluded; the engine neither
// provisions nor holds a token for a person's Slack account.
func slackSeats(roster *org.Organization,
	resolve func(string) (string, bool),
	apps map[string]string, webhookBase, webhookBaseKey string,
) []SeatState {
	// What is built from this is pasted into Slack, which is why the address is
	// Tier A: as Tier B a verbatim `${PUBLIC_URL}/webhooks/slack/sre-lead` in a
	// manifest was refused by Slack with nothing naming the cause. Tier A
	// resolves before it decodes, so it is never a reference.
	base := webhookBase
	out := []SeatState{}
	for role := range roster.AllRoles() {
		// Through the seat, which owns the identity rule.
		seat := role
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
		// Which app this agent is: the running transport's answer, learned
		// from the vendor, since the document names none. Unknown (the seat may
		// not have come up, or no engine is beside this API), it falls back to
		// the credential's address, which is provable.
		if app := apps[handle]; app != "" {
			state.Detail = "App " + app
			// The page it is deleted from, handed over by a disconnect: Slack's
			// delete needs an app-configuration token an operator may lack.
			state.ManageURL = slack.ManageURL(app)
		} else {
			state.Detail = "integrations.slack.bot_token"
		}
		// The manifest, and where there is none, why. One without a request URL
		// receives nothing, so it waits for a public address. A refusal (Slack
		// caps an app name) is one seat's problem, not a reason to fail the
		// roster, but never silent: the field help tells an operator to paste
		// one.
		switch manifest, err := slack.ManifestJSON(role.Name, handle, base); {
		case base == "":
			state.ManifestNote = "no manifest yet: set `" + webhookBaseKey +
				"` in this node's Tier A file to the address this agent's " +
				"app delivers to, and restart the node"
		case err != nil:
			state.ManifestNote = err.Error()
		default:
			state.Manifest = manifest
		}
		if base != "" {
			state.PublicURL = base + state.InboundPath
		}
		out = append(out, state)
	}
	return out
}

// githubSeatSecrets names the `${VAR}`s one seat's app has sealed values in:
// the two the engine writes on converting a manifest, the private key GitHub
// returns once and the webhook signing secret. Per-seat, surviving a
// disconnect, typed by nobody, so this list is the only way an operator learns
// they exist.
func githubSeatSecrets(app *org.GitHubApp) []string {
	if app == nil {
		return nil
	}
	var out []string
	for _, ref := range []string{app.PrivateKey, app.WebhookSecret} {
		if name, ok := provision.SoleVar(ref); ok {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// repoScope is the repositories a finished seat works in. Empty means every one
// the installation covers; saying so beats a blank, which reads as reaching
// nothing.
func repoScope(repos []string) string {
	if len(repos) == 0 {
		return "Every repository the installation covers"
	}
	return strings.Join(repos, ", ")
}

// githubSeats is every agent seat's own GitHub App, and which of the two acts
// that produce one is still outstanding.
//
// Not [credentialSeats]: a GitHub seat holds an app (one bot identity, created
// from a manifest by somebody carrying a GitHub session, installed in a second
// act, its key written by the engine since GitHub returns it once), not a
// credential pasted into mcp_env.
//
// Every agent seat, configured or not, as the Slack roster lists them: this is
// where an operator starts the flow, so the seat with no app must be there.
// Humans are excluded; the engine creates no person's GitHub account.
func githubSeats(company *config.Company, roster *org.Organization,
	resolve func(string) (string, bool),
) []SeatState {
	_, webBase := company.Integrations.GitHub.Bases(resolve)
	out := []SeatState{}
	for role := range roster.AllRoles() {
		seat := role
		if !seat.IsAgent() {
			continue
		}
		app := role.GitHub
		state := SeatState{
			Handle: seat.Handle(), Name: role.Name,
			// A block is the opt-in, as the reconcile reads it: it reports only
			// seats that have one, so a company running GitHub for three of ten
			// agents is finished when those three are.
			Enrolled: app != nil,
			// No requirements, deliberately: the engine writes the app id, slug
			// and key from GitHub's answer, so a field would be a box nobody
			// can fill.
			Requirements: []setup.Requirement{},
			// What this seat has sealed, so a disconnect's `orphaned_secrets`
			// names the app key and webhook secret, not the company signing
			// secret alone. Not deleted, for [Engine.ForgetGitHubApp]'s reason:
			// the app survives a disconnect, GitHub having no delete API.
			Secrets: githubSeatSecrets(app),
		}
		// The manage link is on every seat with an app, whatever its step: the
		// engine can uninstall an app, only its owner can delete it.
		if app != nil && strings.TrimSpace(app.AppSlug) != "" {
			state.ManageURL = github.ManageURL(webBase, githubOrgOf(company), app.AppSlug)
		}
		switch {
		// No `configured` case, deliberately: agents each with their own app and
		// no `integrations.github` block is a supported shape (that block is the
		// organization-wide reconcile, not agent identity), and "waiting for
		// GitHub to be connected" would tell working agents they wait on nothing.
		//
		// A disconnect removes the installation and records it
		// ([githubPass.Teardown]), so the seat falls to the install step with
		// Satisfied false.
		case app == nil || app.AppID == 0:
			// No action URL, by contract: see [SeatState.ActionURL].
			state.Step = StepCreateApp
			state.Detail = "no app of its own yet, so this agent acts as nobody on GitHub"
		case app.InstallationID == 0:
			state.Present = true
			state.Step = StepInstallApp
			// The slug GitHub returned, never the name asked for: GitHub
			// slugifies and disambiguates, so a link from the name may open an
			// app that does not exist. No slug, no link.
			if slug := strings.TrimSpace(app.AppSlug); slug != "" {
				state.ActionURL = github.InstallURL(webBase, githubOrgOf(company), slug)
			}
			state.Detail = "the app exists and nothing has installed it, so it " +
				"sees no repository and mints no usable token"
		default:
			state.Present = true
			present, resolved := setup.Resolution(app.PrivateKey, resolve)
			switch {
			case !present:
				state.Detail = "the app is installed with no private key recorded, so " +
					"nothing can be minted for it: GitHub returns that key once, so " +
					"the app has to be created again"
			case resolved != nil && !*resolved:
				// The reference, never the key: only a whole ${VAR} can fail
				// to resolve, so this carries a variable name, never a PEM.
				state.Detail = app.PrivateKey + " did not resolve, so no token can " +
					"be minted for this seat and it reaches GitHub as nobody"
			default:
				state.Satisfied = true
				// Who it is, then what it reaches: the slug alone (the `[bot]`
				// suffix is the same on every row) is what an operator matches
				// against GitHub's pages. The scope follows only where the
				// installation narrows it.
				state.Detail = app.AppSlug
				if len(app.Repos) > 0 {
					state.Detail += " in " + strings.Join(app.Repos, ", ")
				}
				if state.Detail == "" {
					state.Detail = repoScope(app.Repos)
				}
			}
		}
		out = append(out, state)
	}
	return out
}

// inboundPath is where a third-party app's deliveries arrive, for the surfaces
// this build serves setup for; a test holds it to the integrations answer's.
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
		// The Data Center route; Cloud registers one hook per event under
		// this prefix, so the surface shows the base.
		return "/webhooks/confluence"
	default:
		return ""
	}
}

// inputs serves POST /setup/integrations/{kind}/inputs.
func (s *Service) inputs(w http.ResponseWriter, r *http.Request) {
	kind := integration.Kind(r.PathValue("kind"))
	fc, ok := s.companyFor(w, r, "no company configuration is active")
	if !ok {
		return
	}
	company := fc.company
	state, ok := s.stateFor(r.Context(), company, fc.roster, kind)
	if !ok {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeUnknownKind, map[string]string{
			"hint": "one of " + words.Or(integration.Kinds),
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
		// Generate names the mintable fields the engine should produce.
		// Separate from Values so a client cannot ask for a mint and supply a
		// value together, and an empty Values string is never mistaken for one.
		Generate []string `json:"generate"`
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := decode(body, &req); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": plainjson.Unreadable("the body", err),
		})
		return
	}

	// A submission is for a seat or the company, each with its own requirement
	// list (Slack's credentials live on the seat).
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
			httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeInvalidInput, map[string]string{
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
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidInput, map[string]string{
			"detail": err.Error(),
		})
		return
	}
	if len(values) == 0 {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidInput, map[string]string{
			"detail": "the submission carried no values",
		})
		return
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := refuseUngated(values, against); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidInput, map[string]string{
			"detail": err.Error(),
			"hint": "the answer this submission gives needs that field; send " +
				"it, or choose the answer that does not",
		})
		return
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := refuseEmpty(values, against); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidInput, map[string]string{
			"detail": err.Error(),
		})
		return
	}
	// A credential in the submission is `secrets.write` on top of the route's
	// `setup.connect`, decided before anything is sealed.
	if carriesCredential(values, against) && !s.mayWriteCredentials(w, r) {
		return
	}

	// The site, where the organization knows it and the submission does not.
	// Config refuses Jira and Confluence with neither url nor cloud_id, and the
	// discovering pass only fills an existing block, so a blank connect would be
	// rejected first. Asking here lands the address in the same write as the
	// credentials, valid the moment the block exists.
	if note := s.discoverSite(r.Context(), company, kind, against, values); note != "" {
		httpjson.FailWith(w, http.StatusBadGateway, httpjson.CodeInvalidInput, map[string]string{
			"detail": note,
		})
		return
	}

	summary := auditSummary(kind, req.Summary)
	// On the revision the requirements were read from where the caller named
	// none: the write lands on that document or is refused as stale.
	expect := req.IfMatch
	if expect == "" {
		expect = fc.revision
	}
	result, err := s.writer.Write(r.Context(), against, setup.Submission{
		Kind: kind, Values: values, Seat: req.Seat,
		Summary: summary, By: attributionOf(r), Expect: expect,
	})
	if err != nil {
		s.refuse(w, r, err, result)
		return
	}

	// Names, never values, in the log and the answer: a submitted value must
	// not reach a log line, an error detail or a response body.
	log.InfoContext(r.Context(), "setup_inputs_written",
		"kind", kind, "revision", result.RevisionID, "epoch", result.Epoch,
		"secrets", strings.Join(result.Secrets, ","), "reloaded", result.Reloaded,
		"by", attributionOf(r).Name, "operator", attributionOf(r).OperatorID)

	after, afterErr := s.readCompany(r.Context())
	// The address this surface was set up against, for surfaces whose address
	// only a person can move. A pass-registered hook is re-stamped every tick;
	// a hand-made registration keeps the old address while the surface reports
	// ready, and the first symptom is an agent that stopped replying. Keyed on
	// the ingress, not on a pass running: Slack's Request URL has no write API
	// ([integration.Kind.Ingress]).
	if kind.Ingress() == integration.IngressOperator {
		s.recordEndpoint(r.Context(), kind, s.webhookBase)
	}
	// The state the write left, read back; if that fails the write still
	// landed, so the answer carries the state it was decided on.
	fresh := state
	if afterErr == nil {
		if refreshed, ok := s.state(after.company, after.roster, kind); ok {
			fresh = withLoopFindings(refreshed, s.loopFindings(r.Context(), kind))
		}
	} else {
		log.WarnContext(r.Context(), "setup_state_unread", "kind", kind,
			"revision", result.RevisionID, "error", afterErr.Error(),
			"detail", "the write landed; its answer carries the state read before it")
	}
	answer := map[string]any{
		"revision_id": result.RevisionID, "epoch": result.Epoch,
		"wrote_secrets": result.Secrets, "reloaded": result.Reloaded,
		"state": fresh,
	}
	if afterErr == nil {
		if held := s.forgeHeld(r.Context(), after.company, kind); held != "" {
			answer["forge_held"] = held
		}
	}
	httpjson.Write(w, http.StatusCreated, answer)
}

// forgeHeld is the sentence for a Jira or Confluence write that leaves the
// Forge relay refusing that product's events while an app id is set: a block
// with no address yet, or a url-named site whose cloud id cannot be read now.
// Reading it here is the lookup a new connection starts with. Empty otherwise.
func (s *Service) forgeHeld(ctx context.Context, company *config.Company, kind integration.Kind) string {
	if company == nil || (kind != integration.KindJira && kind != integration.KindConfluence) {
		return ""
	}
	value := func(v string) string { return setup.Deref(v, s.resolve) }
	if strings.TrimSpace(value(company.Integrations.ForgeAppID)) == "" {
		return ""
	}
	product := string(kind)
	relay := company.Integrations.ForgeRelay(value)
	for _, hold := range relay.Held {
		if hold.Product == product {
			return hold.Reason
		}
	}
	if site, ok := relay.Lookups[product]; ok {
		if _, err := s.cloudIDs.Resolve(ctx, site); err != nil {
			return config.ForgeLookupHold(product, err).Reason
		}
	}
	return ""
}

// disconnect serves DELETE /setup/integrations/{kind}.
//
// It removes the block from the company document and nothing else. Sealed
// values stay, inert: a credential may be shared with another deployment.
// `crewlet secrets unset` is the deliberate path; the answer names what is
// orphaned.
func (s *Service) disconnect(w http.ResponseWriter, r *http.Request) {
	kind := integration.Kind(r.PathValue("kind"))
	fc, ok := s.companyFor(w, r, "no company configuration is active")
	if !ok {
		return
	}
	company := fc.company
	state, ok := s.stateFor(r.Context(), company, fc.roster, kind)
	if !ok {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeUnknownKind, map[string]string{
			"hint": "one of " + words.Or(integration.Kinds),
		})
		return
	}
	if !state.Configured {
		// Already absent: 200, not 404, since the caller's goal is met.
		httpjson.Write(w, http.StatusOK, map[string]any{"key": kind, "removed": false})
		return
	}
	// A disconnect removes a block (at once when forced, through teardown
	// otherwise), so a managed document refuses it before the teardown is
	// queued: it would run at the app, then remove a block the managing system
	// puts back.
	if err := s.config.Authorize(attributionOf(r)); err != nil {
		s.refuse(w, r, err, setup.Result{})
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
				map[string]string{"detail": plainjson.Unreadable("the body", err)})
			return
		}
	}

	// Computed above the split, the only moment it can be: every name derives
	// from a `${VAR}` in the document, and both paths end with that block gone
	// (force within this request, the 202 path when teardown finishes).
	orphaned := orphanedSecrets(kind, state)
	orphaned = slices.DeleteFunc(orphaned, s.stillUsedBySibling(r.Context(), kind, company, fc.roster))

	// Asked for, not done here: the block carries the credential the teardown
	// authenticates with, so removing it now would strand every webhook and
	// account the integration holds with nothing to authenticate a retry.
	//
	// Force is the operator saying they will clean up at the app themselves;
	// an app that never accepts the delete (revoked token, gone instance) would
	// otherwise hold the integration in Disconnecting for ever.
	if !req.Force {
		if err := s.markDisconnecting(r.Context(), kind, req.RemoveSeats,
			attributionOf(r)); err != nil {
			refuseDisconnect(w, r, kind, err)
			return
		}
		log.InfoContext(r.Context(), "setup_disconnect_requested",
			"integration", kind, "remove_seats", req.RemoveSeats,
			"by", attributionOf(r).Name, "operator", attributionOf(r).OperatorID)
		httpjson.Write(w, http.StatusAccepted, map[string]any{
			"key": kind, "removed": false, "disconnecting": true,
			"remove_seats": req.RemoveSeats,
			"detail": "the engine is removing what this integration holds at the " +
				"third-party app; the block is dropped when that finishes",
			// Named on this path too: it is the dashboard's.
			"orphaned_secrets": orphaned,
		})
		return
	}

	// The base is the header, the one write here whose base is: a lost race
	// lost a stated precondition and is 412 ([configapi.RacedError.Status]),
	// where other writes state `if_match` in the body and lose as 409.
	//
	// The block and the agents on through it go in one revision
	// ([configapi.Service.Disconnect]): the block alone left agents on at an
	// integration the document refuses.
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	applied, err := s.config.Disconnect(r.Context(), configapi.DisconnectRequest{
		Kind:    string(kind),
		Summary: "disconnect " + string(kind),
		By:      attributionOf(r),
		Expect:  ifMatch,
		Stated:  ifMatch != "",
	})
	if err != nil {
		s.refuse(w, r, err, setup.Result{})
		return
	}
	// Forced, so what the app still holds is the operator's to remove. The
	// status row goes with the block, or it would report an unconfigured
	// surface. Under the surface's own guard: a reconcile tick that ran its pass
	// just before would write the row straight back, and the forced-away surface
	// would reappear healthy with no block and nothing to remove it.
	s.forgetUnderGuard(r.Context(), kind)
	log.InfoContext(r.Context(), "setup_disconnected",
		"kind", kind, "revision", applied.RevisionID, "seats_off", applied.SeatsOff,
		"by", attributionOf(r).Name, "operator", attributionOf(r).OperatorID)
	seatsOff := applied.SeatsOff
	if seatsOff == nil {
		seatsOff = []string{}
	}
	answer := map[string]any{
		"key": kind, "removed": true,
		"revision_id": applied.RevisionID, "epoch": applied.Epoch,
		// The agents this revision turned off, by handle; each keeps what it
		// was given, so reconnecting and turning it on restores it.
		"seats_off": seatsOff,
		// Named, not deleted (`crewlet secrets unset`).
		"orphaned_secrets": orphaned,
	}
	// A consent grant given up with the block, and what Google was asked.
	if applied.Consent != nil {
		answer["oauth"] = applied.Consent
	}
	httpjson.Write(w, http.StatusOK, answer)
}

// refuse maps a write failure onto this surface's answers.
func (s *Service) refuse(w http.ResponseWriter, r *http.Request, err error, partial setup.Result) {
	var literal *setup.ErrLiteralInConfig
	var stale *setup.ErrStaleBase
	var raced *configapi.RacedError
	var invalid *configapi.ValidationError
	var patchErr *configapi.PatchError
	var managed *configapi.ManagedError
	var unanswered *configapi.ActivationUnknownError
	var seal *setup.SealError
	var value *setup.ValueError
	var entity *configapi.EntityError
	// The names sealed before the write failed ([setup.Result] carries them),
	// on every arm below, empty for a refusal before anything was written: the
	// values are in the fleet's store whatever became of the document, a retry
	// overwrites them by name, and the names are the operator's only record of
	// values nothing may point at yet. Names, never values.
	sealed := httpjson.Detail{}
	if len(partial.Secrets) > 0 {
		sealed["wrote_secrets"] = partial.Secrets
	}
	with := func(detail httpjson.Detail) httpjson.Detail {
		maps.Copy(detail, sealed)
		return detail
	}
	switch {
	case errors.As(err, &managed):
		configapi.RefuseManaged(w, managed)
	case errors.As(err, &value):
		refuseValue(w, value)
	case errors.As(err, &literal):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeLiteralInConfig, map[string]string{
			"path": literal.Path,
			"detail": "this field holds a value rather than a ${VAR} reference, so " +
				"there is no variable to write the credential into",
			"hint": "replace it with a whole ${VAR} reference, or clear it and submit again",
		})
	case errors.As(err, &stale):
		// Refused before anything was written, unlike the config surface's
		// raced answer: no stored revision to name.
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeRevisionAdvanced, map[string]string{
			"your_base": stale.Base, "current_revision_id": stale.Current,
			"hint": "the configuration changed since you read it; re-read the " +
				"setup state and submit again",
		})
	case errors.As(err, &raced):
		extra := with(httpjson.Detail{
			"your_base": raced.Base,
			"hint": "another write activated first; re-read the setup state and " +
				"submit again",
		})
		if raced.Current != "" {
			extra["current_revision_id"] = raced.Current
		}
		// The revision it kept and the node holding it, both or neither, as the
		// config surface names them: a revision that lost its activation is in
		// the history of the node that stored it only.
		maps.Copy(extra, configapi.KeptRevision(raced))
		// 412 where If-Match was the base (a disconnect's), 409 where it was a
		// body field.
		httpjson.FailWithFields(w, raced.Status(), httpjson.CodeRevisionAdvanced, extra)
	case errors.As(err, &invalid):
		// The config surface's structure (located problems, derived hierarchy),
		// so a screen reads one shape whichever surface refused.
		fields := with(configapi.RefusalFields(err))
		fields["detail"] = invalid.Err.Error()
		fields["hint"] = "the values are checked as the whole company document they " +
			"produce, so a field that is fine on its own is still refused " +
			"when it leaves the company invalid"
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeValidationError, fields)
	case errors.As(err, &patchErr):
		// validation_error, not invalid_patch: the caller submitted values and
		// this surface built the patch.
		fields := with(configapi.RefusalFields(err))
		fields["detail"] = patchErr.Err.Error()
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeValidationError, fields)
	case errors.Is(err, configapi.ErrNoActiveRevision):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeNoActiveRevision, with(httpjson.Detail{
			"hint": "import a company configuration first",
		}))
	case errors.As(err, &entity) && errors.Is(err, configapi.ErrNoSuchEntity):
		// The seat left the document between the read and the write: nothing
		// was written.
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNoSuchSeat, with(httpjson.Detail{
			"detail": entity.Err.Error(),
			"hint":   "the seat is no longer in the company; read the setup state again",
		}))
	case errors.As(err, &entity):
		// A seat edit the document cannot hold.
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidInput, with(httpjson.Detail{
			"detail": entity.Err.Error(),
		}))
	case errors.As(err, &unanswered):
		// Stored, activation unanswered: an unknown outcome, never "nothing
		// was written"; the document may already be the company.
		log.WarnContext(r.Context(), "setup_write_unknown", "revision", unanswered.Stored,
			"error", err.Error(), "secrets", strings.Join(partial.Secrets, ","))
		configapi.RefuseActivationUnknown(w, unanswered, sealed)
	case configapi.RefuseUnreadableWith(w, err, sealed):
		// A revision this node cannot open, met by the apply's own read (a
		// disconnect with no If-Match racing a pointer move): the config
		// surface's 409 with what was sealed, never a 500.
	case errors.As(err, &seal) && errors.Is(seal.Err, coord.ErrUnavailable):
		// A credential's own write unanswered, ahead of the arm below: the
		// secret store fails with the error a pointer read fails with, so that
		// arm would say "the activation could not be read" about a write that
		// may have landed. The document was untouched, so a resend rewrites
		// every credential by name.
		//
		// Named as a field, `unanswered_secret`, beside `wrote_secrets`: clients
		// branch on fields, and a bare 503 reads as nothing written. No
		// `outcome`: the remedy is the plain 503's.
		log.WarnContext(r.Context(), "setup_seal_unknown", "secret", seal.Name,
			"error", err.Error(), "secrets", strings.Join(partial.Secrets, ","))
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, authz.RetryUndecidedSeconds,
			with(httpjson.Detail{
				"unanswered_secret": seal.Name,
				"reason": "the credential " + seal.Name + " was written to the fleet's " +
					"secret store and the store did not answer, so whether it holds " +
					"the new value is unknown",
				"hint": "the company document was not changed; send the submission " +
					"again once the store answers, which writes every credential in " +
					"it again by name",
			}))
	case errors.Is(err, coord.ErrUnavailable):
		// The fleet's pointer could not be read: the document is unchanged and
		// it is retryable within a raft election, the config surface's own 503.
		// Not "nothing was written": credentials sealed before the failed read
		// stay in the store, named in the answer and the log.
		log.WarnContext(r.Context(), "setup_write_unavailable", "error", err.Error(),
			"secrets", strings.Join(partial.Secrets, ","))
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, authz.RetryUndecidedSeconds,
			with(httpjson.Detail{"reason": "the fleet's activation could not be read " +
				"from the coordination store"}))
	case operator.AnswerCondition(r.Context(), w, log, "setup_write_unavailable", err, sealed,
		"secrets", strings.Join(partial.Secrets, ",")):
		// Any other condition of the node (a store too busy or closed, a
		// request out of time) is the config surface's `503` in a person's
		// words with what was sealed: never "the engine broke" nor "nothing was
		// written".
	default:
		// The detail goes to the log, never the caller: an app's refusal can
		// quote the config value it was given, echoing a literal credential.
		log.ErrorContext(r.Context(), "setup_write_failed",
			"error", err.Error(), "secrets", strings.Join(partial.Secrets, ","))
		httpjson.FailWithFields(w, http.StatusInternalServerError, httpjson.CodeInternalError,
			with(httpjson.Detail{}))
	}
}

// attributionOf is who a write on this request is attributed to (author, kind,
// credential), kept by every revision, seat record and sealed row. Total, never
// empty, for [auth.AttributionOf]'s reason: a credential sealed under an empty
// author cannot be traced.
func attributionOf(r *http.Request) iam.Actor {
	return auth.AttributionOf(r.Context())
}

// disconnectRequest is what the Disconnect dialog sends.
type disconnectRequest struct {
	// RemoveSeats also removes the accounts this engine created at the app;
	// false removes only what the engine registered for itself.
	RemoveSeats bool `json:"remove_seats"`

	// Force drops the block without waiting for the teardown: the way out of
	// one that can never succeed (revoked credential, gone instance). The
	// operator removes what the app holds, so the answer names what was left.
	Force bool `json:"force"`
}

// refuseDisconnect answers a disconnect this node could not record: `503`
// where the same request may then work, `500` for a failure no condition
// claims, which no wait clears.
//
// A concurrent pass holding the surface is `surface_busy` under its own short
// wait, the one race a caller should sit out. The detail is words for a
// person ([builtin.ConditionSentence]), the error goes to the log: the error
// leaked the store's transport error, hosts and ports.
func refuseDisconnect(w http.ResponseWriter, r *http.Request, kind integration.Kind, err error) {
	code := httpjson.CodeUnavailable
	var after int
	var said string
	switch condition, isCondition := builtin.ConditionSentence(err); {
	case errors.Is(err, errSurfaceBusy):
		code, after = httpjson.CodeSurfaceBusy, httpjson.RetrySeconds(RetryBusy)
		said = "This integration is being set up right now, so the " +
			"disconnect didn't start. Try again in a moment."
	case isCondition:
		said = condition
		after = httpjson.RetrySeconds(builtin.RetryAfter(err,
			time.Duration(authz.RetryUndecidedSeconds)*time.Second))
	default:
		// This node's own fault, which no wait clears; a 503 told a person to
		// try again in a moment.
		log.ErrorContext(r.Context(), "setup_disconnect_failed",
			"integration", kind, "error", err.Error())
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	log.WarnContext(r.Context(), "setup_disconnect_unrecorded",
		"integration", kind, "error", err.Error())
	httpjson.UnavailableWith(w, code, after, httpjson.Detail{"detail": said})
}

// errSurfaceBusy is [Service.markDisconnecting] refused by a concurrent writer,
// a state that clears on its own.
var errSurfaceBusy = errors.New("setupapi: this surface is being written at")

// disconnectRetryFor is how long one disconnect waits for a concurrent writer
// before answering "busy, try again". Deliberately short: a pass may hold the
// surface for setup.PassDeadline (minutes), and a request waiting that out is a
// tab spinning on a socket proxies close. It covers the common case, a tick a
// moment from finishing.
const disconnectRetryFor = 3 * time.Second

// disconnectRetryEvery is the retry cadence: six attempts per window, enough to
// catch a pass finishing, few enough that a dozen operators pressing Disconnect
// are no load.
const disconnectRetryEvery = 500 * time.Millisecond

// markDisconnecting records the intent on the fleet row, so the loop picks it
// up and the screen stops showing a connected integration. The row is written
// even when there is none, or a disconnect asked before the first pass would
// be lost.
func (s *Service) markDisconnecting(
	ctx context.Context, kind integration.Kind, removeSeats bool, by iam.Actor,
) error {
	// Under the surface's own lease ([setup.Runner.Hold]): a tick that read the
	// row earlier would write its own back over the intent, losing the
	// disconnect with the screen still reporting connected.
	_, release, held, err := s.passes.Hold(ctx, kind)
	if err != nil {
		return fmt.Errorf(
			"setupapi: this node could not take %s to record the disconnect: %w",
			kind, err)
	}
	// Waited out briefly before refusing ([disconnectRetryFor]): a refusal
	// would spend the operator's gesture on a race over before they read it.
	for deadline := time.Now().Add(disconnectRetryFor); !held && time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(disconnectRetryEvery):
		}
		if _, release, held, err = s.passes.Hold(ctx, kind); err != nil {
			return fmt.Errorf(
				"setupapi: this node could not take %s to record the disconnect: %w",
				kind, err)
		}
	}
	if !held {
		return fmt.Errorf(
			"%w: %s is being provisioned right now, so the disconnect was "+
				"not started; try again in a moment", errSurfaceBusy, kind)
	}
	defer release()

	state, err := s.currentState(ctx, kind)
	if err != nil {
		// Refused rather than written blind: otherwise a row says
		// Disconnecting over a state this node overwrote unread
		// ([Service.currentState]).
		return fmt.Errorf(
			"setupapi: the fleet's record of %s could not be read, so the "+
				"disconnect was not started: %w", kind, err)
	}
	// Through the package that owns the row ([integration.AskTeardown]), which
	// also records who asked ([integration.State.RequestedBy]) for the teardown's
	// writes.
	return s.status.RecordPass(ctx,
		integration.AskTeardown(state, kind, removeSeats, by))
}

// forgetUnderGuard removes a surface's status row with every other writer
// excluded. A failed guard is not a failed disconnect: the block is already
// gone, so the row is merely stale and the loop's sweep removes it later;
// refusing would report failure for work that succeeded.
func (s *Service) forgetUnderGuard(ctx context.Context, kind integration.Kind) {
	_, release, held, err := s.passes.Hold(ctx, kind)
	if err != nil || !held {
		log.WarnContext(ctx, "setup_status_not_forgotten",
			"integration", kind, "error", errorOrBusy(err),
			"detail", "another writer holds this surface, so its row is left "+
				"for the reconcile loop to sweep once the block is gone")
		return
	}
	defer release()

	if err := s.status.ForgetIntegration(ctx, kind); err != nil {
		log.WarnContext(ctx, "setup_status_not_forgotten",
			"integration", kind, "error", err)
	}
}

// githubOrgOf is the organization an agent's app is managed under.
func githubOrgOf(company *config.Company) string {
	gh := company.Integrations.GitHub
	if gh == nil || gh.Provisioning == nil {
		return ""
	}
	return strings.TrimSpace(gh.Provisioning.Org)
}

// stateFor is [Service.state] with the loop's per-seat findings folded in
// ([withLoopFindings]). Every single-surface answer uses it, so a roster read
// alone cannot disagree with the one in the list.
func (s *Service) stateFor(
	ctx context.Context, company *config.Company, roster *org.Organization,
	kind integration.Kind,
) (ToolState, bool) {
	state, ok := s.state(company, roster, kind)
	if !ok {
		return state, false
	}
	return withLoopFindings(state, s.loopFindings(ctx, kind)), true
}

// refuseValue answers a value its field refuses, before anything was written
// or sent: the caller's input, located at the field it was typed into, as a
// literal in the config is. Its sentence never holds a credential.
func refuseValue(w http.ResponseWriter, value *setup.ValueError) {
	httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidInput, map[string]string{
		"detail": value.Problem, "field": value.Field, "path": value.ConfigPath,
	})
}
