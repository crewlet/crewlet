package webhooks_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"time"

	"github.com/crewlet/crewlet/internal/gitlab"
)

// The scheme-by-scheme checks. They go through the ROUTES rather than through
// the verify functions directly, because the property under test is "this
// delivery is accepted / refused", and a unit test of the HMAC would pass
// happily while the handler read the wrong header.

func TestGitHubSignatureShape(t *testing.T) {
	t.Parallel()
	body := []byte(`{"action":"opened"}`)
	digest := hexMAC("gh-secret", body)
	cases := []struct {
		name      string
		signature string
		want      int
	}{
		{"the real thing", "sha256=" + digest, http.StatusOK},
		// Hex is case-insensitive as an encoding, and the digest is
		// decoded before it is compared — so a provider that spelled it
		// in uppercase is arithmetically correct and must be accepted. A
		// string comparison would refuse it.
		{"uppercase hex", "sha256=" + strings.ToUpper(digest), http.StatusOK},
		{"no prefix", digest, http.StatusUnauthorized},
		{"the wrong prefix", "sha1=" + digest, http.StatusUnauthorized},
		{"not hex at all", "sha256=" + strings.Repeat("z", 64), http.StatusUnauthorized},
		{"truncated", "sha256=" + digest[:32], http.StatusUnauthorized},
		{"empty", "", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEdge(t)
			res := e.post(t, "/webhooks/github", body, map[string]string{
				"X-Hub-Signature-256": tc.signature,
				"X-GitHub-Event":      "issues",
			})
			if res.Code != tc.want {
				t.Errorf("got %d, want %d", res.Code, tc.want)
			}
		})
	}
}

func TestASignatureOverADifferentBodyIsRefused(t *testing.T) {
	t.Parallel()
	// The one mistake a shape check cannot catch: a signature that is
	// perfectly well formed, made with the right key, over something else.
	e := newEdge(t)
	signed := []byte(`{"action":"opened"}`)
	swapped := []byte(`{"action":"closed"}`)
	res := e.post(t, "/webhooks/github", swapped, map[string]string{
		"X-Hub-Signature-256": "sha256=" + hexMAC("gh-secret", signed),
		"X-GitHub-Event":      "issues",
	})
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", res.Code)
	}
}

func TestAHeaderTheClientMadeUpCannotBecomeA500(t *testing.T) {
	t.Parallel()
	// Header values arrive as arbitrary bytes. A verifier that assumed a
	// shape would turn an unauthenticated request into a crash rather than
	// a 401 — which is a denial of service anyone can reach.
	e := newEdge(t)
	body := []byte(`{}`)
	for _, signature := range []string{
		"\xff\xfe", strings.Repeat("A", 10000), "sha256=", "v0=", "%%%",
		"sha256=" + strings.Repeat("0", 63), "sha256=" + strings.Repeat("0", 65),
	} {
		for _, route := range []struct{ path, header string }{
			{"/webhooks/github", "X-Hub-Signature-256"},
			{"/webhooks/jira", "X-Hub-Signature"},
		} {
			res := e.post(t, route.path, body, map[string]string{route.header: signature})
			if res.Code != http.StatusUnauthorized {
				t.Errorf("%s with signature %q got %d, want 401",
					route.path, signature, res.Code)
			}
		}
	}
}

// gitlabDelivery signs a Standard-Webhooks request the way GitLab does.
//
// NO FALLBACK for a secret that is not a whsec_ value: GitLab always keys on
// the decoded bytes, so a helper that quietly keyed on the printable string
// would agree with a lenient verifier and prove nothing about a correct one.
// A bad secret here is a bad fixture, and it says so.
func gitlabDelivery(body []byte, secret, id string, at time.Time) map[string]string {
	ts := strconv.FormatInt(at.Unix(), 10)
	payload, found := strings.CutPrefix(secret, "whsec_")
	if !found {
		panic("gitlabDelivery: " + secret + " is not a whsec_ secret")
	}
	key, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		panic("gitlabDelivery: " + secret + " is not standard base64")
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte(id + "." + ts + "."))
	h.Write(body)
	return map[string]string{
		"webhook-id":        id,
		"webhook-timestamp": ts,
		"webhook-signature": "v1," + base64.StdEncoding.EncodeToString(h.Sum(nil)),
		"X-Gitlab-Event":    "Merge Request Hook",
	}
}

