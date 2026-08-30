// Package webhooks is the API's inbound edge: the endpoints external systems
// POST to, and the provider credential each one is authenticated by.
//
// THESE ROUTES ARE EXEMPT FROM THE API'S BEARER TOKEN, which is why every one
// of them verifies a provider credential BEFORE the delivery is recorded,
// broadcast or republished. That ordering is the whole security property, and
// an earlier edge lost it twice: Slack skipped verification entirely
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
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
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

// Emitter surfaces an accepted delivery on the live stream.
//
// Declared here rather than imported, so this package depends on the stream's
// shape and not on the stream. Satisfied by *stream.Service.
type Emitter interface {
	Ingest(livestate.Envelope)
}

// Options wire the receiver.
type Options struct {
	// Secrets reads the current epoch's verification material. Called per
	// request — see [Secrets]. Nil means no secret for anything, so every
	// route answers 503.
	Secrets func() Secrets

	// Publisher republishes an accepted delivery for the transports. THE
	// one required dependency: everything else here is observability, and
	// this is the wake.
	Publisher queue.Publisher

	// Events records the delivery for the dashboard's feed. Nil records
	// nothing, which is a standalone posture rather than a failure.
	Events *store.EventLog

	// Claims is the FLEET-WIDE dedupe. Nil handles every delivery, which
	// is what a single node without coordination already does.
	//
	// It is coordination state rather than store state because a vendor
	// retrying a delivery reaches whichever ingress node the load balancer
	// picks: a claim only one node could see suppressed nothing, and the
	// same push woke the same seat twice.
	Claims coord.Claims

	// Stream surfaces the delivery live. Nil pushes nothing.
	Stream Emitter

	// Configured reports whether a company revision is active here. Nil
	// reads as configured: an embedder that never wires it is running one
	// company from a file, and a receiver that answered 503 to everything
	// would make the omission look like an outage.
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
	events     *store.EventLog
	claims     coord.Claims
	stream     Emitter
	configured func() bool
	now        func() time.Time
	forge      *forgeVerifier
}

// New assembles the receiver.
func New(opts Options) *Receiver {
	r := &Receiver{
		secrets:    opts.Secrets,
		publisher:  opts.Publisher,
		events:     opts.Events,
		claims:     opts.Claims,
		stream:     opts.Stream,
		configured: opts.Configured,
		now:        opts.Now,
	}
	if r.secrets == nil {
		r.secrets = func() Secrets { return Secrets{} }
	}
	if r.configured == nil {
		r.configured = func() bool { return true }
	}
	if r.now == nil {
		r.now = func() time.Time { return time.Now().UTC() }
	}
	r.forge = newForgeVerifier(opts.Keys, r.now)
	return r
}

// Routes registers every inbound endpoint on the API's mux.
//
// Registered on the caller's mux rather than served from an inner one, so the
// auth middleware that exempts /webhooks/ wraps these exactly as it wraps
// everything else — a nested handler would put the exemption and the routes in
// two places that have to agree.
func (r *Receiver) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhooks/github", r.github)
	mux.HandleFunc("POST /webhooks/gitlab", r.gitlab)
	mux.HandleFunc("POST /webhooks/jira", r.jira)
	mux.HandleFunc("POST /webhooks/confluence", r.confluence)
	mux.HandleFunc("POST /webhooks/slack/{handle}", r.slack)
	mux.HandleFunc("POST /webhooks/forge", r.forgeWebhook)
	mux.HandleFunc("GET /webhooks/slack-oauth", slackOAuthLanding)
}

// --- the shared pipeline ---------------------------------------------------

// scheme is one provider's signature check: does this body, under this
// secret, produce this signature. The two schemes that also need a header or a
// clock close over them, which is what keeps [Receiver.authenticate] one
// function rather than five.
type scheme func(body []byte, secret, signature string) bool

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
	if signature == "" || !check(body, secret, signature) {
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
	// appears from nowhere. No vendor Crewlet serves sends W3C traceparent
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
			writeJSON(w, http.StatusRequestEntityTooLarge,
				map[string]string{"error": "delivery too large to queue"})
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
	r.record(ctx, d, trace)
	writeJSON(w, http.StatusOK, answer)
}

