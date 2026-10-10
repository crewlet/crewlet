// Package webhooks is the API's inbound edge: the endpoints external systems
// POST to, and the provider credential each one is authenticated by.
//
// THESE ROUTES ARE OPEN — a vendor holds no key of this engine — which is why
// every one of them verifies a provider credential BEFORE the delivery is
// recorded, broadcast or republished. That ordering is the whole security
// property, and an earlier edge lost it twice: Slack skipped verification entirely
// when no secret was configured — so anyone who could reach the port could
// publish a raw_webhook addressed at any seat, and the engine woke that agent
// and drove a turn — while Jira and Confluence verified only inside their
// transports, by which point the payload had already been written to the event
// store and fanned out to every connected dashboard socket. Here the check is
// structural: accept takes a [verified], and only the guard below mints one.
//
// A ROUTE WITH NOTHING TO VERIFY WITH ANSWERS 503, NEVER 200 AND NEVER 4xx. A
// 4xx tells the sender its request was malformed and should be discarded — and
// the request is fine; what is missing is on this side. 503 with Retry-After is
// the honest answer: nothing crashed, this node cannot serve this delivery yet,
// and the delivery waits at the provider until somebody sets the secret. A
// signature that does not MATCH is the other case entirely and stays 401.
package webhooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/tracing"
)

var log = logging.Get("api.webhooks")

// The two Retry-After values, and the difference between them is the point.
const (
	// NoRevisionRetryAfter is what a node with no active company revision
	// asks for. Matched to the control plane's reconcile cadence: a node
	// that missed an activation picks the revision up on its next poll, so
	// telling a sender to come back sooner just burns deliveries against a
	// node that cannot have converged yet.
	NoRevisionRetryAfter = 15 * time.Second

	// NoSecretRetryAfter is what a route with no secret asks for.
	// Deliberately much longer: the unconfigured case resolves itself on
	// the next poll, this one waits on a human editing config, and a
	// sender hammering every 15 s in the meantime buys nothing.
	NoSecretRetryAfter = 5 * time.Minute
)

// verified is proof that a delivery authenticated.
//
// Only [Receiver.authenticate] mints one and [Receiver.accept] requires one, so
// a handler cannot reach the event store or the queue without having checked a
// provider credential first. The compiler holds an ordering that was previously
// a convention, and conventions are what the two regressions in this package's
// doc comment broke.
type verified struct {
	// source is the ROUTE that authenticated, which is not always the
	// integration the payload belongs to: Forge relays Jira and Confluence
	// events under its own JWT.
	source string
}

// Options wire the receiver.
//
// Secrets, Publisher, Claims, Configured and AppFlow are
// REQUIRED, and [New] refuses a missing one by name. The API mounts this beside
// an engine that supplies every one, so a nil is a wiring mistake, and a
// receiver that quietly did less around it (refusing every delivery, recording
// none, deduplicating nothing, reading as configured) would hide the mistake
// until a duplicate turn or an empty feed gave it away.
type Options struct {
	// Secrets reads the current epoch's verification material. Called per
	// request; see [Secrets]. A route whose secret is unset answers 503.
	Secrets func() Secrets

	// Publisher republishes an accepted delivery for the transports — the
	// wake — and publishes its record, [types.InboundDelivery], which the
	// node's publish listener files as the delivery's row and every node's
	// live projection hears. A record that fails to publish is logged and
	// does not fail the delivery.
	Publisher queue.Publisher

	// AppFlow finishes a GitHub App creation begun on the setup surface.
	// The redirect URL is baked into every app this engine creates, so the
	// callback has to be served wherever the setup surface is.
	AppFlow AppCompleter

	// Recheck asks the reconcile loop to look at GitHub immediately, for
	// the moment a person finishes installing an agent's app there.
	//
	// Nil waits out the cadence, which is what happened before this
	// existed: a surface owed to a GitHub admin backs off from fifteen
	// seconds to ten minutes ([integration.Schedule]), so the instant
	// somebody DOES the thing the card is asking for is the instant the wait
	// is longest.
	Recheck GitHubRechecker

	// Claims is the FLEET-WIDE dedupe.
	//
	// It is coordination state rather than store state because a
	// third-party app retrying a delivery reaches whichever ingress node
	// the load balancer picks: a claim only one node could see suppressed
	// nothing, and the same push woke the same seat twice. A registry that
	// cannot answer fails open; see [Receiver.claim].
	Claims coord.Claims

	// Configured reports whether a company revision is active here. An
	// unconfigured node cannot have the secrets a delivery is verified
	// with, so it answers 503 and the provider retries.
	Configured func() bool

	// Now is injectable so a test can pin the replay windows.
	Now func() time.Time

	// Keys verifies Forge invocation tokens. Nil uses Atlassian's
	// published JWKS.
	Keys KeySource
}