func TestGitLabStandardWebhooks(t *testing.T) {
	t.Parallel()
	body := []byte(`{"object_kind":"merge_request","object_attributes":{"iid":3,"action":"open"}}`)
	e := newEdge(t)
	if got := e.post(t, "/webhooks/gitlab",
		body, gitlabDelivery(body, gitlabSecret, "msg_1", pinned)).Code; got != http.StatusOK {
		t.Fatalf("a genuine delivery got %d", got)
	}
}

func TestGitLabSignsTheIDAndTimestampToo(t *testing.T) {
	t.Parallel()
	// The signed string is {id}.{timestamp}.{body}, so changing either
	// header must break the signature. A verifier that signed only the body
	// would let a captured delivery be replayed under a fresh timestamp.
	body := []byte(`{"object_kind":"issue"}`)
	e := newEdge(t)
	for _, tamper := range []struct {
		name string
		with func(map[string]string)
	}{
		{"a different id", func(h map[string]string) { h["webhook-id"] = "msg_2" }},
		{"a fresher timestamp", func(h map[string]string) {
			h["webhook-timestamp"] = strconv.FormatInt(pinned.Add(time.Minute).Unix(), 10)
		}},
		{"no id", func(h map[string]string) { delete(h, "webhook-id") }},
		{"no timestamp", func(h map[string]string) { delete(h, "webhook-timestamp") }},
	} {
		t.Run(tamper.name, func(t *testing.T) {
			headers := gitlabDelivery(body, gitlabSecret, "msg_1", pinned)
			tamper.with(headers)
			if got := e.post(t, "/webhooks/gitlab", body, headers).Code; got != http.StatusUnauthorized {
				t.Errorf("got %d, want 401", got)
			}
		})
	}
}

func TestGitLabAcceptsAnyOfSeveralSignatures(t *testing.T) {
	t.Parallel()
	// Several tokens ride in one header during a key rotation — that is how
	// the scheme rotates without an outage. A verifier that read only the
	// first would break for the whole rotation window.
	e := newEdge(t)
	body := []byte(`{"object_kind":"issue"}`)
	headers := gitlabDelivery(body, gitlabSecret, "msg_1", pinned)
	headers["webhook-signature"] = "v1,c3RhbGU= " + headers["webhook-signature"]
	if got := e.post(t, "/webhooks/gitlab", body, headers).Code; got != http.StatusOK {
		t.Fatalf("got %d, want 200", got)
	}
}

func TestGitLabDecodesAWhsecSecret(t *testing.T) {
	t.Parallel()
	// The prefix marks a base64 payload, and the KEY is those decoded
	// bytes, not the printable form. Keying on the text produces a
	// signature that never matches, on every delivery, with no error
	// anywhere to say why.
	raw := []byte("thirty-two-bytes-of-signing-key!")
	secret := "whsec_" + base64.StdEncoding.EncodeToString(raw)
	e := newEdge(t)
	e.secrets.GitLab = secret

	body := []byte(`{"object_kind":"issue"}`)
	if got := e.post(t, "/webhooks/gitlab",
		body, gitlabDelivery(body, secret, "msg_1", pinned)).Code; got != http.StatusOK {
		t.Fatalf("got %d — the whsec_ payload was not decoded", got)
	}
}

