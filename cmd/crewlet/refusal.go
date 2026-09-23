package main

import (
	"encoding/json"

	"github.com/crewlet/crewlet/internal/httpx"
)

// How this binary's three node clients render a refusal.
//
// ONE RENDERER, because there are three of them — [configClient.refusal],
// [secretsClient.refusal] and [nodeClient.refusal] — each talking to the same
// [httpjson] surface, and the rule they share is not the interesting part of
// any of them. Three decisions are shared: what makes a body the node's own
// ([refusalBody.fromNode]), what a body that is not says
// ([unrecognisedRefusal]), and what happens to `detail` and `hint`
// ([withRefusalDetail]). Everything else — which of the node's codes means
// what on which surface — stays with the client that knows its routes.

// refusalBody is the shape a node's route refuses in: a machine code in
// `error`, a `detail` saying what happened and a `hint` saying what to do.
type refusalBody struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
	Hint   string `json:"hint"`
}

// decodeRefusal reads a refusal's body into into, which is a *refusalBody or a
// struct embedding one.
//
// The decode error is DROPPED, and nothing is lost by it: a body that is not
// JSON leaves `error` empty, which is exactly the answer [refusalBody.fromNode]
// needs, and a body that IS JSON but not the node's decodes without error and
// leaves `error` empty too — so whether it decoded never said whose it was.
func decodeRefusal(raw []byte, into any) {
	_ = json.Unmarshal(raw, into)
}

// fromNode reports whether the body carries the node's own `error` code.
//
// THE CODE, NEVER THE STATUS, is what a client may interpret. A proxy, a
// gateway or an auth wall in front of the node answers 401, 404 and 503 too,
// with a page — or a JSON object — of its own, and reading one of those as the
// node's sends an operator to a node that never saw the request: to the
// api.auth.tokens entry of a node an SSO wall stopped the call short of, or to
// the backends of a node that is fine while the load balancer in front of it
// has no healthy peer. A body without a code gets no status arm at all; it is
// shown, through [unrecognisedRefusal], for the operator to recognise.
func (b refusalBody) fromNode() bool { return b.Error != "" }

// said is the node's own refusal: its code, then its detail and hint.
func (b refusalBody) said() string { return withRefusalDetail(b.Error, b.Detail, b.Hint) }

// withRefusalDetail appends a refusal's detail and hint to the message a
// client has already built, each on its own indented line, skipping whichever
// the body did not carry.
//
// `error` says WHAT happened and `hint` says WHAT TO DO, which is why dropping
// the second is worse than it looks: "draining" tells an operator nothing they
// can act on, while the hint beside it names the peer to retry against.
func withRefusalDetail(msg, detail, hint string) string {
	for _, extra := range []string{detail, hint} {
		if extra != "" {
			msg += "\n  " + extra
		}
	}
	return msg
}

// unrecognisedRefusal is what a refusal says when its body carried no `error`
// code: a proxy's page, a gateway's text or JSON, or a server that is not a
// node at all.
//
// THROUGH [httpx.RefusalOf], the shared answer to the same question the vendor
// clients ask, rather than the raw body. Each client here reads an answer up to
// its own response ceiling — a megabyte for the node client — so a body pasted
// as it arrived is a proxy's whole HTML page in the operator's terminal.
// RefusalOf reduces a page to its <title>, compacts JSON of a shape this build
// does not know, collapses plain text onto one line, and cuts what is left
// inside [httpx.RefusalDetail] with a marker, so a cut line cannot read as the
// endpoint's complete answer.
//
// THE WHOLE BODY IS NOT KEPT: each client reads an answer once and drops it.
// Each client's message around this line names the address and the route it
// asked, and sending that request to that address again is how to see the
// page whole — for a GET, `curl -i` of the address followed by the path. For
// a write, sending it again is writing again.
//
// An EMPTY body is named rather than rendered as nothing: RefusalOf answers ""
// for exactly that case, and a line ending in a colon reads as a message that
// failed to print.
func unrecognisedRefusal(contentType string, raw []byte) string {
	if line := httpx.RefusalOf(contentType, raw, nil); line != "" {
		return line
	}
	return "an empty body"
}
