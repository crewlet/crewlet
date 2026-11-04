package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"sync"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/mcp/oauth"
)

// THE MCP SIGN-IN'S TWO PUBLIC ADDRESSES, under /webhooks/ because that is the
// partition a vendor and a person's browser reach without an operator
// credential (internal/api/auth's public prefixes): the callback a vendor sends
// the person back to, and the engine's client metadata document a vendor reads
// its client id from.
//
// # The callback trusts nothing but the state's signature
//
// `GET /webhooks/mcp-oauth` is unauthenticated: a redirect carries no engine
// credential. What it does (exchange a code and seal a grant every agent
// reaches a server with) is decided by the state alone, which the engine
// signed under the fleet keyring when the sign-in started and spends once
// (internal/engine's mcpoauth.go). Every query value is bounded before
// anything reads it, nothing the vendor or the address sent is shown, and
// the page is a template with no script under its own Content-Security-Policy,
// on the dashboard's origin.

// MCPSignIns finishes the MCP sign-ins the engine started and publishes its
// client metadata document.
type MCPSignIns interface {
	// CompleteSignIn finishes the sign-in landing's state names, answering
	// the server's name; a refusal is an [*MCPSignInRefusal].
	CompleteSignIn(ctx context.Context, landing MCPLanding) (string, error)
	// ClientMetadata is the engine's client metadata document, false where
	// the engine has no public address to publish one at.
	ClientMetadata() (oauth.Metadata, bool)
}

// MCPLanding is what a vendor sent a person's browser back with.
type MCPLanding struct {
	State, Code, Issuer string
	// Error and Description are the vendor's refusal (RFC 6749, 4.1.2.1).
	Error, Description string
}

// MCPSignInRefusal is why a sign-in could not be finished, in words a person
// acts on, and the status the page answers with. It never carries the code, a
// token or the verifier.
type MCPSignInRefusal struct {
	Status         int
	Reason, Remedy string
}

func (r *MCPSignInRefusal) Error() string { return "webhooks: " + r.Reason + ". " + r.Remedy }

// The bounds of a landing's query, past which it is no landing this engine
// sent anybody to. REASONED: a state the engine signs is a server name, a uuid
// and a signature, a few hundred bytes; an authorization code is opaque and
// some vendors make it a signed token of a few kilobytes; an issuer is an
// address; an error code is one short word and its description a sentence.
const (
	maxMCPState       = 1 << 10
	maxMCPCode        = 8 << 10
	maxMCPIssuer      = 1 << 10
	maxMCPError       = 64
	maxMCPDescription = 1 << 10
)

// mcpOAuthPage is the page a person lands on after consenting at a vendor,
// and its Content-Security-Policy: its one inline style, by hash, and no
// script at all. html/template for [slackOAuthPage]'s reason: every value it
// renders is escaped for where it lands, and none is anything the query
// carried.
//
// BUILT AT THE FIRST REQUEST THAT RENDERS IT, for [githubAppPage]'s reason:
// every push's proc-receive hook is this binary, and runs every package's
// init. Built at init it was this package's largest cost there, and its
// least steady: the policy matches the rendered page against patterns whose
// matchers come from a pool the race detector empties at random, so the same
// build allocated 59 KB, 96 KB or 133 KB here from one start to the next.
var mcpOAuthPage = sync.OnceValues(func() (*template.Template, string) {
	page := template.Must(template.New("mcp-oauth").Parse(mcpOAuthTemplate))
	return page, pagepolicy.MustForTemplate(page, mcpOAuthView{})
})

// mcpOAuthTemplate is the page's text.
const mcpOAuthTemplate = `<!doctype html>
<html>
  <head>
    <meta charset="utf-8">
    <title>Crewlet: MCP server sign-in</title>
    <style>
      body { font-family: system-ui, sans-serif; max-width: 40rem;
             margin: 4rem auto; padding: 0 1rem; line-height: 1.5; }
      .muted { opacity: .7; font-size: .9rem; }
    </style>
  </head>
  <body>
    {{if .Server}}<h1>Signed in to {{.Server}}</h1>
      <p>Crewlet keeps the sign-in for the company and starts the server on
      every node. Its tools reach the agents granted it in a moment.</p>
      <p class="muted">You can close this tab and go back to Settings, MCP &amp; tools.</p>
    {{else}}<h1>Sign-in not finished</h1>
      <p>{{.Reason}}.</p>
      <p>{{.Remedy}}.</p>
    {{end}}
  </body>
</html>
`