// A SECRET THAT IS NOT A whsec_ VALUE IS NOT A SECRET.
//
// This inverts a test that asserted the opposite. The old verifier keyed on
// the printable string when the payload would not decode, so a mistyped
// secret produced an HMAC that cannot match anything GitLab computes — and
// the operator got an endless stream of signature mismatches,
// indistinguishable from an attack, with nothing naming the encoding.
//
// 503 rather than 401 is the point: the sender's request was fine, and what
// is wrong is on this side. That is the same answer a route with no secret
// at all gives, because it is the same situation — this node cannot verify.
func TestAMalformedSigningSecretIsReportedNotUsedVerbatim(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{
		"whsec_not-base64!!",      // carries the prefix, will not decode
		"a-plain-string",          // no prefix at all
		"whsec_" + "dXJsLXNhZmU_", // URL-safe alphabet, which GitLab never emits
	} {
		e := newEdge(t)
		e.secrets.GitLab = bad

		got := e.post(t, "/webhooks/gitlab", []byte(`{"object_kind":"issue"}`),
			map[string]string{
				"webhook-id":        "msg_1",
				"webhook-timestamp": strconv.FormatInt(pinned.Unix(), 10),
				"webhook-signature": "v1,bm90LWEtc2lnbmF0dXJl",
				"X-Gitlab-Event":    "Issue Hook",
			}).Code
		if got != http.StatusServiceUnavailable {
			t.Errorf("secret %q got %d, want 503 — this node cannot verify, "+
				"which is not the sender's fault", bad, got)
		}
	}
}

func TestASignedTimestampBoundsTheReplay(t *testing.T) {
	t.Parallel()
	// Without the window a captured delivery replays forever, correctly
	// signed. A FUTURE stamp is as suspect as an old one: it is what a
	// replay looks like against a node whose clock ran slow.
	body := []byte(`{"type":"event_callback","event_id":"E1","event":{"type":"message"}}`)
	cases := []struct {
		name string
		at   time.Time
		want int
	}{
		{"now", pinned, http.StatusOK},
		{"four minutes old", pinned.Add(-4 * time.Minute), http.StatusOK},
		{"six minutes old", pinned.Add(-6 * time.Minute), http.StatusUnauthorized},
		{"six minutes in the future", pinned.Add(6 * time.Minute), http.StatusUnauthorized},
		{"a day old", pinned.Add(-24 * time.Hour), http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run("slack/"+tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEdge(t)
			res := e.post(t, "/webhooks/slack/ceo", body, slackDelivery(body, "slack-secret", tc.at))
			if res.Code != tc.want {
				t.Errorf("got %d, want %d", res.Code, tc.want)
			}
		})
		t.Run("gitlab/"+tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEdge(t)
			res := e.post(t, "/webhooks/gitlab", body,
				gitlabDelivery(body, gitlabSecret, "msg_"+tc.name, tc.at))
			if res.Code != tc.want {
				t.Errorf("got %d, want %d", res.Code, tc.want)
			}
		})
	}
}

func TestASlackTimestampThatIsNotANumberIsRefused(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	body := []byte(`{"type":"event_callback"}`)
	headers := slackDelivery(body, "slack-secret", pinned)
	// Re-signed with the bad stamp, so the only thing wrong is the stamp.
	headers["X-Slack-Request-Timestamp"] = "not-a-number"
	signed := append([]byte("v0:not-a-number:"), body...)
	headers["X-Slack-Signature"] = "v0=" + hexMAC("slack-secret", signed)

	if got := e.post(t, "/webhooks/slack/ceo", body, headers).Code; got != http.StatusUnauthorized {
		t.Errorf("got %d, want 401 — an unparseable stamp bounds nothing", got)
	}
}

func TestJiraAndConfluenceAreOneScheme(t *testing.T) {
	t.Parallel()
	// Two copies of one signature check is how they come to disagree, and
	// the disagreement is silent because each half stays self-consistent.
	e := newEdge(t)
	body := []byte(`{"webhookEvent":"jira:issue_created","issue":{"key":"OPS-1"}}`)
	if got := e.post(t, "/webhooks/jira", body, atlassianDelivery(body, "jira-secret")).Code; got != http.StatusOK {
		t.Errorf("jira got %d", got)
	}
	page := []byte(`{"event":"page_updated","page":{"title":"Runbook"}}`)
	if got := e.post(t, "/webhooks/confluence", page, atlassianDelivery(page, "conf-secret")).Code; got != http.StatusOK {
		t.Errorf("confluence got %d", got)
	}
	// Each has its OWN secret: one signed with the other's must not pass.
	if got := e.post(t, "/webhooks/confluence", page, atlassianDelivery(page, "jira-secret")).Code; got != http.StatusUnauthorized {
		t.Errorf("a Jira-signed Confluence delivery got %d, want 401", got)
	}
}