// Receiver serves the inbound edge.
type Receiver struct {
	secrets    func() Secrets
	publisher  queue.Publisher
	claims     coord.Claims
	configured func() bool
	now        func() time.Time
	forge      *forgeVerifier
	appFlow    AppCompleter

	// recheck asks the reconcile loop to look at GitHub now, and
	// recheckedAt is when this route last did. See [recheckEvery]: the
	// route is unauthenticated, so the ask is rate limited here rather
	// than at the loop, which cannot tell who asked.
	recheck     GitHubRechecker
	recheckMu   sync.Mutex
	recheckedAt time.Time
}

// New assembles the receiver, or refuses a missing required dependency by
// name. See [Options].
func New(opts Options) (*Receiver, error) {
	var missing []string
	for _, field := range []struct {
		name   string
		absent bool
	}{
		{"Secrets", opts.Secrets == nil},
		{"Publisher", opts.Publisher == nil},
		{"Claims", opts.Claims == nil},
		{"Configured", opts.Configured == nil},
		{"AppFlow", opts.AppFlow == nil},
	} {
		if field.absent {
			missing = append(missing, "Options."+field.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("webhooks: %s required: the receiver is mounted "+
			"beside the engine, which supplies every one of them",
			strings.Join(missing, ", "))
	}
	r := &Receiver{
		secrets:    opts.Secrets,
		publisher:  opts.Publisher,
		appFlow:    opts.AppFlow,
		recheck:    opts.Recheck,
		claims:     opts.Claims,
		configured: opts.Configured,
		now:        opts.Now,
	}
	if r.now == nil {
		r.now = func() time.Time { return time.Now().UTC() }
	}
	r.forge = newForgeVerifier(opts.Keys, r.now)
	return r, nil
}

// Routes registers every inbound endpoint on the API's mux.
//
// Registered on the caller's mux rather than served from an inner one, so each
// route's reach is declared where the route is — OPEN, every one: a vendor
// holds no key of this engine, and each route authenticates the delivery by
// the vendor's own signature or shared token before it does anything. A nested
// handler would put the declaration and the routes in two places that have to
// agree.
func (r *Receiver) Routes(mux auth.Router) {
	mux.HandleFunc("POST /webhooks/github", auth.ReachOpen, r.github)
	// The seat form, for an app belonging to one agent. Same handler:
	// what differs is only whether the path names a seat.
	mux.HandleFunc("POST /webhooks/github/{handle}", auth.ReachOpen, r.github)
	mux.HandleFunc("POST /webhooks/gitlab", auth.ReachOpen, r.gitlab)
	mux.HandleFunc("POST /webhooks/jira", auth.ReachOpen, r.jira)
	mux.HandleFunc("POST /webhooks/datadog", auth.ReachOpen, r.datadog)
	mux.HandleFunc("POST /webhooks/confluence", auth.ReachOpen, r.confluence)
	// The Cloud form, one path per event. Registered after the bare one
	// so a reader sees the pair together; the mux matches on the pattern,
	// not the order.
	mux.HandleFunc("POST /webhooks/confluence/{event}", auth.ReachOpen, r.confluenceCloud)
	mux.HandleFunc("POST /webhooks/slack/{handle}", auth.ReachOpen, r.slack)
	mux.HandleFunc("POST /webhooks/forge", auth.ReachOpen, r.forgeWebhook)
	mux.HandleFunc("GET /webhooks/slack-oauth", auth.ReachOpen, slackOAuthLanding)
	mux.HandleFunc("GET /webhooks/github-app", auth.ReachOpen, r.githubAppLanding)
}

// --- the shared pipeline ---------------------------------------------------

// scheme is how one provider's deliveries are authenticated, and it carries
// TWO questions rather than one.
//
// verify is the familiar half: does this body, under this secret, produce
// this signature. The two schemes that also need a header or a clock close
// over them, which is what keeps [Receiver.authenticate] one function rather
// than five.
//
// usable is what a scheme needs OF ITS OWN CONFIGURED SECRET before the
// question is worth asking, and it is a property of the scheme rather than of
// the route because that is where it stops being forgettable. For the signing
// schemes it is nil: an HMAC key's shape is refused where the key is written
// (see [whsec]), and its strength does not depend on how long an operator
// made it. For the two providers that CANNOT SIGN it is the whole of the
// security: the shared token is the entire check, so a token short enough to
// guess is a route with nothing to authenticate with, and saying so here
// means a ${VAR} pointing at a weak one is refused exactly where a weak
// literal is.
type scheme struct {
	verify func(body []byte, secret, signature string) bool
	usable func(secret string) error
}

// signed is a scheme whose secret needs nothing of it but existence.
func signed(verify func(body []byte, secret, signature string) bool) scheme {
	return scheme{verify: verify}
}

// sharedToken is the scheme for a provider that signs nothing, where the
// token IS the authentication — Datadog and Confluence Cloud.
//
//nolint:gochecknoglobals // a value, not state
var sharedToken = scheme{verify: verifyToken, usable: secrets.CheckSharedToken}

// serving answers 503 when no company revision is active here.
//
// FIRST, before the signature check, and deliberately so. A node with no
// revision has no secrets either, so verifying first would answer every
// delivery with the no-secret 503 and its five-minute Retry-After — telling a
// sender to wait out a human edit when what it is actually waiting for is a
// reconcile poll fifteen seconds away. Nothing is persisted by answering here,
// so the verify-before-persistence rule is untouched.
func (r *Receiver) serving(w http.ResponseWriter, source, event string) bool {
	if r.configured() {
		return true
	}
	log.Warn("webhook_rejected_unconfigured", "source", source, "event", event,
		"detail", "no company revision is active on this node, so the delivery "+
			"cannot be routed; answering 503 so the sender retries rather than discards it")
	unavailable(w, "unconfigured", NoRevisionRetryAfter)
	return false
}

// authenticate is the gate the whole package rests on.
//
// One function for the five HMAC routes AND the shape every other guard
// returns, so the refusal vocabulary — 503 with nothing to verify against, 401
// with a credential that did not match — is written once. Each route supplies
// its own scheme; nothing else about them differs.
func (r *Receiver) authenticate(w http.ResponseWriter, source, secret, signature string,
	body []byte, check scheme,
) (verified, bool) {
	if secret == "" {
		noSecret(w, source)
		return verified{}, false
	}
	// A SECRET THIS SCHEME CANNOT WORK WITH IS A SECRET IT DOES NOT HAVE,
	// and it gets the same 503: the route has nothing it can check a
	// delivery against, and a sender that retries while an operator fixes
	// the configuration is the outcome worth having.
	if check.usable != nil {
		if err := check.usable(secret); err != nil {
			weakSecret(w, source, err)
			return verified{}, false
		}
	}
	if signature == "" || !check.verify(body, secret, signature) {
		log.Warn("webhook_signature_invalid", "source", source)
		unauthorized(w, "invalid signature")
		return verified{}, false
	}
	return verified{source: source}, true
}

// delivery is one accepted webhook, in the one shape every record of it comes
// from — the queue envelope, the stored row and the live push. Building three
// shapes from three readings of the payload is how a feed row ends up
// describing a different event from the one that woke the agent.
type delivery struct {
	// source is the integration the PAYLOAD belongs to. Equal to the route
	// for six of the seven; Forge relays Jira and Confluence.
	source string

	// label is the event type the dashboard files this under.
	label   string
	summary string

	// body is what the provider SENT, parsed. It is what both records
	// show, so the delivery an operator opens is the one that was signed.
	body map[string]any

	// routed is what the transports receive, when that differs from what
	// arrived. Only Forge sets it: it relays Jira and Confluence events in
	// its own shape, and the transports read the native one. Nil means the
	// two are the same, which is the case for the other six.
	routed map[string]any

	raw []byte

	// handle names the seat a per-seat delivery was addressed to.
	handle string

	// forgeID is the Atlassian account behind a relayed Cloud event.
	forgeID string

	// key is the provider's own delivery id, empty when it sends none.
	key string

	// headers are the request's, credential-bearing ones redacted.
	headers map[string]string
}

// accept claims, republishes, records and answers one verified delivery.
//
// THE ORDER IS THE POINT. The claim comes first, because two concurrent
// retries must not both wake the seat. The republish comes next, because it is
// the only step that has to happen: a delivery that reached the queue will be
// worked even if this process dies in the next instruction. The store row and
// the live push come last and are best effort — observability must not be able
// to swallow a wake.
//
// A republish that fails RELEASES the claim and answers 503, so the provider's
// retry finds the delivery unclaimed. Recording first and publishing second
// would leave the opposite failure: a feed row saying the webhook arrived, an
// agent that never heard about it, and a retry refused by the claim.
func (r *Receiver) accept(w http.ResponseWriter, req *http.Request, v verified, d delivery, answer any) {
	ctx := req.Context()
	if !r.claim(ctx, d) {
		log.Debug("webhook_delivery_duplicate", "source", d.source, "key", d.key)
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}

	// THE TRACE A WEBHOOK STARTS, and the root of almost every trace this
	// engine produces: a delivery arrives, wakes a seat, and everything the
	// turn does hangs beneath it.
	//
	// A span rather than a bare minted id, so the arrival itself has a
	// duration and a name at the collector rather than being an id that
	// appears from nowhere. No third-party app Crewlet serves sends W3C traceparent
	// today, but the propagator is installed and an inbound one is honoured
	// if it ever is — which costs nothing and is what makes a delivery
	// forwarded through an operator's own gateway join their trace.
	ctx, span := tracing.Start(
		otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(req.Header)),
		"api.webhooks", "webhook.receive",
		attribute.String("crewlet.source", d.source),
		attribute.String("crewlet.seat", d.handle))
	defer span.End()

	trace := tracing.TraceOf(ctx)
	routed := d.routed
	if routed == nil {
		routed = d.body
	}
	ev := events.New(types.RawWebhook{
		Body:             routed,
		Headers:          d.headers,
		BodyRaw:          d.raw,
		Handle:           d.handle,
		ForgeAtlassianID: d.forgeID,
	}, trace)
	ev.Source = d.source

	if err := r.publisher.Publish(ctx, topics.NotificationsInbound, ev); err != nil {
		r.release(ctx, d)
		// TWO FAILURES, AND ONLY ONE OF THEM IS WORTH RETRYING. A broker
		// that is down comes back, so the provider should try again; a
		// delivery that does not fit on the wire will not fit on the
		// next attempt either, and answering it as an outage asks the
		// provider to repeat a request that cannot ever succeed —
		// claiming and releasing on every pass, forever, with nothing in
		// the loop reporting a size.
		//
		// The size is logged because it is the only place it appears: the
		// body passed the reader's own cap, and what made it too big
		// happened during encoding.
		if errors.Is(err, queue.ErrTooLarge) {
			log.Warn("webhook_event_too_large", "source", d.source, "route", v.source,
				"body_bytes", len(d.raw), "error", err,
				"detail", "the delivery was verified and is too large to publish; "+
					"refused permanently so the provider does not retry it")
			httpjson.FailWith(w, http.StatusRequestEntityTooLarge, httpjson.CodeBodyTooLarge,
				map[string]string{"detail": "too large to queue"})
			return
		}
		log.Error("webhook_publish_failed", "source", d.source, "route", v.source,
			"error", err, "detail", "the delivery was verified and could not be "+
				"queued; releasing its claim so the provider's retry is not refused")
		unavailable(w, "queue_unavailable", NoRevisionRetryAfter)
		return
	}

	log.Info("webhook_received", "source", d.source, "route", v.source,
		"event", d.label, "handle", d.handle)
	r.record(ctx, v, d)
	writeJSON(w, http.StatusOK, answer)
}

// claim reports whether this caller may handle the delivery.
//
// FAILS OPEN in both directions: a delivery with no key, or a store that
// cannot be reached, yields true. A duplicate is recoverable noise (the
// completion ledger collapses the turn), while a delivery dropped because the
// store blinked is a message nobody ever answers.
func (r *Receiver) claim(ctx context.Context, d delivery) bool {
	if d.key == "" {
		return true
	}
	won, err := r.claims.Claim(ctx, claimKey(d), coord.ClaimTTL, r.now())
	if err != nil {
		log.WarnContext(ctx, "delivery_dedupe_unavailable", "source", d.source, "error", err,
			"detail", "handling the delivery, which may duplicate one a peer took")
		return true
	}
	return won
}

func (r *Receiver) release(ctx context.Context, d delivery) {
	if d.key == "" {
		return
	}
	// context.WithoutCancel: the request context is already being torn
	// down on this path, and a release that skipped because the client
	// hung up would leave the claim standing for the whole TTL — which is
	// precisely the delivery this is trying to save.
	if err := r.claims.Release(context.WithoutCancel(ctx), claimKey(d)); err != nil {
		log.WarnContext(ctx, "delivery_release_failed", "source", d.source, "key", d.key, "error", err,
			"detail", "the provider's retry of this delivery will be refused until the claim expires")
	}
}

// claimKey is the fleet-wide identity of one delivery.
//
// The SOURCE is in it, so two third-party apps that happen to mint the same delivery
// id do not suppress each other — a UUID from one and a sequence number from
// another collide far more easily than either third-party app's own ids do.
func claimKey(d delivery) string { return d.source + "|" + d.key }

// record publishes the delivery's record, best effort: it runs after the wake
// is safely queued, and cannot fail the delivery.
//
// PUBLISHED, NOT APPENDED, though this node holds the store it would land in
// (the API runs only on a node with `data`): the Mattermost socket fleet runs
// on every node, stateless ones included, so a delivery reaches a data node's
// store only by being published, and ONE record that both edges publish is one
// rule for what a delivery row is — filed under its label, with the provider's
// bytes, by internal/observe. Publishing is also what puts the delivery on
// EVERY node's live projection rather than only on the one that took it, which
// a direct ingest into this node's stream could not.
func (r *Receiver) record(ctx context.Context, v verified, d delivery) {
	// WithoutCancel: the wake is already queued, so the record is owed
	// whatever the client does next. A caller that hangs up the instant it
	// is answered would otherwise cancel the publish and leave the delivery
	// invisible in the feed — work happening with no record of why.
	ctx = context.WithoutCancel(ctx)
	ev := events.New(deliveryRecord(v.source, d), tracing.TraceOf(ctx))
	ev.Source = d.source
	if err := r.publisher.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "webhook_delivery_unrecorded", "source", d.source,
			"route", v.source, "event", d.label, "error", err,
			"detail", "the delivery was queued and will wake its seat; only its "+
				"row on the integrations screen and in the event log is missing")
	}
}

