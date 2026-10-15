package session

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/runtoken"
)

// AN ATTRIBUTE THIS BUILD DOES NOT READ IS MALFORMED, EVEN SIGNED.
//
// A scope, at most once — what the engine writes. Every value here is signed
// under a key the signer holds, so the refusal is the attribute rule's and not
// the signature's: read loosely, a newer build's attribute would be read as
// nothing — which, for something that narrows a session, is reading it as
// everything. THE CONTROL is the two shapes this build writes, which parse.
func TestAttributesOutOfPlaceAreMalformedEvenSigned(t *testing.T) {
	t.Parallel()
	signer, err := New(Options{Material: runtoken.OneKey("k1", "key-material")})
	if err != nil {
		t.Fatal(err)
	}
	lineage := uuid.Must(uuid.NewV7())
	fixed := Bearer{
		KeyTag: signer.activeTag, Generation: 1, Lineage: lineage,
		Person: "token:ops", StartPosition: 9,
		AbsoluteExpiresAt: time.Unix(2_000_000_000, 0).UTC(),
		IdleExpiresAt:     time.Unix(2_000_000_000, 0).UTC(),
	}.payload()
	signed := func(attrs ...string) string {
		payload := strings.Join(append([]string{fixed}, attrs...), ".")
		return payload + "." + sign(signer.keys[signer.activeTag], payload)
	}
	for name, value := range map[string]string{
		"two scopes":           signed(scopeEnrolment, scopeEnrolment),
		"a word nobody wrote":  signed("admin"),
		"a scope, then a word": signed(scopeEnrolment, "admin"),
		"a word, then a scope": signed("admin", scopeEnrolment),
	} {
		if _, err := signer.parse(value); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s parsed: %v", name, err)
		}
	}
	for name, value := range map[string]string{
		"a scope":          signed(scopeEnrolment),
		"none (the whole)": signed(),
	} {
		if _, err := signer.parse(value); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
}
