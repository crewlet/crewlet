package slack

import (
	"net/url"
	"strings"
)

// InstallCode reads the temporary OAuth code out of whatever the operator
// pasted.
//
// # Three shapes, because all three are what people actually paste
//
// The landing page shows the bare code, so that is the common case. But a
// browser's address bar holds the whole redirect URL, and copying that is
// the obvious thing to do when the page is not read carefully — so a URL
// with a `code` parameter is accepted, and so is a bare `code=…` fragment.
// Refusing them would fail a run for a reason the operator cannot see, since
// the pasted text CONTAINS the right answer.
func InstallCode(pasted string) string {
	pasted = strings.TrimSpace(pasted)
	if pasted == "" {
		return ""
	}
	if parsed, err := url.Parse(pasted); err == nil {
		if code := parsed.Query().Get("code"); code != "" {
			return code
		}
	}
	// A bare "code=…&state=…" fragment, which is what a partial copy out
	// of the address bar gives.
	if values, err := url.ParseQuery(pasted); err == nil {
		if code := values.Get("code"); code != "" {
			return code
		}
	}
	// Anything else is taken as the code itself. A wrong one fails at the
	// exchange with Slack's own `invalid_code`, which says more than any
	// guess this function could make.
	return pasted
}