// deliveryRecord is one delivery's record.
//
// WHO IT WAS FOR is a field, and so a tag on the row, rather than only inside
// the payload. The row is what a listing returns — the payload deliberately is
// not — so without it a deliveries screen could say a delivery arrived and not
// which seat it was addressed to. `recipient` rather than a name of this
// package's own: it is one of the keys the event store indexes as a PARTY, so
// it is also what makes `events?agent=<handle>` return what reached that seat
// from outside.
//
// THE ROUTE is the one that AUTHENTICATED, which the source is not on the
// Forge relay: a relayed Jira event belongs to `jira` and arrived at `forge`,
// and the relay's own row on the integrations screen counts by it.
//
// THE BODY is the provider's exact bytes — what opening the row shows — and
// every route here has already parsed them as a JSON object; the parsed body
// re-encoded is the fallback only for bytes that are not JSON at all, which no
// route admits today and a raw message could not carry.
func deliveryRecord(route string, d delivery) types.InboundDelivery {
	body := json.RawMessage(d.raw)
	if !json.Valid(body) {
		body, _ = json.Marshal(d.body)
	}
	return types.InboundDelivery{
		Label: d.label, Route: route, Text: d.summary,
		Recipient: d.handle,
		// The PROVIDER'S own delivery id, which is what an operator has
		// in front of them in the provider's console when they come here
		// asking what this engine did with it. Empty for the providers
		// that send none — a body hash is this edge's dedupe key, not
		// the provider's identity for anything.
		DeliveryKey: providerKey(d),
		Body:        body,
	}
}