type mcpOAuthView struct {
	// Server is the server signed in to, from the signed state; "" for a
	// sign-in that was not finished, which Reason and Remedy explain.
	Server         string
	Reason, Remedy string
}

// mcpOAuthLanding serves GET /webhooks/mcp-oauth.
func (r *Receiver) mcpOAuthLanding(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	landing := MCPLanding{State: q.Get("state"), Code: q.Get("code"), Issuer: q.Get("iss"),
		Error: q.Get("error"), Description: q.Get("error_description")}
	view := mcpOAuthView{}
	status := http.StatusOK
	switch {
	case len(landing.State) > maxMCPState || len(landing.Code) > maxMCPCode ||
		len(landing.Issuer) > maxMCPIssuer || len(landing.Error) > maxMCPError ||
		len(landing.Description) > maxMCPDescription:
		status = http.StatusBadRequest
		view.Reason = "this address is not one a sign-in returns to"
		view.Remedy = "Start the sign-in again from the server's page in Settings, MCP & tools"
	case landing.State == "":
		status = http.StatusBadRequest
		view.Reason = "the vendor sent no sign-in state with this page"
		view.Remedy = "Start the sign-in from the server's page in Settings, MCP & tools"
	case r.mcpSignIns == nil:
		// NOT IMPLEMENTED HERE, rather than unavailable: this node does not
		// finish sign-ins at all, and waiting would not change that.
		status = http.StatusNotImplemented
		view.Reason = "this engine does not finish MCP sign-ins"
		view.Remedy = "Start the sign-in again from an engine serving the dashboard"
	default:
		server, err := r.mcpSignIns.CompleteSignIn(req.Context(), landing)
		var refused *MCPSignInRefusal
		switch {
		case err == nil:
			view.Server = server
			log.InfoContext(req.Context(), "mcp_sign_in_landed", "server", server)
		case errors.As(err, &refused):
			status, view.Reason, view.Remedy = refused.Status, refused.Reason, refused.Remedy
			log.WarnContext(req.Context(), "mcp_sign_in_refused", "server", server, "error", err.Error())
		default:
			status = http.StatusInternalServerError
			view.Reason = "the sign-in could not be finished"
			view.Remedy = "Start it again from the server's page in Settings, MCP & tools"
			log.WarnContext(req.Context(), "mcp_sign_in_refused", "server", server, "error", err.Error())
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// NOT CACHED: a page a code arrived on should not outlive the visit in
	// a browser's cache.
	w.Header().Set("Cache-Control", "no-store")
	// NO REFERRER, so the next page the person opens is not told the
	// address the code and the state arrived in.
	w.Header().Set("Referrer-Policy", "no-referrer")
	page, policy := mcpOAuthPage()
	pagepolicy.Set(w.Header(), policy)
	w.WriteHeader(status)
	if err := page.Execute(w, view); err != nil {
		log.Warn("mcp_oauth_page_failed", "error", err)
	}
}

// mcpClientMetadata serves GET /webhooks/mcp-oauth/client-metadata.json: the
// engine's client metadata document, which an authorization server that reads
// one fetches from the client id a cimd sign-in sends. Public by design: it
// names the engine's callback and nothing else.
func (r *Receiver) mcpClientMetadata(w http.ResponseWriter, _ *http.Request) {
	var doc oauth.Metadata
	ok := r.mcpSignIns != nil
	if ok {
		doc, ok = r.mcpSignIns.ClientMetadata()
	}
	if !ok {
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNotFound, httpjson.Detail{
			"detail": "this engine publishes no client metadata document: it has no public address " +
				"(api.external_url) to name a callback at"})
		return
	}
	body, err := json.Marshal(doc)
	if err != nil {
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// AN HOUR: the document changes only when the engine's address does,
	// which is a restart, and a vendor that fetches it on every sign-in is
	// spared the round trip.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(body)
}