// claim reports whether this caller may handle the delivery.
//
// FAILS OPEN in every direction: no registry, no key, or a store that cannot
// be reached all yield true. A duplicate is recoverable noise — the completion
// ledger collapses the turn — while a delivery dropped because the store
// blinked is a message nobody ever answers.
func (r *Receiver) claim(ctx context.Context, d delivery) bool {
	if r.claims == nil || d.key == "" {
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
	if r.claims == nil || d.key == "" {
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
// The SOURCE is in it, so two vendors that happen to mint the same delivery
// id do not suppress each other — a UUID from one and a sequence number from
// another collide far more easily than either vendor's own ids do.
func claimKey(d delivery) string { return d.source + "|" + d.key }

// record writes the audit row and pushes the live one. Both are best effort:
// they run after the wake is safely queued, and neither can fail the delivery.
func (r *Receiver) record(ctx context.Context, d delivery, trace events.TraceContext) {
	// WithoutCancel: the wake is already queued, so the row is owed
	// whatever the client does next. A caller that hangs up the instant it
	// is answered would otherwise cancel the write and leave the delivery
	// invisible in the feed — work happening with no record of why.
	ctx = context.WithoutCancel(ctx)
	at := r.now()
	id := uuid.NewString()
	if r.events != nil {
		// The RAW bytes as the stored payload, not a re-serialization of
		// the parsed body: this row is what the dashboard shows when
		// somebody opens the delivery, and it should show what the
		// provider actually sent.
		if err := r.events.Append(ctx, store.EventRecord{
			ID: id, Type: d.label, Source: d.source, Time: at,
			Category: "webhook", Summary: d.summary, Actor: d.source,
			TraceID: trace.TraceID, SpanID: trace.SpanID,
			Payload: json.RawMessage(d.raw),
		}); err != nil {
			log.WarnContext(ctx, "event_store_write_failed", "source", d.source, "error", err)
		}
	}
	if r.stream == nil {
		return
	}
	// The engine never publishes these on crewlet.events.*, so the stream
	// service would otherwise never see them and the activity feed would
	// show a company that answered messages nobody sent.
	r.stream.Ingest(livestate.Envelope{
		ID: id, Type: d.label, Timestamp: at.Format(time.RFC3339Nano),
		Source: d.source, Actor: d.source, Summary: d.summary,
		Category: "webhook", TraceID: trace.TraceID, SpanID: trace.SpanID,
		Topic:   "crewlet.webhooks." + d.source,
		Payload: d.body,
	})
}

// --- request plumbing ------------------------------------------------------

// sensitiveHeaders are redacted before a delivery's headers travel.
//
// A delivery's headers are persisted to the event store and rendered on the
// dashboard, so anything here that carries a SECRET is a secret at rest in
// the audit log — readable by everyone who can read an event, and impossible
// to un-write.
//
// `x-gitlab-token` is that, and it is here because of what put it there. The
// provisioner registered the minted signing key in GitLab's plaintext
// `token` attribute rather than `signing_token`, so GitLab echoed a 32-byte
// HMAC key back on every single delivery — and it was copied verbatim into
// the stored headers. The provisioning bug is fixed and this engine no longer
// sets that field, but the header must be redacted regardless: a hook created
// by an older version still carries the old value and still sends it.
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
		writeJSON(w, http.StatusRequestEntityTooLarge,
			map[string]string{"error": "body too large"})
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		log.Error("webhook_encode_failed", "error", err)
		http.Error(w, `{"error":"encode_failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
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
func noSecret(w http.ResponseWriter, source string) {
	log.Error("webhook_no_secret_configured", "source", source,
		"detail", "this route verifies a provider credential and has none to check "+
			"against, so it cannot accept deliveries; answering 503 so the sender "+
			"retries rather than discards them. Set the integration's secret to clear it")
	unavailable(w, "no_webhook_secret", NoSecretRetryAfter)
}

// bodyKey is the delivery identity of a vendor that sends none.
//
// # Byte identity IS delivery identity here
//
// A Cloud event relayed through Forge carries no per-delivery header at all,
// and an Atlassian Data Center delivery carries one only from the versions
// that send X-Atlassian-Webhook-Identifier — so all three Atlassian routes
// reach here. What they do send is a payload that is byte-identical across
// the provider's own retries and different for any two distinct events: every
// one of these vendors stamps its payloads with entity ids and timestamps, so
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
// vendor that re-serialized between attempts would fail to collapse a
// redelivery, which is exactly today's behaviour and no worse.
//
// It is also the only derivation that needs to know nothing about the
// vendor, which is what keeps three routes from each growing their own
// half-right field list.
func bodyKey(raw []byte) string {
	if len(raw) == 0 {
		// NOT a key. An empty body is the same for every delivery, and
		// keying on it would claim the first one and refuse every other
		// delivery from that vendor for the whole TTL.
		return ""
	}
	sum := sha256.Sum256(raw)
	return "body:" + hex.EncodeToString(sum[:])
}