// bodyKeyPrefix marks a dedupe key this edge derived from a body's bytes.
const bodyKeyPrefix = "body:"

// providerKey is the provider's own id for a delivery, or "" when it sent none.
//
// NOT the dedupe key whenever that is a hash of the body ([bodyKey]): the
// Forge relay, Datadog and an Atlassian build without the identifier header
// send no id, and this edge claims a hash of the bytes instead. Tagged as the
// provider's id, that hash drew under "Provider id" on the deliveries panel as
// though the provider had minted it, beside a dash that exists precisely to
// say "this provider sent no delivery id".
func providerKey(d delivery) string {
	if strings.HasPrefix(d.key, bodyKeyPrefix) {
		return ""
	}
	return d.key
}

// --- request plumbing ------------------------------------------------------

// sensitiveHeaders are redacted before a delivery's headers travel.
//
// A delivery's headers are persisted to the event store and rendered on the
// dashboard, so anything here that carries a SECRET is a secret at rest in
// the audit log — readable by everyone who can read an event, and impossible
// to un-write.
//
// `x-gitlab-token` is that. GitLab sends a hook's plaintext secret token
// verbatim in this header whenever one is set — by hand, by another tool, or
// by a provisioner that used `token` rather than `signing_token` — so it is a
// secret at rest the moment it is stored. This engine's provisioner sets
// `signing_token` only, and the route never authenticates on this header.
//
// SIGNATURE headers are deliberately NOT redacted, and the reason is not the
// one that used to be written here. It said "a transport re-verifies against
// them" — nothing does; no consumer of RawWebhook reads a signature at all.
// They stay because a signature is an HMAC OUTPUT rather than a key: it
// reveals nothing about the secret, and it is the evidence an operator needs
// to tell "the provider did not sign this" from "the provider signed it with
// the wrong key".
var sensitiveHeaders = map[string]bool{
	"authorization": true, "cookie": true, "proxy-authorization": true,
	"x-gitlab-token": true,
	// `x-crewlet-token` is this engine's OWN version of the same mistake.
	// Datadog and Confluence Cloud have no signature to send, so both
	// routes authenticate on a shared token carried in this header and
	// compared for EQUALITY: every accepted delivery therefore arrives
	// carrying the secret itself, not a value derived from it. Copied
	// through, it is written into the event the receiver publishes, kept
	// in the dead-letter stream for the retention window, and rendered on
	// the dashboard beside the payload. A signature header is safe here
	// and a KEY never is.
	"x-crewlet-token": true,
}

