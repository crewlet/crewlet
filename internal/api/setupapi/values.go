package setupapi

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/textcut"
)

// Reading the document a submission is measured against, and producing the
// values a person should not be asked to invent.

// decode reads a JSON body strictly, so a misspelled key is a refusal rather
// than a field silently ignored.
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
// crypto/rand's own text encoding: 26 base32 characters over 128 bits of
// entropy, URL-safe and safe to paste into a third-party app's header field, which is
// where every token this mints ends up. The same generator the provisioning
// passes already use, so a token minted from the dashboard and one minted
// from the CLI are the same kind of thing.
//
// NOT EVERY MINTABLE FIELD TAKES ONE. A field the third-party app accepts in
// exactly one form says so with [setup.Requirement.Shape], and this is what
// the rest get. Minting a token into GitLab's signing secret produced a value
// that could not be the HMAC key for any delivery, and every check that would
// have caught it is closed by construction: the value goes to the secret
// store and the document gets a `${VAR}`, which is the one thing config
// validation cannot check the shape of.
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

// maxSummary bounds a caller's own words in the stored sentence.
//
// A short phrase is all this field has ever been: it is rendered inline in the
// revision list beside the id and the actor, where a longer one would push
// both off the row.
const maxSummary = 120

// auditSummary is the sentence stored on the revision.
//
// SERVER-GENERATED, and a caller's own text is capped and never trusted to be
// a sentence: the summary is stored on the revision, returned by
// GET /config/revisions and rendered on the Config screen, so a caller that
// pasted a credential into it would put that credential on a screen.
func auditSummary(kind integration.Kind, supplied string) string {
	base := "connect " + string(kind)
	supplied = strings.TrimSpace(supplied)
	if supplied == "" {
		return base
	}
	// Newlines out, length capped. What survives is a short phrase, which
	// is all this field has ever been.
	supplied = strings.Join(strings.Fields(supplied), " ")
	// THROUGH textcut, which exists to remove exactly this. A raw
	// `supplied[:n]` cuts mid-rune whenever a multi-byte character straddles
	// the boundary, and what is left is invalid UTF-8: the JSON encoder
	// substitutes it, so the summary stored on the revision and rendered on
	// the Config screen ends in a replacement character.
	return base + ": " + textcut.Ellipsis(supplied, maxSummary)
}
