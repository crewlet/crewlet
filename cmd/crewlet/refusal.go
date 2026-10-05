package main

import (
	"bytes"
	"fmt"

	"github.com/crewlet/crewlet/internal/httpx"
)

// How this binary's three node clients render a refusal.
//
// ONE RENDERER, because there are three of them — [configClient.refusal],
// [secretsClient.refusal] and [nodeError] — each talking to the same
// [httpjson] surface, and the rule they share is not the interesting part of
// any of them. Written twice and about to be written a third time, it was
// already asymmetric: the node client decoded `error` alone, so a route that
// answered with a detail and a hint reached an operator as the bare code, and
// on the drain's refusal the bare code is the half that does not help.

// withRefusalDetail appends a refusal's detail and hint to the message a
// client has already built, each on its own indented line, skipping whichever
// the body did not carry.
//
// THE MESSAGE IS THE CALLER'S, not this function's — each builds it from the
// engine's `error`, or from [foreignAnswer] when the body carried none. What
// is shared here is what happens to `detail` and `hint`, which is the part
// that was drifting.
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

// foreignAnswer is what an answer that is NOT the engine's JSON said — a
// proxy's page, a gateway's plain-text error — for an error a person reads.
//
// Through [httpx.Refusal], for every one of the three clients: an HTML page's
// title, plain text as itself, and a body past what that reads marked. They
// used to differ — the config client quoted the first two kilobytes of the
// markup, the secrets client the whole answer, up to 68 KiB of it — and the
// one sentence the page held was in neither. A body with nothing readable in
// it says how large it was rather than reading as an empty one.
func foreignAnswer(contentType string, raw []byte) string {
	if said := httpx.Refusal(contentType, raw); said != "" {
		return said
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return "(an empty body)"
	}
	kind := contentType
	if kind == "" {
		kind = "an untyped body"
	}
	return fmt.Sprintf("(%d bytes of %s with nothing readable in them)", len(raw), kind)
}