// safeHeaders flattens the request's headers, lowercased, with the
// credential-bearing ones redacted.
func safeHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for key, values := range h {
		name := strings.ToLower(key)
		if sensitiveHeaders[name] {
			out[name] = "REDACTED"
			continue
		}
		// Joined rather than first-wins: a repeated header is unusual and
		// dropping half of it silently would make the delivery a
		// transport re-verifies differ from the one that arrived.
		out[name] = strings.Join(values, ", ")
	}
	return out
}

// body reads and bounds the request body, answering the refusal itself.
func (r *Receiver) body(w http.ResponseWriter, req *http.Request) ([]byte, bool) {
	raw, err := readBody(w, req)
	if err == nil {
		return raw, true
	}
	if errors.Is(err, errBodyTooLarge) {
		log.Warn("webhook_body_too_large", "path", req.URL.Path, "limit", MaxBodyBytes)
		httpjson.Fail(w, http.StatusRequestEntityTooLarge, httpjson.CodeBodyTooLarge)
		return nil, false
	}
	// A read that failed part way is a client that hung up or a socket
	// that broke. There is nothing to verify and nobody left to tell, but
	// the status still has to be written or the handler returns 200.
	log.Warn("webhook_body_unreadable", "path", req.URL.Path, "error", err)
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable body"})
	return nil, false
}

