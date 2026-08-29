package webhooks

import (
	"net/http"
)

// The six endpoints. Each one is the same five steps in the same order —
// read, answer readiness, AUTHENTICATE, parse, accept — and each differs only
// in the credential it checks and how it names the event. They are written out
// rather than driven from a table because what varies between them is the part
// worth reading, and a table would bury it in optional hooks.
//
// # Nothing unverified is decoded
//
// The raw body has to be buffered before the gate — the signature is over it,
// so there is no reading it afterwards — and that cost is unavoidable and
// bounded by MaxBodyBytes. Decoding it is not. Parsing first handed an
// unauthenticated caller a JSON unmarshal and a map several times the size of
// what they sent, on every request, for nothing: not one of these routes
// reads a body field before its gate.
//
// Two parse first and genuinely must. Slack reads `type` to answer readiness
// and to echo the url_verification handshake, which is deliberately answered
// without a signature; Forge reads `eventType` for the same readiness answer.
// Both say so at the line.

func (r *Receiver) github(w http.ResponseWriter, req *http.Request) {
	raw, ok := r.body(w, req)
	if !ok {
		return
	}
	event := headerOr(req, "X-GitHub-Event", "unknown")
	if !r.serving(w, "github", event) {
		return
	}
	v, ok := r.authenticate(w, "github", r.secrets().GitHub,
		req.Header.Get("X-Hub-Signature-256"), raw, verifyGitHub)
	if !ok {
		return
	}
	body, ok := parseBody(w, raw)
	if !ok {
		return
	}
	r.accept(w, req, v, delivery{
		source:  "github",
		label:   "webhook:" + event,
		summary: githubSummary(event, body),
		body:    body,
		raw:     raw,
		// GitHub sends a stable per-delivery uuid, and it is the same on
		// every retry and on a redelivery an operator triggers from the
		// provider UI. Without it every one of those woke the seat again.
		key:     req.Header.Get("X-GitHub-Delivery"),
		headers: safeHeaders(req.Header),
	}, statusOK)
}

func (r *Receiver) gitlab(w http.ResponseWriter, req *http.Request) {
	raw, ok := r.body(w, req)
	if !ok {
		return
	}
	event := headerOr(req, "X-Gitlab-Event", "unknown")
	if !r.serving(w, "gitlab", event) {
		return
	}
	// Standard-Webhooks signs {webhook-id}.{webhook-timestamp}.{body}, so
	// the scheme needs two more headers and the clock. Closed over rather
	// than widening the shared gate's signature for the one route that
	// needs them.
	now := r.now()
	standardWebhooks := func(body []byte, secret, signature string) bool {
		return verifyGitLab(body, secret,
			req.Header.Get("webhook-id"), req.Header.Get("webhook-timestamp"),
			signature, now)
	}
	// ONE SCHEME. A delivery without a valid signature is refused —
	// see decisions/702.
	//
	// There used to be a fallback to the plaintext X-Gitlab-Token, on the
	// measured premise that gitlab-ee 19.3.0 sent no `webhook-signature`
	// at all. The measurement was real and the conclusion was wrong: the
	// hook had been provisioned with GitLab's `token` attribute instead of
	// `signing_token`, so the instance was doing exactly as asked. GitLab
	// signs from 19.1 onward whenever a hook has a signing token, and this
	// engine's provisioner now sets one.
	//
	// So the fallback authenticated the sender with a bearer string
	// GitLab's own documentation calls weaker and not recommended, over a
	// payload it said nothing about — and it was reachable by an attacker
	// simply omitting the signature header, which is the shape a
	// downgrade attack takes. Its absence is the point: there is no path
	// here that verifies anything but an HMAC over the body.
	// A SECRET THAT CANNOT BE A KEY IS NOTHING TO VERIFY WITH.
	//
	// GitLab's signing token is whsec_<standard base64>, and it always keys
	// on the decoded bytes. A value shaped any other way cannot produce a
	// matching HMAC for any delivery, so treating it as a credential turns
	// "your secret is mistyped" into an endless run of signature
	// mismatches — 401s that read as an attack. Answered here rather than
	// inside the comparison because it is the 503 case by this package's
	// own rule: the sender's request is fine, and what is missing is on
	// this side.
	secret := r.secrets().GitLab
	if _, usable := gitlabKey(secret); secret != "" && !usable {
		log.Error("gitlab_signing_secret_malformed", "source", "gitlab",
			"detail", "integrations.gitlab.signing_secret is not a "+
				"whsec_<standard base64> value, so it cannot be the HMAC key "+
				"GitLab signs with and no delivery can ever verify. Re-run "+
				"`crewlet gitlab provision` or set the value GitLab was given")
		noSecret(w, "gitlab")
		return
	}
	v, ok := r.authenticate(w, "gitlab", secret,
		req.Header.Get("webhook-signature"), raw, scheme(standardWebhooks))
	if !ok {
		return
	}
	body, ok := parseBody(w, raw)
	if !ok {
		return
	}
	// GitLab 19.1+ sends webhook-id; older deliveries carry
	// X-Gitlab-Event-UUID. Either is a stable per-delivery identity, and a
	// deployment mid-upgrade sends both across its instances.
	key := headerOr(req, "X-Gitlab-Event-UUID", req.Header.Get("webhook-id"))
	r.accept(w, req, v, delivery{
		source:  "gitlab",
		label:   "webhook:" + event,
		summary: gitlabSummary(event, body),
		body:    body,
		raw:     raw,
		key:     key,
		headers: safeHeaders(req.Header),
	}, statusOK)
}

