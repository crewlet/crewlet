package webhooks

import (
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// The six endpoints. Each is the same five steps — read, answer readiness,
// AUTHENTICATE, parse, accept — differing in credential and event name. Written
// out, not table-driven: what varies is the part worth reading.
//
// # Nothing unverified is decoded
//
// The raw body is buffered before the gate (the signature is over it), bounded
// by MaxBodyBytes. Parsing first would hand an unauthenticated caller a JSON
// unmarshal and a map several times the size of what they sent.
//
// Two parse first and must: Slack reads `type` for readiness and the
// unsigned url_verification handshake, Forge reads `eventType` for readiness.

func (r *Receiver) github(w http.ResponseWriter, req *http.Request) {
	// PER SEAT WHEN ADDRESSED TO ONE. GitHub delivers to every app
	// installed on a repository, each delivery carrying its own id, so a
	// repository five agents work produces five deliveries of one
	// comment. Those are not duplicates to collapse — they are five
	// agents being told, which is the point of each holding its own app —
	// and without the handle nothing downstream can tell them apart from
	// a redelivery of one.
	//
	// Empty for a single app serving a whole organisation, which names no
	// seat and must still be accepted.
	handle := req.PathValue("handle")

	raw, ok := r.body(w, req)
	if !ok {
		return
	}
	event := headerOr(req, "X-GitHub-Event", "unknown")
	if !r.serving(w, "github", event) {
		return
	}
	v, ok := r.authenticate(w, "github", githubSecret(r.secrets(), handle),
		req.Header.Get("X-Hub-Signature-256"), raw, signed(verifyGitHub))
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
		handle:  handle,
		headers: safeHeaders(req.Header),
	}, statusOK)
}

// githubSecret is what a delivery to this path could have been signed with.
//
// THE SEAT'S OWN APP FIRST: an agent's app has its own signing secret.
// Checking its deliveries against `integrations.github.webhook_secret` uses a
// DIFFERENT app's key, or nothing in a company of per-agent apps: every
// delivery 503s while GitHub's hook page shows the app healthy.
//
// THE ORGANIZATION'S OTHERWISE: an organization-wide app pointed at a seat has
// only the organization's secret.
//
// Empty when neither is set: [Receiver.authenticate] answers 503, never
// "nothing to verify".
func githubSecret(s Secrets, handle string) string {
	if handle != "" {
		if secret := s.GitHubSeat[handle]; secret != "" {
			return secret
		}
	}
	return s.GitHub
}

