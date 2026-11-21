package setupapi

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// Reading the document a submission is measured against, and producing the
// values a person should not be asked to invent.

// decode reads a JSON body strictly, so a misspelled key is a refusal rather
// than a field silently ignored. Its error is the decoder's, which a caller
// answers in words ([plainjson.Unreadable]).
func decode(body []byte, into any) error {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	return nil
}

// mintInto generates the values the caller asked the engine to produce.
//
// A MINT IS A DIFFERENT REQUEST FROM A VALUE, which is why it arrives as a
// separate list. A shared token is a password nobody should be asked to
// invent, and a client that could supply one under the same key as a mint
// request would eventually supply a weak one by accident.
func mintInto(values map[string]string, reqs []setup.Requirement, generate []string) error {
	byField := map[string]setup.Requirement{}
	for _, r := range reqs {
		byField[r.Field] = r
	}
	// THE SUPPLIED SET IS TAKEN BEFORE ANYTHING IS MINTED, because minting
	// writes into the same map the collision check reads. A field named
	// twice in `generate` otherwise met its own first minting and was
	// refused as "both supplied and asked to be generated" — a sentence
	// about a request the caller did not make.
	supplied := make(map[string]struct{}, len(values))
	for field := range values {
		supplied[field] = struct{}{}
	}
	minted := make(map[string]struct{}, len(generate))
	for _, field := range generate {
		r, ok := byField[field]
		if !ok {
			return fmt.Errorf("no field %q to generate", field)
		}
		if !r.Mintable {
			return fmt.Errorf("%s is not something this engine can generate; "+
				"it comes from the third-party app", field)
		}
		if _, twice := minted[field]; twice {
			return fmt.Errorf("%s is named twice in generate; name it once", field)
		}
		minted[field] = struct{}{}
		if _, sent := supplied[field]; sent {
			return fmt.Errorf(
				"%s was both supplied and asked to be generated; send one or the other", field)
		}
		value, err := r.Shape.Mint(mint)
		if err != nil {
			return fmt.Errorf("generate %s: %w", field, err)
		}
		values[field] = value
	}
	return nil
}

// mint produces a shared token.
//
// crypto/rand's text encoding: 26 base32 characters over 128 bits of entropy,
// URL-safe for a third-party app's header field. The same generator the
// provisioning passes use.
//
// NOT EVERY MINTABLE FIELD TAKES ONE. A field the app accepts in exactly one
// form says so with [setup.Requirement.Shape], and this is what the rest get.
// Config validation cannot check the shape of the `${VAR}` the document gets,
// so a token minted into GitLab's signing secret would be no HMAC key for any
// delivery and nothing else would catch it.
func mint() string { return rand.Text() }

// refuseEmpty rejects a submission that clears a required field.
//
// An empty string in a submission is almost always a form that rendered a
// value it never had and sent it back blank. Writing it would disable a
// working integration and report 201, so it is refused by name.
func refuseEmpty(values map[string]string, reqs []setup.Requirement) error {
	byField := map[string]setup.Requirement{}
	for _, r := range reqs {
		byField[r.Field] = r
	}
	for field, value := range values {
		if strings.TrimSpace(value) != "" {
			continue
		}
		// A SECRET COUNTS AS WELL AS A REQUIRED FIELD, and for the same
		// reason rather than a different one. An optional credential that
		// is already set is a WORKING credential: an empty submission
		// seals "" over it, and every route that resolves it then reads a
		// value that is present and useless. There is no way to clear one
		// here in any case — this route's own answer to "leave it alone"
		// is to omit the field — so an empty secret is never a request,
		// only ever a form that rendered a value it never had.
		if r := byField[field]; r.Required || r.Kind == setup.KindSecret {
			return fmt.Errorf(
				"%s is a credential the submission clears; omit the field to leave it alone",
				field)
		}
	}
	return nil
}

// refuseUngated refuses a submission that turns a gate on without answering
// the field it opens.
//
// It reads the SUBMITTED values, never the stored ones:
// [setup.Requirement.Needed] answers from the document as it is, the wrong
// question when this request changes the answer (organization-wide coverage
// with no token would read the replaced value, "not required yet"). So the
// gating field comes from the submission, and from the document only where the
// submission is silent: the state the write leaves behind.
//
// UNANSWERED, NOT EMPTY. An empty gated secret is caught by [refuseEmpty];
// this adds the field being absent altogether.
func refuseUngated(values map[string]string, reqs []setup.Requirement) error {
	after := func(field string) string {
		if v, sent := values[field]; sent {
			return strings.TrimSpace(v)
		}
		for _, r := range reqs {
			if r.Field == field {
				return strings.TrimSpace(r.Stored)
			}
		}
		return ""
	}
	for _, r := range reqs {
		if r.RequiredWhen == nil ||
			after(r.RequiredWhen.Field) != r.RequiredWhen.Equals {
			continue
		}
		// ALREADY HELD IS ANSWERED. A company that supplied the token on
		// an earlier submission and is re-saving the same choice sends
		// nothing for it, and demanding it again would make the value
		// unre-submittable without retyping a credential nobody can read
		// back.
		if after(r.Field) != "" || r.Satisfied() {
			continue
		}
		return fmt.Errorf("%s is required by the answer this submission gives "+
			"for %s, and it is unset", r.Field, r.RequiredWhen.Field)
	}
	return nil
}

// auditSummary is the sentence stored on the revision.
//
// SERVER-GENERATED around the caller's own words, which are never trusted to
// be a sentence: the summary is stored on the revision, returned by
// GET /config/revisions and rendered on the Config screen.
//
// The caller's words are carried WHOLE, on one line. They used to be cut to
// 120 bytes for a row in the revision list, which is the renderer's to elide
// and already is for every summary PUT /config stores — so a connect's note
// was the one summary in the history whose end nobody could read.
func auditSummary(kind integration.Kind, supplied string) string {
	base := "connect " + string(kind)
	// Newlines out: the history renders a summary as one line, and the
	// revision list is not where a paragraph is read.
	supplied = strings.Join(strings.Fields(supplied), " ")
	if supplied == "" {
		return base
	}
	return base + ": " + supplied
}
