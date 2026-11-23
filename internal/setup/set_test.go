package setup_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// keyList is a set of two entries, two fields each, at least one entry.
func keyList() []setup.Requirement {
	var out []setup.Requirement
	for _, role := range []string{"marketing", "admin"} {
		set := &setup.Set{ID: "main_keys", Label: "API keys", Item: "key", Add: "Add a key", Pick: "Role",
			Hint: vendortext.Plain("A key ID and .p8 file per role: at least one."), Min: 1,
			Entry: setup.Choice{Value: role, Label: strings.ToUpper(role[:1]) + role[1:]}}
		base := "integrations.app_store_connect.connections.main.keys." + role
		out = append(out,
			setup.Requirement{Field: role + "_key_id", Label: "Key ID", Kind: setup.KindID,
				ConfigPath: base + ".key_id", Set: set},
			setup.Requirement{Field: role + "_private_key", Label: "Private key (.p8)", Kind: setup.KindSecret,
				ConfigPath: base + ".private_key", SecretName: strings.ToUpper(role) + "_KEY", Set: set})
	}
	return out
}

// AT LEAST ONE ENTRY, EACH WHOLE, said in the operator's words at a field: no
// entry is the set's own sentence, a started entry missing a field names the
// entry and the field, and one whole entry is enough.
//
// Mutation: count fields rather than entries, or require every entry, and a
// key ID alone passes or one whole key is refused.
func TestASetNeedsItsMinimumOfWholeEntries(t *testing.T) {
	t.Parallel()
	reqs := keyList()
	short := setup.CheckSets(reqs, nil)
	want := "Add a key to continue. A key ID and .p8 file per role: at least one."
	if short == nil || short.Problem != want || short.Field != "marketing_key_id" {
		t.Fatalf("an empty set answered %+v, want its control and its hint at its first field", short)
	}
	half := setup.CheckSets(reqs, map[string]string{"admin_key_id": "ABC123DEFG"})
	if half == nil || half.Field != "admin_private_key" || half.Problem != "The Admin key needs its private key (.p8) too." {
		t.Fatalf("a started entry with a field empty answered %+v", half)
	}
	if strings.Contains(half.Problem, "integrations.") || strings.Contains(half.Problem, "private_key") {
		t.Errorf("the refusal %q speaks in config paths", half.Problem)
	}
	whole := map[string]string{"admin_key_id": "ABC123DEFG", "admin_private_key": "pem"}
	if got := setup.CheckSets(reqs, whole); got != nil {
		t.Errorf("one whole entry was refused: %+v", got)
	}
	// A FIELD THE DOCUMENT HOLDS counts as answered: a re-save sends no key.
	reqs[3].Stored, reqs[3].Present = "${ADMIN_KEY}", true
	if got := setup.CheckSets(reqs, map[string]string{"admin_key_id": "ABC123DEFG"}); got != nil {
		t.Errorf("an entry whose key the document holds was refused: %+v", got)
	}
}

// A SET'S FIELD IS NEEDED ONCE ITS ENTRY IS STARTED, never by Required, and a
// set below its minimum is outstanding, so a pass never runs on no key.
func TestASetIsOutstandingUntilItHasAnEntry(t *testing.T) {
	t.Parallel()
	reqs := keyList()
	for _, r := range reqs {
		if r.Needed(reqs) {
			t.Errorf("%s is needed before any entry is started", r.Field)
		}
	}
	if out := setup.Outstanding(reqs); len(out) != 1 || out[0].Field != "marketing_key_id" {
		t.Errorf("an empty set leaves %v outstanding, want its first field", fieldsOf(out))
	}
	reqs[0].Stored, reqs[0].Present = "ABC123DEFG", true
	if !reqs[1].Needed(reqs) || reqs[2].Needed(reqs) {
		t.Error("a started entry's other field is not needed, or another entry's is")
	}
	if out := setup.Outstanding(reqs); len(out) != 1 || out[0].Field != "marketing_private_key" {
		t.Errorf("a half entry leaves %v outstanding, want its key", fieldsOf(out))
	}
}

// A SUBMISSION LEAVING A SET SHORT IS REFUSED BEFORE ANYTHING IS SEALED, at a
// field of the set, where config would refuse it after the seal by its path.
func TestASubmissionLeavingASetShortSealsNothing(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	_, err := writer(rec).Write(t.Context(), keyList(), setup.Submission{
		Kind: integration.KindAppStoreConnect, Values: map[string]string{"admin_private_key": "pem"},
		Summary: "connect", By: founder,
	})
	var value *setup.ValueError
	if !errors.As(err, &value) || value.Field != "admin_key_id" {
		t.Fatalf("Write = %v, want a refusal at the Admin key's ID", err)
	}
	if len(rec.secrets) != 0 || len(rec.events) != 0 {
		t.Errorf("a refused set sealed %v and wrote %v", rec.secrets, rec.events)
	}
}

func fieldsOf(reqs []setup.Requirement) []string {
	out := []string{}
	for _, r := range reqs {
		out = append(out, r.Field)
	}
	return out
}