func TestEachRouteUsesItsOwnSecret(t *testing.T) {
	t.Parallel()
	// A single shared secret across the routes would mean anyone holding
	// one integration's credential could forge every other integration's
	// deliveries.
	e := newEdge(t)
	body := []byte(`{"action":"opened"}`)
	res := e.post(t, "/webhooks/github", body, map[string]string{
		"X-Hub-Signature-256": "sha256=" + hexMAC("jira-secret", body),
		"X-GitHub-Event":      "issues",
	})
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("a GitHub delivery signed with Jira's secret got %d, want 401", res.Code)
	}
}

// WHAT THE PROVISIONER MINTS IS WHAT THIS ROUTE ACCEPTS.
//
// The minter and the verifier live in different packages and agree on one
// thing that is never written down between them: the `whsec_` payload is
// STANDARD base64, and the HMAC key is the decoded bytes.
//
// Getting it wrong is silent in both directions. A URL-safe payload usually
// fails a standard decode, and this route's fallback then keys on the
// printable string verbatim — deliberately, so a hand-written secret still
// works — while GitLab keys on the decoded bytes. Every delivery is refused
// with a signature mismatch and nothing names the encoding. Measured against
// a real instance: the hook fired, and the only trace was one
// `webhook_signature_invalid` line.
func TestAMintedSecretVerifiesOnThisRoute(t *testing.T) {
	t.Parallel()
	secret, err := gitlab.MintSigningSecret()
	if err != nil {
		t.Fatalf("MintSigningSecret: %v", err)
	}
	e := newEdge(t)
	e.secrets.GitLab = secret

	// SIGNED THE WAY GITLAB SIGNS, not the way this package reads. The
	// ordinary helper derives its key exactly as the verifier does — with
	// the same verbatim fallback — so it agrees with any encoding and
	// would prove only that this repository is self-consistent. GitLab
	// has no fallback: `whsec_` means standard base64 and the key is the
	// decoded bytes, full stop.
	body := []byte(`{"object_kind":"issue"}`)
	if got := e.post(t, "/webhooks/gitlab",
		body, vendorSignedDelivery(t, body, secret, "msg_1", pinned)).Code; got != http.StatusOK {
		t.Fatalf("got %d — a secret this repository's own provisioner minted "+
			"does not verify against a vendor-shaped signature", got)
	}
}

// vendorSignedDelivery signs a delivery the way GitLab does: the key is the
// `whsec_` payload decoded as STANDARD base64, and a payload that does not
// decode that way is an error rather than a fallback.
func vendorSignedDelivery(t *testing.T, body []byte, secret, id string, at time.Time) map[string]string {
	t.Helper()
	payload, found := strings.CutPrefix(secret, "whsec_")
	if !found {
		t.Fatalf("secret %q carries no whsec_ prefix, so GitLab would key on "+
			"it verbatim and the scheme's own encoding is not being tested", secret)
	}
	key, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("the whsec_ payload is not standard base64 (%v), so GitLab "+
			"cannot derive the key this route expects", err)
	}
	ts := strconv.FormatInt(at.Unix(), 10)
	h := hmac.New(sha256.New, key)
	h.Write([]byte(id + "." + ts + "."))
	h.Write(body)
	return map[string]string{
		"webhook-id":        id,
		"webhook-timestamp": ts,
		"webhook-signature": "v1," + base64.StdEncoding.EncodeToString(h.Sum(nil)),
		"X-Gitlab-Event":    "Issue Hook",
	}
}

// --- the one GitLab scheme -------------------------------------------------

