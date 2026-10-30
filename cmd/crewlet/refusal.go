package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/words"
)

// How this binary's four node clients render a refusal.
//
// ONE RENDERER, because there are four of them — [configClient.refusal],
// [secretsClient.refusal], [nodeError] and [iamRefusal] —
// each talking to the same [httpjson] surface, and the rule they share is not
// the interesting part of any of them. Written twice and about to be written a
// third time, it was already asymmetric: the node client decoded `error`
// alone, so a route that answered with a detail and a hint reached an operator
// as the bare code, and on the drain's refusal the bare code is the half that
// does not help.
//
// AND IT DRIFTED AGAIN, one field later: the three clients that decode a
// struct read `detail` and `hint` and never `message`, so a refusal at
// ADR-0060's floor (no `detail`, its code's own sentence the whole of what it
// says, which is every refusal nobody has worded for a person) reached an
// operator as the bare code once more. What a refusal says is decoded into
// one type ([refusalWords]) and said by one rule ([refusalWords.said]).

// credentialRefusal renders the two refusals that are about the CREDENTIAL
// rather than the request, and reports whether raw was one of them.
//
// TWO FACTS THAT WERE ONE SENTENCE. A 401 is a credential the node did not
// accept at all. A 403 `unauthorized` is one it DID accept, whose grants do
// not reach this verb — and the body names the grants that would have. The
// node client answered both with "the node refused the token: check it
// against the api.auth.tokens entry you meant to use", which sent an operator
// whose token works to look for a typo in it, and the other three rendered the
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
			return "no token was sent. Set " + apiTokenEnv + " to an API " +
				"token or an access token", true
		}
		return "the node didn't accept the token in " + apiTokenEnv + ". " +
			"Check it hasn't expired or been revoked", true
	case http.StatusForbidden:
		var body map[string]json.RawMessage
		if json.Unmarshal(raw, &body) != nil {
			return "", false
		}
		var code, reason, message string
		var grants []string
		_ = json.Unmarshal(body["error"], &code)
		_ = json.Unmarshal(body[authz.DetailReason], &reason)
		_ = json.Unmarshal(body[authz.DetailGrants], &grants)
		_ = json.Unmarshal(body["message"], &message)
		if code != string(httpjson.CodeUnauthorized) || (reason == "" && grants == nil) {
			return "", false
		}
		lacks := reason == "" || reason == string(authz.ReasonNoGrant)
		switch {
		case reason == string(authz.ReasonUnseated):
			// NO GRANT WOULD, AND NO RELATION EITHER: the verb is made AS
			// A SEAT — said in a room, read as somebody who is in it — and
			// this credential acts as itself. What changes it is which
			// credential, or a binding, so that is what is named, in this
			// binary's own commands: the reason's bare word sent an
			// operator holding admin looking for a permission they already
			// had.
			return "this token acts as no seat, and this is done as one. Use " +
				"your own token (`crewlet iam token -login LOGIN`), or bind " +
				"this one to a human seat (`crewlet iam bind ID SEAT`)", true
		case lacks && len(grants) > 0:
			return "this token doesn't have " + words.Or(grants) +
				". Ask an admin for it", true
		case message != "":
			// ANY OTHER RULE IS THE NODE'S TO WORD, and its `message`
			// follows the reason: a relation it refused, naming the
			// permission that would admit the token anyway, or one no
			// permission changes — an account that is not active, a verb
			// only an agent's seat makes. This said the reason's bare word
			// for all of them, and that what was missing was a relation,
			// which for most of them it was not.
			return "this token isn't allowed to do this (" + reason + "): " +
				message, true
		case len(grants) > 0:
			return "this token doesn't have " + words.Or(grants) +
				". Ask an admin for it", true
		}
		// NO GRANT WOULD, and the node named no rule's words for it.
		return "this token isn't allowed to do this (" + reason + ")", true
	}
	return "", false
}

// refusalWords are what an engine refusal says to a person beside its code,
// as every client here decodes them from the envelope: embedded in each
// client's own body, so none can decode two of them and miss the third.
type refusalWords struct {
	// Message is the code's own sentence, written for people
	// ([httpjson.Code.Message]); the envelope carries it beside every code
	// the vocabulary words.
	Message string `json:"message"`
	// Detail is this refusal in a person's words, where the node worded one.
	Detail string `json:"detail"`
	// Hint says what to do about it.
	Hint string `json:"hint"`
}

// said is the sentence saying what happened: the detail where the node worded
// one, and the code's own message where it did not.
//
// ONE OR THE OTHER, NEVER BOTH, as the dashboard reads them (ADR-0060): a
// detail is the same refusal said of this request, so it replaces the code's
// general sentence rather than standing under it, and a refusal nobody has
// worded for a person still says what kind of refusal it is.
func (w refusalWords) said() string {
	if w.Detail != "" {
		return w.Detail
	}
	return w.Message
}

// answerWords is [refusalWords] off an answer decoded whole, as the iam
// clients decode theirs.
func answerWords(answer map[string]any) refusalWords {
	var words refusalWords
	words.Message, _ = answer["message"].(string)
	words.Detail, _ = answer["detail"].(string)
	words.Hint, _ = answer["hint"].(string)
	return words
}

// withRefusalDetail appends what a refusal says to the message a client has
// already built, each on its own indented line: [refusalWords.said], then the
// hint, skipping whichever the body did not carry.
//
// THE MESSAGE IS THE CALLER'S, not this function's — each builds it from the
// engine's `error`, or from [foreignAnswer] when the body carried none. What
// is shared here is what happens to the words beside it, which is the part
// that was drifting.
//
// `error` says WHAT happened and `hint` says WHAT TO DO, which is why dropping
// the second is worse than it looks: "draining" tells an operator nothing they
// can act on, while the hint beside it names the peer to retry against.
func withRefusalDetail(msg string, words refusalWords) string {
	for _, extra := range []string{words.said(), words.Hint} {
		if extra != "" {
			msg += "\n  " + extra
		}
	}
	return msg
}

// ownBody is an answer's body where the engine wrote it, and nil where it did
// not: an answer without the engine's mark ([httpjson.Marked]) was written by
// something in front of the node, whatever its body says, so none of its
// fields is read as the engine's. A proxy can answer JSON with an `error`
// field; read as the engine's, its 409 was a lost race and its 401 a token
// the node refused. What it said is still rendered, through [foreignAnswer],
// from the raw body.
func ownBody(h http.Header, raw []byte) []byte {
	if !httpjson.Marked(h) {
		return nil
	}
	return raw
}

// foreignAnswer is what an answer that is NOT the engine's JSON said — a
// proxy's page, a gateway's plain-text error — for an error a person reads.
//
// Through [httpx.Refusal], for every one of the four clients: an HTML page's
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
