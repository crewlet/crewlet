package webhooks

import (
	"net/http"
)

// The seven endpoints. Each one is the same five steps in the same order —
// read, answer readiness, parse, authenticate, accept — and each differs only
// in the credential it checks and how it names the event. They are written out
// rather than driven from a table because what varies between them is the part
// worth reading, and a table would bury it in optional hooks.

func (r *Receiver) github(w http.ResponseWriter, req *http.Request) {
	raw, ok := r.body(w, req)
	if !ok {
		return
	}
	event := headerOr(req, "X-GitHub-Event", "unknown")
	if !r.serving(w, "github", event) {
		return
	}
	body, ok := parseBody(w, raw)
	if !ok {
		return
	}
	v, ok := r.authenticate(w, "github", r.secrets().GitHub,
		req.Header.Get("X-Hub-Signature-256"), raw, verifyGitHub)
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
	body, ok := parseBody(w, raw)
	if !ok {
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
	// TWO SCHEMES, IN ORDER — see rewrite/decisions/702.
	//
	// A signed delivery must have a VALID signature: that check runs
	// whenever the header is present, so stripping it is not a way down
	// to the weaker path. Only a delivery GitLab sent unsigned is
	// verified by its token.
	//
	// Measured, because the doc's premise was wrong: gitlab-ee 19.3.0
	// sends `webhook-id` and `webhook-timestamp` — the Standard-Webhooks
	// envelope — and no `webhook-signature`, with the feature flag on or
	// off. Requiring the signature meant the integration received
	// nothing at all, from a hook GitLab's own settings page called
	// healthy.
	signature := req.Header.Get("webhook-signature")
	check, presented := scheme(standardWebhooks), signature
	if signature == "" {
		check, presented = gitlabToken, req.Header.Get("X-Gitlab-Token")
	}
	v, ok := r.authenticate(w, "gitlab", r.secrets().GitLab, presented, raw, check)
	if !ok {
		return
	}
	noteGitLabScheme(signature != "")
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

func (r *Receiver) plane(w http.ResponseWriter, req *http.Request) {
	raw, ok := r.body(w, req)
	if !ok {
		return
	}
	if !r.serving(w, "plane", req.Header.Get("X-Plane-Event")) {
		return
	}
	body, ok := parseBody(w, raw)
	if !ok {
		return
	}
	v, ok := r.authenticate(w, "plane", r.secrets().Plane,
		req.Header.Get("X-Plane-Signature"), raw, verifyPlane)
	if !ok {
		return
	}
	event := headerOr(req, "X-Plane-Event", orElse(str(body, "event"), "unknown"))
	// The action rides on the dashboard's event type so create, update and
	// delete stay distinguishable in the feed's filter — Plane sends all
	// three under one event name.
	label := "webhook:" + event
	if action := str(body, "action"); action != "" {
		label += "." + action
	}
	r.accept(w, req, v, delivery{
		source:  "plane",
		label:   label,
		summary: planeSummary(body),
		body:    body,
		raw:     raw,
		// No delivery id: Plane sends none, and what counts as "the same
		// delivery" here is payload coordinates the transport derives
		// with the routing context that makes them correct.
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
	body, ok := parseBody(w, raw)
	if !ok {
		return
	}
	v, ok := r.authenticate(w, "jira", r.secrets().Jira,
		req.Header.Get("X-Hub-Signature"), raw, verifyAtlassian)
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
	body, ok := parseBody(w, raw)
	if !ok {
		return
	}
	// Cloud is unaffected by this route's secret: those events arrive
	// through the Forge app on /webhooks/forge with its own JWT.
	v, ok := r.authenticate(w, "confluence", r.secrets().Confluence,
		req.Header.Get("X-Hub-Signature"), raw, verifyAtlassian)
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