// parseBody parses the body, answering 400 when it is not a JSON object.
func parseBody(w http.ResponseWriter, raw []byte) (map[string]any, bool) {
	body, ok := parseObject(raw)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return nil, false
	}
	return body, true
}

// headerOr reads a header, falling back when it is absent.
func headerOr(req *http.Request, name, fallback string) string {
	if v := req.Header.Get(name); v != "" {
		return v
	}
	return fallback
}

// statusOK is the answer five of the six routes give.
var statusOK = map[string]string{"status": "ok"}

// writeJSON is [httpjson.Write] under this package's own name.
func writeJSON(w http.ResponseWriter, status int, body any) {
	httpjson.Write(w, status, body)
}

func unavailable(w http.ResponseWriter, reason string, retryAfter time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	writeJSON(w, http.StatusServiceUnavailable,
		map[string]string{"status": "unavailable", "reason": reason})
}

func unauthorized(w http.ResponseWriter, reason string) {
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": reason})
}

// noSecret is the answer when a route has no secret to verify against.
// weakSecret refuses a route whose shared token cannot be the authentication.
func weakSecret(w http.ResponseWriter, source string, why error) {
	log.Error("webhook_secret_too_weak", "source", source, "error", why,
		"detail", "this route's provider signs nothing, so the shared token is the "+
			"whole check and this one is not strong enough to be it; answering 503 "+
			"so the sender retries rather than discards. Set a stronger token, or "+
			"press Generate in the setup form")
	// ITS OWN REASON, not the absent-secret one. The status is the same
	// because the truth is the same — this route cannot check a delivery —
	// but the two are different misconfigurations with different fixes, and
	// a caller correlating logs or a person reading the body should not have
	// to guess which of them they hit. Sharing the string also made the
	// distinction this function exists for invisible on the wire.
	unavailable(w, "weak_webhook_secret", NoSecretRetryAfter)
}

