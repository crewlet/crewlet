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

func TestPlaneSignatureIsBareHex(t *testing.T) {
	t.Parallel()
	// Plane sends the digest with no algorithm prefix. A verifier that
	// expected GitHub's shape would refuse every genuine delivery.
	e := newEdge(t)
	body := []byte(`{"event":"issue","action":"created"}`)
	if got := e.post(t, "/webhooks/plane", body, planeDelivery(body, "pl-secret")).Code; got != http.StatusOK {
		t.Fatalf("got %d", got)
	}
	// And a prefixed one is not a Plane signature.
	res := e.post(t, "/webhooks/plane", body, map[string]string{
		"X-Plane-Signature": "sha256=" + hexMAC("pl-secret", body),
	})
	if res.Code != http.StatusUnauthorized {
		t.Errorf("a prefixed digest got %d, want 401", res.Code)
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
			{"/webhooks/plane", "X-Plane-Signature"},
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

// gitlabDelivery signs a Standard-Webhooks request.
func gitlabDelivery(body []byte, secret, id string, at time.Time) map[string]string {
	ts := strconv.FormatInt(at.Unix(), 10)
	key := []byte(secret)
	if payload, found := strings.CutPrefix(secret, "whsec_"); found {
		if raw, err := base64.StdEncoding.DecodeString(payload); err == nil {
			key = raw
		}
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
		body, gitlabDelivery(body, "gl-secret", "msg_1", pinned)).Code; got != http.StatusOK {
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
			headers := gitlabDelivery(body, "gl-secret", "msg_1", pinned)
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
	headers := gitlabDelivery(body, "gl-secret", "msg_1", pinned)
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

func TestAWhsecSecretThatDoesNotDecodeIsUsedVerbatim(t *testing.T) {
	t.Parallel()
	// Refusing it outright would turn a mis-typed secret into a route that
	// rejects every delivery with no way to tell that from an attack.
	e := newEdge(t)
	e.secrets.GitLab = "whsec_not-base64!!"

	body := []byte(`{"object_kind":"issue"}`)
	if got := e.post(t, "/webhooks/gitlab",
		body, gitlabDelivery(body, "whsec_not-base64!!", "msg_1", pinned)).Code; got != http.StatusOK {
		t.Fatalf("got %d", got)
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
				gitlabDelivery(body, "gl-secret", "msg_"+tc.name, tc.at))
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
		"X-Hub-Signature-256": "sha256=" + hexMAC("pl-secret", body),
		"X-GitHub-Event":      "issues",
	})
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("a GitHub delivery signed with Plane's secret got %d, want 401", res.Code)
	}
}
