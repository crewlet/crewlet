package httpapi

import (
	"strings"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/redact"
)

// Filed is one field a provider filed a refusal under — its type, its code,
// the parameter it objected to — named as [Said] shows it.
type Filed struct {
	Name  string
	Value string
}

// Said is what a provider SAID about refusing a request — its own message, and
// the fields it filed the refusal under — as one redacted, bounded line, or ""
// where it said nothing.
//
// It is how a backend's classified error ([llm.Error.Detail], and the
// embeddings backend's) carries the provider's reason, which the SDK's own
// error does not, or must not: OpenAI's names the status alone, deliberately,
// because its URL and the provider's fields "may contain secrets", and
// Anthropic's prints the URL and pastes the raw body, so it is never shown
// (see [FromStatus]). The reason is what an operator acts on — a context
// length, a model a gateway does not serve and a key it rejects all arrive as
// one 400 or 401 — and only the error's own fields still carry it.
//
// The fields are shown only where they say something, and the message first:
// "This model's maximum context length is 8192 tokens (type
// invalid_request_error, code context_length_exceeded)".
func Said(message string, filed ...Filed) string {
	message = strings.TrimSpace(message)
	var fields []string
	for _, f := range filed {
		if value := strings.TrimSpace(f.Value); value != "" {
			fields = append(fields, f.Name+" "+value)
		}
	}
	said := strings.Join(fields, ", ")
	switch {
	case message == "":
	case said == "":
		said = message
	default:
		said = message + " (" + said + ")"
	}
	return shown("text/plain", said)
}

// SaidBody is what a provider said in a refused response's body that is NOT
// its error envelope — a gateway's, a proxy's, a self-hosted server's own
// shape — read for what it can honestly yield ([httpx.Refusal]: compact JSON,
// an HTML page's title, plain text), redacted and bounded like [Said].
//
// body is the WHOLE body, which every caller already holds: both SDKs read a
// refused response whole to build their error and keep it — OpenAI's puts a
// copy back on the response, Anthropic's keeps it as the error's raw JSON.
func SaidBody(contentType string, body []byte) string {
	return shown(contentType, string(body))
}

// shown is the one order a provider's words are shown in: REDACTED WHOLE, then
// shaped and bounded.
//
// THE REDACTION COMES FIRST because a bound cuts wherever the bytes run out,
// and a credential the cut runs through is left as a prefix too short for any
// rule to recognise — `sk-proj-` and a dozen characters of the key, shown as
// they are. Redacted first, a credential is whole when it is matched and only
// its marker can be cut. It costs one linear pass over a text the caller
// already holds whole — the SDK read and parsed every byte of it to build its
// error — and it is needed at all because an endpoint that rejects a key can
// echo it.
func shown(contentType, text string) string {
	return httpx.Refusal(contentType, []byte(redact.Secrets(text)))
}
