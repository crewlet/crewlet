package session

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/runtoken"
)

// AN ATTRIBUTE IN A PLACE THIS BUILD DOES NOT READ IT IS MALFORMED, EVEN
// SIGNED.
//
// A scope then a binding, each at most once and in that order — the order the
// engine writes them. Every value here is signed under a key the signer holds,
// so the refusal is the attribute rule's and not the signature's: read
// loosely, a binding could sit where nothing reads it, and a newer build's
// attribute would be read as nothing — which, for something that narrows a
// session, is reading it as everything. THE CONTROL is the two orders this
// build writes, which parse.
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
	binding := bindingPrefix + "c29tZS1tYWM"
	for name, value := range map[string]string{
		"a binding before a scope": signed(binding, scopeEnrolment),
		"two bindings":             signed(binding, binding),
		"two scopes":               signed(scopeEnrolment, scopeEnrolment),
		"an empty binding":         signed(bindingPrefix),
		"a word nobody wrote":      signed("admin"),
		"three attributes":         signed(scopeEnrolment, binding, binding),
	} {
		if _, err := signer.parse(value); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s parsed: %v", name, err)
		}
	}
	for name, value := range map[string]string{
		"a scope":             signed(scopeEnrolment),
		"a binding":           signed(binding),
		"a scope, a binding":  signed(scopeEnrolment, binding),
		"neither (the whole)": signed(),
	} {
		if _, err := signer.parse(value); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
}