// A GITLAB DELIVERY IS AUTHENTICATED BY ITS SIGNATURE, AND BY NOTHING ELSE.
//
// GitLab takes two different secrets on a hook: `signing_token`, an HMAC key
// it signs every delivery with, and `token`, a bearer string it echoes back
// in plaintext as X-Gitlab-Token — which GitLab's own documentation calls
// weaker and not recommended. This engine provisions the first and accepts
// only the first.
//
// There was a fallback to the second, added on a measurement that a live
// 19.3.0 instance sent no `webhook-signature`. The measurement was real and
// the conclusion was wrong: the hook had been provisioned with `token`, so
// GitLab was doing as asked.
func TestGitLabAcceptsASignedDelivery(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	e.secrets.GitLab = gitlabSecret

	body := []byte(`{"object_kind":"issue"}`)
	id, ts := "c32719e0", strconv.FormatInt(pinned.Unix(), 10)
	got := e.post(t, "/webhooks/gitlab", body, map[string]string{
		"webhook-id":          id,
		"webhook-timestamp":   ts,
		"webhook-signature":   gitlabSignature(t, gitlabSecret, id, ts, body),
		"X-Gitlab-Event":      "Issue Hook",
		"X-Gitlab-Event-UUID": "243c86f4",
	}).Code
	if got != http.StatusOK {
		t.Fatalf("got %d — a correctly signed delivery was refused", got)
	}
}

// THE PLAINTEXT TOKEN IS NOT A CREDENTIAL HERE ANY MORE.
//
// This is the inversion of a test that used to assert the opposite. An
// unsigned delivery presenting the right token in X-Gitlab-Token was the
// engine's accepted path; it is now refused, because a bearer string says
// nothing about the payload it arrived with and omitting the signature
// header was all it took to reach it.
func TestGitLabRefusesAnUnsignedDeliveryEvenWithTheRightToken(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	e.secrets.GitLab = gitlabSecret

	got := e.post(t, "/webhooks/gitlab", []byte(`{"object_kind":"issue"}`), map[string]string{
		// Exactly what a hook provisioned the OLD way sends.
		"webhook-id":          "c32719e0",
		"webhook-timestamp":   strconv.FormatInt(pinned.Unix(), 10),
		"X-Gitlab-Token":      gitlabSecret,
		"X-Gitlab-Event":      "Issue Hook",
		"X-Gitlab-Event-UUID": "243c86f4",
	}).Code
	if got != http.StatusUnauthorized {
		t.Fatalf("got %d — the plaintext token still authenticates a delivery", got)
	}
}

// AND NO CREDENTIAL AT ALL IS NOT A WAY IN. The shape an attacker sends first.
func TestGitLabRefusesADeliveryWithNoSignature(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	e.secrets.GitLab = gitlabSecret

	got := e.post(t, "/webhooks/gitlab", []byte(`{"object_kind":"issue"}`), map[string]string{
		"webhook-id":        "c32719e0",
		"webhook-timestamp": strconv.FormatInt(pinned.Unix(), 10),
		"X-Gitlab-Event":    "Issue Hook",
	}).Code
	if got != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", got)
	}
}

// A WRONG SIGNATURE IS REFUSED, and cannot fall through to anything.
func TestGitLabRefusesABadSignature(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	e.secrets.GitLab = gitlabSecret

	got := e.post(t, "/webhooks/gitlab", []byte(`{"object_kind":"issue"}`), map[string]string{
		"webhook-id":        "c32719e0",
		"webhook-timestamp": strconv.FormatInt(pinned.Unix(), 10),
		"webhook-signature": "v1,bm90LWEtc2lnbmF0dXJl",
		// Presented as well, so the test proves there is nothing to fall
		// through TO rather than that the fall-through was not reached.
		"X-Gitlab-Token": gitlabSecret,
		"X-Gitlab-Event": "Issue Hook",
	}).Code
	if got != http.StatusUnauthorized {
		t.Fatalf("got %d — a bad signature was accepted", got)
	}
}

// THE BODY IS WHAT IS SIGNED. A signature valid for one payload must not
// authenticate another: that is the entire difference between this scheme
// and the bearer token it replaced.
func TestGitLabRefusesASignatureFromAnotherBody(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	e.secrets.GitLab = gitlabSecret

	id, ts := "c32719e0", strconv.FormatInt(pinned.Unix(), 10)
	signed := gitlabSignature(t, gitlabSecret, id, ts, []byte(`{"object_kind":"issue"}`))
	got := e.post(t, "/webhooks/gitlab", []byte(`{"object_kind":"merge_request"}`), map[string]string{
		"webhook-id":        id,
		"webhook-timestamp": ts,
		"webhook-signature": signed,
		"X-Gitlab-Event":    "Issue Hook",
	}).Code
	if got != http.StatusUnauthorized {
		t.Fatalf("got %d — a signature over a different body was accepted", got)
	}
}

