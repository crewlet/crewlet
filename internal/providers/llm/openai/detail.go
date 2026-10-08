package openai

import (
	"io"

	sdk "github.com/openai/openai-go/v3"

	"github.com/crewlet/crewlet/internal/providers/llm/httpapi"
)

// Detail is what an OpenAI-compatible endpoint SAID about failing a request —
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
// the same 400 or 404. The fields are still on the error; this reads them, for
// both clients built on the SDK (the chat backend here, and
// internal/providers/embeddings).
//
// # What is shown, and what never is
//
// The STRUCTURED fields first, since they are the endpoint's own account. An
// endpoint that does not answer in OpenAI's error envelope — a gateway, a
// self-hosted server — leaves them empty, and its body is then read for what
// it can honestly yield ([httpapi.SaidBody]), from the copy the SDK puts back
// on the response. The request URL is never shown, and neither is anything
// the SDK's dumps would add: upstream removed them for a reason this reading
// keeps. Either way the text is redacted whole before it is bounded
// ([httpapi.Said]), because upstream's warning is true — an endpoint that
// rejects a key can echo it.
func Detail(apiErr *sdk.Error) string {
	if apiErr == nil {
		return ""
	}
	said := httpapi.Said(apiErr.Message,
		httpapi.Filed{Name: "type", Value: apiErr.Type},
		httpapi.Filed{Name: "code", Value: apiErr.Code},
		httpapi.Filed{Name: "param", Value: apiErr.Param})
	if said != "" {
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
	if said := httpapi.SaidBody(apiErr.Response.Header.Get("Content-Type"), body); said != "" || err == nil {
		return said
	}
	return "(the response body could not be read: " + err.Error() + ")"
}
