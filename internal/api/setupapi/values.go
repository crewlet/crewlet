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
	for _, field := range generate {
		r, ok := byField[field]
		if !ok {
			return fmt.Errorf("no field %q to generate", field)
		}
		if !r.Mintable {
			return fmt.Errorf(
				"%s is not something this engine can generate; it comes from the vendor", field)
		}
		if _, supplied := values[field]; supplied {
			return fmt.Errorf(
				"%s was both supplied and asked to be generated; send one or the other", field)
		}
		values[field] = mint()
	}
	return nil
}

// mint produces a shared token.
//
// crypto/rand's own text encoding: 26 base32 characters over 128 bits of
// entropy, URL-safe and safe to paste into a vendor's header field, which is
// where every token this mints ends up. The same generator the provisioning
// passes already use, so a token minted from the dashboard and one minted
// from the CLI are the same kind of thing.
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
		if byField[field].Required {
			return fmt.Errorf(
				"%s is required and the submission clears it; omit the field to leave it alone",
				field)
		}
	}
	return nil
}

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
	const maxSummary = 120
	if len(supplied) > maxSummary {
		supplied = supplied[:maxSummary]
	}
	return base + ": " + supplied
}
