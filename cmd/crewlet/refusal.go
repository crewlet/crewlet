package main

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
// THE MESSAGE IS THE CALLER'S, not this function's: what to say when the body
// carried no `error` at all differs per client — a proxy's HTML page elided to
// a screenful for the config surface, the raw answer for the others — and that
// decision is theirs. What is shared is only what happens to `detail` and
// `hint`, which is the part that was drifting.
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