// SEVERAL SIGNATURES CAN RIDE ONE HEADER, which is how the scheme rotates a
// key without an outage. GitLab sends one today and its documentation says
// that may change, so any entry matching is a match.
func TestGitLabAcceptsOneOfSeveralSignatures(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	e.secrets.GitLab = gitlabSecret

	body := []byte(`{"object_kind":"issue"}`)
	id, ts := "c32719e0", strconv.FormatInt(pinned.Unix(), 10)
	real := gitlabSignature(t, gitlabSecret, id, ts, body)
	got := e.post(t, "/webhooks/gitlab", body, map[string]string{
		"webhook-id":        id,
		"webhook-timestamp": ts,
		"webhook-signature": "v1,bm90LWEtc2lnbmF0dXJl " + real,
		"X-Gitlab-Event":    "Issue Hook",
	}).Code
	if got != http.StatusOK {
		t.Fatalf("got %d — a valid signature beside a stale one was refused", got)
	}
}

// gitlabSecret is a REAL whsec_ value: the prefix over standard base64 of a
// 32-byte key, which is the only shape GitLab's API accepts. A fixture that
// merely looks the part passes a lax verifier and fails a correct one.
const gitlabSecret = "whsec_TZKyEaPXhi0xZl3mrSf9DdHgcjMC+EWPsBVilfjdgOI="

// gitlabSignature signs exactly as GitLab documents it: HMAC-SHA256 over
// "{webhook-id}.{webhook-timestamp}.{body}", keyed on the DECODED payload of
// the whsec_ secret, rendered as "v1,<standard base64>".
//
// Built here from the published algorithm rather than by calling the
// engine's own verifier inside out — a fixture generated by the code under
// test agrees with it by construction, including where both are wrong.
func gitlabSignature(t *testing.T, secret, id, timestamp string, body []byte) string {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		t.Fatalf("the fixture secret is not whsec_<standard base64>: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("the fixture key is %d bytes, and GitLab requires 32", len(key))
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + timestamp + "."))
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// THE SIGNATURE'S VERSION IS PART OF ITS FORMAT.
//
// GitLab sends "v1,<base64>". A future v2 will be a different construction —
// a different message, a different hash, or both — so trying its bytes
// against a v1 computation is not a fallback, it is comparing two unrelated
// values and hoping one collides.
//
// The entry is IGNORED rather than the delivery refused: ignoring what this
// code cannot evaluate is exactly what lets several signatures ride one
// header, so a v2 arriving beside a v1 keeps working through a rotation.
func TestGitLabIgnoresASignatureVersionItCannotEvaluate(t *testing.T) {
	t.Parallel()
	body := []byte(`{"object_kind":"issue"}`)
	id, ts := "msg_1", strconv.FormatInt(pinned.Unix(), 10)
	v1 := gitlabSignature(t, gitlabSecret, id, ts, body)
	// The same bytes under a version this build does not know.
	v2 := "v2," + strings.TrimPrefix(v1, "v1,")

	t.Run("a v2 entry alone is not accepted", func(t *testing.T) {
		e := newEdge(t)
		e.secrets.GitLab = gitlabSecret
		got := e.post(t, "/webhooks/gitlab", body, map[string]string{
			"webhook-id": id, "webhook-timestamp": ts,
			"webhook-signature": v2, "X-Gitlab-Event": "Issue Hook",
		}).Code
		if got != http.StatusUnauthorized {
			t.Fatalf("got %d — a v2 signature was verified as though it were "+
				"a v1, which is a comparison of two unrelated values", got)
		}
	})

	t.Run("a v2 beside a valid v1 still passes", func(t *testing.T) {
		e := newEdge(t)
		e.secrets.GitLab = gitlabSecret
		got := e.post(t, "/webhooks/gitlab", body, map[string]string{
			"webhook-id": id, "webhook-timestamp": ts,
			"webhook-signature": v2 + " " + v1, "X-Gitlab-Event": "Issue Hook",
		}).Code
		if got != http.StatusOK {
			t.Fatalf("got %d — an unknown version beside a valid signature "+
				"broke the rotation window it exists to keep open", got)
		}
	})
}