// datadog is the one route whose credential is not a signature.
//
// Datadog's Webhooks integration can attach headers, but only with fixed
// values, so there is no HMAC to check and the shared token IS the
// authentication. Everything else about the route is identical to its
// neighbours, deliberately: the same five steps, the same 503 when there is
// nothing to check against, the same claim before the wake.
func (r *Receiver) datadog(w http.ResponseWriter, req *http.Request) {
	raw, ok := r.body(w, req)
	if !ok {
		return
	}
	event := headerOr(req, "X-Datadog-Event", "monitor")
	if !r.serving(w, "datadog", event) {
		return
	}
	v, ok := r.authenticate(w, "datadog", r.secrets().Datadog,
		req.Header.Get("X-Crewlet-Token"), raw, sharedToken)
	if !ok {
		return
	}
	body, ok := parseBody(w, raw)
	if !ok {
		return
	}
	r.accept(w, req, v, delivery{
		source:  "datadog",
		label:   "webhook:" + event,
		summary: datadogSummary(body),
		body:    body,
		raw:     raw,
		// Datadog stamps a notification id that survives its own
		// retries. Without one there is nothing stable to claim on, and
		// delivering twice beats dropping a firing monitor.
		//
		// THROUGH num, which accepts the NUMBER an unquoted `$ID` arrives
		// as. `str` answered "" for it, so a template an operator wrote
		// without quotes — the same slip [datadog.str] tolerates for
		// every other field — silently turned retry suppression off and
		// woke a seat once per delivery attempt.
		key:     num(body, "id"),
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
	// ONE SCHEME: a delivery without a valid signature is refused, with NO
	// FALLBACK TO THE PLAINTEXT X-Gitlab-Token. A delivery with no
	// `webhook-signature` comes from a hook given its key as `token` rather
	// than `signing_token`; GitLab signs from 19.1 whenever a hook has one,
	// and this engine's provisioner sets it. A fallback would accept a weaker
	// bearer string and be reachable by omitting the signature header: a
	// downgrade attack.
	//
	// A SECRET THAT CANNOT BE A KEY IS NOTHING TO VERIFY WITH. The signing
	// token is whsec_<standard base64>, keyed on the decoded bytes; any other
	// shape matches no delivery, turning a mistyped secret into 401s that read
	// as an attack. Answered as the 503 case: what is missing is on this side.
	secret := r.secrets().GitLab
	if _, usable := gitlabKey(secret); secret != "" && !usable {
		log.Error("gitlab_signing_secret_malformed", "source", "gitlab",
			"detail", "integrations.gitlab.signing_secret is not a "+
				"whsec_<standard base64> value, so it cannot be the HMAC key "+
				"GitLab signs with and no delivery can ever verify. Re-run "+
				"`crewlet gitlab provision` or set the value GitLab was given")
		unusableSecret(w)
		return
	}
	v, ok := r.authenticate(w, "gitlab", secret,
		req.Header.Get("webhook-signature"), raw, signed(standardWebhooks))
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
		req.Header.Get("X-Hub-Signature"), raw, signed(verifyAtlassian))
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
	// THE DATA CENTER ROUTE, which signs. Cloud arrives on
	// [Receiver.confluenceCloud] below, or through the Forge relay; a
	// Cloud delivery posted here carries no signature and is refused,
	// which is correct rather than a gap.
	v, ok := r.authenticate(w, "confluence", r.secrets().Confluence,
		req.Header.Get("X-Hub-Signature"), raw, signed(verifyAtlassian))
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
	// for an app being created is before its signing secret is sealed
	// anywhere, so a verified handshake cannot be completed and the app can
	// never be installed. Answering it
	// is safe because the response is a pure echo of the caller's own
	// challenge: it persists nothing, publishes nothing, wakes nobody, and
	// tells an unauthenticated caller only what they already sent.
	if str(body, "type") == "url_verification" {
		accepted(w, map[string]string{"challenge": str(body, "challenge")})
		return
	}

	// PER SEAT, because Slack gives each agent its own app. An empty map is
	// "cannot verify", not "nothing to verify". Getting that distinction
	// wrong means a deployment with no Slack secret skips the check
	// entirely and this route answers 200 to an unsigned POST addressed at
	// any seat.
	secrets := r.secrets().Slack
	if len(secrets) == 0 {
		noSecret(w, "slack")
		return
	}
	timestamp := req.Header.Get("X-Slack-Request-Timestamp")
	signature := req.Header.Get("X-Slack-Signature")
	now := r.now()
	check := signed(func(body []byte, key, presented string) bool {
		return verifySlack(body, key, presented, timestamp, now)
	})
	secret, known := secrets[handle]
	if !known || secret == "" {
		// A SEAT WITH NO APP IS ANSWERED EXACTLY AS A SIGNATURE THAT DID
		// NOT MATCH — the same 401, the same code, the same HMAC paid first.
		// A code of its own, or a skipped HMAC showing in timing, would be a
		// roster of which handles have a Slack app. So the decoy is checked
		// as a known seat's key is (an empty signature pays nothing on
		// either) and its answer DISCARDED: no delivery to a seat with no
		// app is accepted. The log tells an operator which check failed.
		_ = signature != "" && check.verify(raw, slackDecoy(), signature)
		log.Warn("slack_webhook_unknown_handle", "handle", handle,
			"detail", "no seat by this handle has a Slack signing secret here; "+
				"refused as an invalid signature so the answer does not say which "+
				"handles have an app")
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidSignature)
		return
	}
	v, ok := r.authenticate(w, "slack", secret, signature, raw, check)
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

// confluenceCloud is the route Confluence CLOUD delivers to, the second whose
// credential is not a signature: a token in the query, compared constant-time.
// A shared route with the HMAC one would have two secrets and no way to say
// which a 503 was about.
//
// A Cloud payload names no event, so one hook is registered per event with it
// in the path, and the route stamps it onto the body under the key the parser
// reads: no Cloud branch in the parser.
//
// The query string is NEVER LOGGED: the token rides in the URL, so log lines
// and event rows are built from the path and the body, never the whole URL.
func (r *Receiver) confluenceCloud(w http.ResponseWriter, req *http.Request) {
	raw, ok := r.body(w, req)
	if !ok {
		return
	}
	event := strings.TrimSpace(req.PathValue("event"))
	if !r.serving(w, "confluence", event) {
		return
	}
	// THE TOKEN ARRIVES HOWEVER THE SENDER CAN CARRY IT. The undocumented
	// admin hook can only put it in the URL, so the query is read. A
	// Confluence Automation "Send web request" rule, which IS documented
	// and supported, can set a header, so the same header Datadog uses is
	// read too. One route, one credential, two senders; the header wins
	// when both are present so a rule that set it is never judged by a
	// stale query.
	token := req.Header.Get("X-Crewlet-Token")
	if token == "" {
		token = req.URL.Query().Get("token")
	}
	v, ok := r.authenticate(w, "confluence", r.secrets().ConfluenceToken,
		token, raw, sharedToken)
	if !ok {
		return
	}
	body, ok := parseBody(w, raw)
	if !ok {
		return
	}
	// STAMPED, never trusted from the body: a Cloud payload has no event
	// field, and a Data Center one posted here by mistake would otherwise
	// name its own. The registered path is the only thing that knows.
	body["event"] = event
	r.accept(w, req, v, delivery{
		source:  "confluence",
		label:   "webhook:" + event,
		summary: confluenceSummary(body),
		body:    body,
		raw:     raw,
		// Cloud sends no per-delivery identifier. The payload carries the
		// content id, its version and a timestamp, and the raw body is
		// what stays identical across a retry, so it is claimed on that
		// as the Data Center fallback already is.
		key:     bodyKey(raw),
		headers: safeHeaders(req.Header),
	}, statusOK)
}