func noSecret(w http.ResponseWriter, source string) {
	log.Error("webhook_no_secret_configured", "source", source,
		"detail", "this route verifies a provider credential and has none to check "+
			"against, so it cannot accept deliveries; answering 503 so the sender "+
			"retries rather than discards them. Set the integration's secret to clear it")
	unavailable(w, "no_webhook_secret", NoSecretRetryAfter)
}

// bodyKey is the delivery identity of a third-party app that sends none.
//
// # Byte identity IS delivery identity here
//
// A Cloud event relayed through Forge carries no per-delivery header at all,
// and an Atlassian Data Center delivery carries one only from the versions
// that send X-Atlassian-Webhook-Identifier — so all three Atlassian routes
// reach here. What they do send is a payload that is byte-identical across
// the provider's own retries and different for any two distinct events: every
// one of these third-party apps stamps its payloads with entity ids and timestamps, so
// two events cannot serialize the same.
//
// # Why a hash of the whole body rather than derived coordinates
//
// Coordinates are the tempting shape — event, action, entity id, activity id
// — and they are strictly worse in the direction that matters. Every field
// left out of a coordinate set is a way for two DIFFERENT events to collapse
// into one, and a collapsed event is a message nobody ever answers. A hash
// over the whole body cannot do that: any difference at all yields a
// different key. Its failure mode is the opposite and the safe one — a
// third-party app that re-serialized between attempts would fail to collapse a
// redelivery, which is exactly today's behaviour and no worse.
//
// It is also the only derivation that needs to know nothing about the
// third-party app, which is what keeps three routes from each growing their own
// half-right field list.
func bodyKey(raw []byte) string {
	// Prefixed so a hash is never mistaken for the provider's own id —
	// see [providerKey].
	if len(raw) == 0 {
		// NOT a key. An empty body is the same for every delivery, and
		// keying on it would claim the first one and refuse every other
		// delivery from that third-party app for the whole TTL.
		return ""
	}
	sum := sha256.Sum256(raw)
	return bodyKeyPrefix + hex.EncodeToString(sum[:])
}