func (r *Receiver) jira(w http.ResponseWriter, req *http.Request) {
	raw, ok := r.body(w, req)
	if !ok {
		return
	}
	if !r.serving(w, "jira", "") {
		return
	}
	v, ok := r.authenticate(w, "jira", r.secrets().Jira,
		req.Header.Get("X-Hub-Signature"), raw, verifyAtlassian)
	if !ok {
		return
	}
	body, ok := parseBody(w, raw)
	if !ok {
		return
	}
	event := orElse(str(body, "webhookEvent"), "unknown")
	r.accept(w, req, v, delivery{
		source:  "jira",
		label:   "webhook:" + event,
		summary: jiraSummary(body),
		body:    body,
		raw:     raw,
		// Jira sends a stable per-delivery identifier on both
		// deployments, and it is the same across its own retries — which
		// is what makes it a dedupe key rather than a request id. A
		// Cloud event relayed through Forge carries none, and that route
		// answers for its own deliveries.
		key:     orElse(req.Header.Get("X-Atlassian-Webhook-Identifier"), bodyKey(raw)),
		headers: safeHeaders(req.Header),
	}, statusOK)
}

func (r *Receiver) confluence(w http.ResponseWriter, req *http.Request) {
	raw, ok := r.body(w, req)
	if !ok {
		return
	}
	if !r.serving(w, "confluence", "") {
		return
	}
	// Cloud is unaffected by this route's secret: those events arrive
	// through the Forge app on /webhooks/forge with its own JWT.
	v, ok := r.authenticate(w, "confluence", r.secrets().Confluence,
		req.Header.Get("X-Hub-Signature"), raw, verifyAtlassian)
	if !ok {
		return
	}
	body, ok := parseBody(w, raw)
	if !ok {
		return
	}
	event := orElse(firstOf(body, "event", "webhookEvent", "eventType"), "unknown")
	r.accept(w, req, v, delivery{
		source:  "confluence",
		label:   "webhook:" + event,
		summary: confluenceSummary(body),
		body:    body,
		raw:     raw,
		// THE SAME HEADER ITS JIRA TWIN USES. Data Center sends a stable
		// per-delivery identifier that does not change across its own
		// retries; an instance that sends none falls back to the payload,
		// which is what stays identical across a retry either way.
		key:     orElse(req.Header.Get("X-Atlassian-Webhook-Identifier"), bodyKey(raw)),
		headers: safeHeaders(req.Header),
	}, statusOK)
}

// slackOK is Slack's own success shape. Slack reads the body of a 200, and
// {"ok": true} is what its clients expect to find there.
var slackOK = map[string]bool{"ok": true}

func (r *Receiver) slack(w http.ResponseWriter, req *http.Request) {
	handle := req.PathValue("handle")
	raw, ok := r.body(w, req)
	if !ok {
		return
	}
	// PARSED BEFORE THE GATE, which the other routes deliberately no
	// longer do, and for two reasons that are both in the body: the
	// readiness answer is keyed on `type`, and the url_verification
	// handshake below is answered without a signature at all. The cost is
	// bounded by MaxBodyBytes and nothing is persisted by decoding it.
	body, ok := parseBody(w, raw)
	if !ok {
		return
	}
	if !r.serving(w, "slack", str(body, "type")) {
		return
	}

	// The URL-verification handshake, answered WITHOUT a signature check
	// and deliberately.
	//
	// It is what Slack sends the moment an app's request URL is set, which
	// during `crewlet slack provision` is before the operator has the
	// signing secret to put in config at all — so a verified handshake
	// cannot be completed and the app can never be installed. Answering it
	// is safe because the response is a pure echo of the caller's own
	// challenge: it persists nothing, publishes nothing, wakes nobody, and
	// tells an unauthenticated caller only what they already sent.
	if str(body, "type") == "url_verification" {
		writeJSON(w, http.StatusOK, map[string]string{"challenge": str(body, "challenge")})
		return
	}

	// PER SEAT, because Slack gives each agent its own app. An empty map is
	// "cannot verify", not "nothing to verify" — the distinction the Python
	// this replaces got wrong, where a deployment with no Slack secret
	// skipped the check entirely and this route answered 200 to an unsigned
	// POST addressed at any seat.
	secrets := r.secrets().Slack
	if len(secrets) == 0 {
		noSecret(w, "slack")
		return
	}
	secret, known := secrets[handle]
	if !known || secret == "" {
		// 401, not 503: the map is populated, so this is a delivery
		// addressed to a seat that has no Slack app rather than a node
		// with nothing to check against.
		log.Warn("slack_webhook_unknown_handle", "handle", handle)
		unauthorized(w, "unknown handle")
		return
	}
	timestamp := req.Header.Get("X-Slack-Request-Timestamp")
	now := r.now()
	v0 := func(body []byte, secret, signature string) bool {
		return verifySlack(body, secret, signature, timestamp, now)
	}
	v, ok := r.authenticate(w, "slack", secret, req.Header.Get("X-Slack-Signature"), raw, v0)
	if !ok {
		return
	}
	r.accept(w, req, v, delivery{
		source:  "slack",
		label:   "webhook:" + orElse(str(body, "type"), "unknown"),
		summary: slackSummary(handle, body),
		body:    body,
		raw:     raw,
		handle:  handle,
		// Slack's envelope carries an event_id that is stable across its
		// retries — a retry repeats it and sets X-Slack-Retry-Num — so
		// the same message cannot wake the seat twice.
		key:     str(body, "event_id"),
		headers: safeHeaders(req.Header),
	}, slackOK)
}
