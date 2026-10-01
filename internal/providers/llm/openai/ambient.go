package openai

import (
	"os"
	"strings"

	"github.com/openai/openai-go/v3/option"
)

// WithoutAmbientEnvironment is the options that undo everything openai-go's
// NewClient reads from the process environment, for a client that must send
// only what the company configured.
//
// THE SDK READS SEVEN VARIABLES AT CONSTRUCTION (DefaultClientOptions) and
// exposes no public way to stop it — the switch it has,
// requestconfig.WithEnvironmentDefaultsDisabled, is internal and reached only
// through its Azure and Bedrock options. Every one of them is a leak here,
// because both clients built on this SDK point at whatever `base_url` an
// `openai-compatible` entry or an embedder names — a gateway, a self-hosted
// server — and none of those should see a credential the operator happened
// to export for something else:
//
//   - OPENAI_API_KEY and OPENAI_BASE_URL: overridden by the caller, which
//     always sets its own key (an empty one included) and base URL;
//   - OPENAI_ADMIN_KEY: the organization's admin credential. The chat and
//     embeddings endpoints declare bearer-only security, so today it is never
//     SENT from here — it is cleared so the client does not HOLD it, and a
//     call this package adds later cannot be the one that sends it;
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
func WithoutAmbientEnvironment() []option.RequestOption {
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
