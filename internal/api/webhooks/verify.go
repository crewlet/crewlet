package webhooks

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/whsec"
)

// The provider signature schemes. THREE schemes in four functions, over the
// five routes that verify a shared secret: GitHub's prefixed hex, Atlassian's
// identically-shaped X-Hub-Signature (verifyAtlassian delegates; Jira and
// Confluence share it), Slack's v0 basestring, and GitLab's Standard-Webhooks
// base64. The Forge relay verifies an invocation JWT instead (forge.go).
//
// Every one decodes the presented signature and compares BYTES with
// hmac.Equal. Comparing the hex or base64 TEXT would be case- and
// padding-sensitive (an uppercase digest would fail an arithmetically correct
// signature) and Go's text comparison is not constant-time.

// replayWindow bounds how far a signed request's own timestamp may be from
// now, for the two schemes that carry one.
//
// Five minutes because that is what both providers specify: Slack documents it
// as the window it signs against, and GitLab's Standard-Webhooks
// implementation uses the same. Widening it would accept an older captured
// request than the provider itself considers valid; narrowing it would reject
// genuine deliveries on a node whose clock has drifted by less than the
// provider tolerates.
const replayWindow = 5 * time.Minute

// verifyGitHub checks X-Hub-Signature-256: "sha256=" + hex(HMAC-SHA256(body)).
func verifyGitHub(body []byte, secret, signature string) bool {
	digest, ok := strings.CutPrefix(signature, "sha256=")
	if !ok {
		return false
	}
	return equalHex(digest, mac(secret, body))
}

// verifyAtlassian checks X-Hub-Signature, which Jira and Confluence Data
// Center both send in GitHub's shape.
//
// ONE function for the two, because it is one scheme. Two copies of a
// signature check is how they come to disagree — and the disagreement is
// silent, because each half stays self-consistent.
func verifyAtlassian(body []byte, secret, signature string) bool {
	return verifyGitHub(body, secret, signature)
}

// verifySlack checks Slack's v0 scheme over "v0:{timestamp}:{body}".
//
// The timestamp is part of the signed string AND is checked against the replay
// window, which is what makes a captured request stop working: without the
// window a recorded delivery replays forever, correctly signed.
func verifySlack(body []byte, secret, signature, timestamp string, now time.Time) bool {
	digest, ok := strings.CutPrefix(signature, "v0=")
	if !ok {
		return false
	}
	if !withinWindow(timestamp, now) {
		return false
	}
	signed := append([]byte("v0:"+timestamp+":"), body...)
	return equalHex(digest, mac(secret, signed))
}

// slackDecoy is the key a delivery to a seat with no Slack app is checked
// against, so the refusal costs what a wrong signature on a real seat costs.
//
// RANDOM PER PROCESS, and never a constant in the source, although the answer
// it produces is discarded either way: this repository is public, and a
// published key that a refactor one day stopped discarding would be a key
// anybody could sign an accepted delivery with.
//
//nolint:gochecknoglobals // a value computed once, not state
var slackDecoy = sync.OnceValue(rand.Text)

// verifyGitLab checks a GitLab 19.1+ Standard-Webhooks signature.
//
// GitLab signs "{webhook-id}.{webhook-timestamp}.{body}" with HMAC-SHA256
// keyed on the DECODED payload of a "whsec_…" secret, and sends the base64
// signature in webhook-signature as space-separated "v1,<sig>" tokens. Several
// tokens can be present at once — that is how the scheme rotates a key without
// an outage — so any one of them matching is a match.
func verifyGitLab(body []byte, secret, id, timestamp, signature string, now time.Time) bool {
	if id == "" || timestamp == "" || signature == "" {
		return false
	}
	if !withinWindow(timestamp, now) {
		return false
	}
	key, ok := gitlabKey(secret)
	if !ok {
		return false
	}
	signed := append([]byte(id+"."+timestamp+"."), body...)
	want := macKey(key, signed)
	for token := range strings.FieldsSeq(signature) {
		// "v1,<base64>", and the version is CHECKED rather than skipped.
		// A future v2 will be a different construction — a different
		// message, a different hash, or both — and trying its signature
		// against a v1 computation is not a fallback, it is comparing two
		// unrelated values and hoping. Ignoring an entry this code cannot
		// evaluate is what lets several ride one header in the first
		// place, so a v2 alongside a v1 keeps working.
		version, presented, ok := strings.Cut(token, ",")
		if !ok || version != "v1" || presented == "" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(presented)
		if err != nil {
			continue
		}
		if hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

// gitlabKey is the signing key a "whsec_…" secret denotes, and whether the
// secret is one at all.
//
// The prefix marks a STANDARD base64 payload and the key is those decoded
// bytes, never the printable form. GitLab has no fallback here — it always
// keys on the decoded bytes — so a secret this engine reads any other way
// simply computes a different HMAC and refuses every delivery.
//
// That is why a malformed one is reported rather than keyed on verbatim: the
// printable string matches nothing GitLab produces, turning "your secret is
// not a whsec_ value" into endless signature mismatches that look like an
// attack. Answering false makes the route say `no_webhook_secret` instead.
func gitlabKey(secret string) ([]byte, bool) { return whsec.Key(secret) }

// withinWindow reports whether a signed decimal-seconds timestamp is close
// enough to now, in EITHER direction. A future stamp is as suspect as an old
// one: it is what a replay looks like against a node whose clock ran slow.
func withinWindow(timestamp string, now time.Time) bool {
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	skew := now.Sub(time.Unix(seconds, 0))
	if skew < 0 {
		skew = -skew
	}
	return skew <= replayWindow
}

func mac(secret string, body []byte) []byte { return macKey([]byte(secret), body) }

func macKey(key, body []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(body)
	return h.Sum(nil)
}

// equalHex decodes a presented hex digest and compares it in constant time.
func equalHex(presented string, want []byte) bool {
	got, err := hex.DecodeString(presented)
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

// verifyToken compares a shared token constant-time.
//
// The check for the two providers that CANNOT sign a body: Datadog attaches
// fixed-value headers only, and Confluence Cloud delivers only what was written
// into its URL. The token IS the authentication, hence constant-time.
//
// ONE FUNCTION, because a constant-time compare written twice is how one copy
// stops being constant-time.
//
// The body is taken and ignored to fit its neighbours' shape. An empty secret
// never reaches here: [Receiver.authenticate] answers 503 first.
func verifyToken(_ []byte, secret, token string) bool {
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
}
