package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
)

// How this binary's node clients render a refusal.
//
// ONE RENDERER, because there are five of them — [configClient.refusal],
// [secretsClient.refusal], [nodeError], [chartRefusal] and [iamRefusal] —
// each talking to the same [httpjson] surface, and the rule they share is not
// the interesting part of any of them. Written twice and about to be written a
// third time, it was already asymmetric: the node client decoded `error`
// alone, so a route that answered with a detail and a hint reached an operator
// as the bare code, and on the drain's refusal the bare code is the half that
// does not help.

// credentialRefusal renders the two refusals that are about the CREDENTIAL
// rather than the request, and reports whether raw was one of them.
//
// TWO FACTS THAT WERE ONE SENTENCE. A 401 is a credential the node did not
// accept at all. A 403 `unauthorized` is one it DID accept, whose grants do
// not reach this verb — and the body names the grants that would have. The
// node client answered both with "the node refused the token: check it
// against the api.auth.tokens entry you meant to use", which sent an operator
// whose token works to look for a typo in it, and the other four rendered the
// 403 as a bare code or a bare reason with the grants dropped: the one fact
// that says whom to ask for what.
//
// A 403 whose body is not a refusal on authority — a seat the chart no longer
// holds, a mint request asking for more than its owner carries — is not this
// function's, and each client renders it with the rest of its answers.
func credentialRefusal(status int, raw []byte, sentToken bool) (string, bool) {
	switch status {
	case http.StatusUnauthorized:
		if !sentToken {
			return "no token was sent: export " + apiTokenEnv + " (or pass -token) with one of " +
				"the values in the node's api.auth.tokens, or a machine token " +
				"minted by `crewlet iam token`", true
		}
		return "the node did not accept the token in " + apiTokenEnv + ": it " +
			"must be one of the values in that node's api.auth.tokens, or a " +
			"live machine token minted by `crewlet iam token`", true
	case http.StatusForbidden:
		var body map[string]json.RawMessage
		if json.Unmarshal(raw, &body) != nil {
			return "", false
		}
		var code, reason string
		var grants []string
		_ = json.Unmarshal(body["error"], &code)
		_ = json.Unmarshal(body[authz.DetailReason], &reason)
		_ = json.Unmarshal(body[authz.DetailGrants], &grants)
		if code != string(httpjson.CodeUnauthorized) || (reason == "" && grants == nil) {
			return "", false
		}
		if len(grants) > 0 {
			return "the node accepted the token, and it does not carry " +
				strings.Join(grants, " or ") + ", which this needs", true
		}
		// NO GRANT WOULD: what is missing is a relation — the seat is not
		// the caller's and they do not lead it — and the reason names it.
		return "the node accepted the token, and no grant admits this " +
			"request (" + reason + ")", true
	}
	return "", false
}

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
