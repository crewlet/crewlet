package embeddings

import (
	"io"
	"os"
	"strings"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/redact"
)

// withoutAmbientEnvironment is the options that undo everything openai-go's
// NewClient reads from the process environment, for a client that must send
// only what the company configured.
//
// THE SDK READS SEVEN VARIABLES AT CONSTRUCTION (DefaultClientOptions) and
// exposes no public way to stop it — the switch it has,
// requestconfig.WithEnvironmentDefaultsDisabled, is internal and reached only
// through its Azure and Bedrock options. Every one of them is a leak here,
// because this client points at whatever `base_url` an embedder names — a
// gateway, a self-hosted server — and none of those should see a credential
// the operator happened to export for something else:
//
//   - OPENAI_API_KEY and OPENAI_BASE_URL: overridden by [New], which always
//     sets its own key (an empty one included) and base URL;
//   - OPENAI_ADMIN_KEY: the organization's admin credential. The embeddings
//     endpoint declares bearer-only security, so today it is never SENT from
//     here — it is cleared so the client does not HOLD it, and a call this
//     package adds later cannot be the one that sends it;
//   - OPENAI_ORG_ID and OPENAI_PROJECT_ID: the OpenAI-Organization and
//     OpenAI-Project headers, on every request to every endpoint;
//   - OPENAI_CUSTOM_HEADERS: arbitrary `Name: value` lines, each a header on
//     every request — deleted by name, parsed exactly as the SDK parses them;
//   - OPENAI_WEBHOOK_SECRET: read only by webhook verification, which nothing
//     here calls, and cleared anyway so the client holds nothing it was not
//     given.
//
// These are applied BEFORE the caller's own options, because deleting a
// custom `Authorization` header marks the request as carrying an explicit
// one, and it is the caller's WithAPIKey after it that hands authorization
// back to the configured key.
func withoutAmbientEnvironment() []option.RequestOption {
	opts := []option.RequestOption{
		option.WithAdminAPIKey(""),
		option.WithOrganization(""),
		option.WithHeaderDel("OpenAI-Organization"),
		option.WithProject(""),
		option.WithHeaderDel("OpenAI-Project"),
		option.WithWebhookSecret(""),
	}
	if custom, ok := os.LookupEnv("OPENAI_CUSTOM_HEADERS"); ok {
		for _, line := range strings.Split(custom, "\n") {
			if colon := strings.Index(line, ":"); colon >= 0 {
				opts = append(opts, option.WithHeaderDel(strings.TrimSpace(line[:colon])))
			}
		}
	}
	return opts
}

// detail is what an OpenAI-compatible endpoint SAID about failing a request —
// its own message, and the type, code and param it filed the failure under —
// as one redacted, bounded line, or "" where it said nothing.
//
// # Why the SDK's own error is not enough
//
// openai-go's API error answers Error(), String(), Format and LogValue with the
// status alone ("OpenAI API error: 400 Bad Request"). That is deliberate
// upstream: the request URL and the provider's fields "may contain secrets",
// so the summary carries neither. But an error formatted with %v then says
// THAT an endpoint refused and never WHY, and the why is what an operator acts
// on: a context length the endpoint enforces, a `dimensions` the model does not
// take, a model the gateway does not serve and a key it rejects all read as
// the same 400 or 404. The fields are still on the error; this reads them.
//
// # What is shown, and what never is
//
// The STRUCTURED fields first, since they are the endpoint's own account. An
// endpoint that does not answer in OpenAI's error envelope — a gateway, a
// self-hosted server — leaves them empty, and its body is then read for what
// it can honestly yield, from the copy the SDK puts back on the response. The
// request URL is never shown, and neither is anything the SDK's dumps would
// add: upstream removed them for a reason this reading keeps. Either way the
// text is redacted whole before it is bounded ([shown]), because upstream's
// warning is true — an endpoint that rejects a key can echo it.
func detail(apiErr *sdk.Error) string {
	if apiErr == nil {
		return ""
	}
	if said := said(apiErr.Message, filed{"type", apiErr.Type}, filed{"code", apiErr.Code},
		filed{"param", apiErr.Param}); said != "" {
		return said
	}
	// NOT THE ENVELOPE, or no body at all. The SDK reads the body whole to
	// build this error and puts a copy back on the response, so this read
	// is of bytes already in memory; read once here, it is not read by
	// anything else.
	if apiErr.Response == nil || apiErr.Response.Body == nil {
		return ""
	}
	body, err := io.ReadAll(apiErr.Response.Body)
	if said := shown(apiErr.Response.Header.Get("Content-Type"), string(body)); said != "" || err == nil {
		return said
	}
	return "(the response body could not be read: " + err.Error() + ")"
}

// filed is one field an endpoint filed a refusal under — its type, its code,
// the parameter it objected to — named as [said] shows it.
type filed struct {
	name  string
	value string
}

// said is an endpoint's own message and the fields it filed the refusal
// under, as one redacted, bounded line, or "" where it said nothing. The
// fields are shown only where they say something, and the message first:
// "This model's maximum context length is 8192 tokens (type
// invalid_request_error, code context_length_exceeded)".
func said(message string, fields ...filed) string {
	message = strings.TrimSpace(message)
	var named []string
	for _, f := range fields {
		if value := strings.TrimSpace(f.value); value != "" {
			named = append(named, f.name+" "+value)
		}
	}
	text := strings.Join(named, ", ")
	switch {
	case message == "":
	case text == "":
		text = message
	default:
		text = message + " (" + text + ")"
	}
	return shown("text/plain", text)
}

// shown is the one order an endpoint's words are shown in: REDACTED WHOLE,
// then shaped and bounded ([httpx.Refusal]: compact JSON, an HTML page's
// title, plain text).
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
