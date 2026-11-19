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

// withoutAmbientEnvironment undoes what openai-go's NewClient reads from the
// process environment (seven variables, DefaultClientOptions; the SDK's own
// switch is internal), so the client sends only what the company configured:
// `base_url` may name a gateway that must not see a credential exported for
// something else. OPENAI_API_KEY and OPENAI_BASE_URL are overridden by [New];
// ADMIN_KEY and WEBHOOK_SECRET are cleared so the client does not HOLD them;
// ORG_ID and PROJECT_ID are headers on every request; CUSTOM_HEADERS lines are
// deleted by name, parsed as the SDK parses them.
//
// Applied BEFORE the caller's options: deleting a custom `Authorization` header
// marks the request as carrying an explicit one, and the caller's WithAPIKey
// after it hands authorization back to the configured key.
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

// detail is what an OpenAI-compatible endpoint SAID about failing a request
// (its message, and the type, code and param it filed it under) as one
// redacted, bounded line, or "" where it said nothing.
//
// The SDK's API error prints the status alone, deliberately (the URL and
// fields "may contain secrets"), so %v says THAT an endpoint refused, never WHY:
// a context length, a `dimensions` the model does not take, an unserved model
// and a rejected key all read as the same 400 or 404. The fields are still on
// the error; this reads the STRUCTURED ones first, then, for an endpoint outside
// OpenAI's envelope, the body copy the SDK puts back. The URL is never shown.
// The text is redacted whole before it is bounded ([shown]): an endpoint that
// rejects a key can echo it.
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
