package doclet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/iam"
)

// A CREDENTIAL'S WRITE THAT WAKES SOMEBODY CARRIES THE APPS IT USED HERE, AND
// NOTHING ELSE DOES (ADR-0105), on the knowledge base's store as on the
// tracker's. A pipeline's token commenting on a page records, beside its login,
// the apps the node that took the write resolved for it — a turn the comment's
// notice wakes is bounded by them — at the version that carries them. Its
// eviction of a node wakes nobody and is a gate every build there will ever be
// must read, so it carries none and is not refused; a person's comment carries
// none even when handed some, and both stay at the base version.
//
// Mutation: leave the apps off the record the store builds — the token's
// comment carries none; keep them on a record that wakes nobody — the token's
// eviction is refused for carrying them; keep them for every kind — the
// person's comment carries Doclet.
func TestACredentialsPageCarriesTheAppsItUsedHere(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	doclets := []iam.Grant{iam.GrantDoclet}
	token := doclet.Actor{Handle: "token:ci", Kind: doclet.AuthorOperator, OperatorID: "token:ci",
		Apps: doclets}
	person := author("jane")
	person.Apps = doclets
	page := r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "prose"})
	for _, c := range []struct {
		actor   doclet.Actor
		want    []iam.Grant
		version int
	}{
		{token, doclets, 2},
		{person, nil, 1},
	} {
		if _, _, err := r.store.Comment(t.Context(), c.actor, page.Page.ID,
			doclet.NewComment{Body: "@bob is this current?", Mentions: []string{"bob"}}); err != nil {
			t.Fatalf("%s comments: %v", c.actor.Handle, err)
		}
		record, err := doclet.Decode(r.lastRecord())
		if err != nil {
			t.Fatalf("decode %s's record: %v", c.actor.Handle, err)
		}
		if record.Notify == nil || !slices.Equal(record.ActorApps, c.want) || record.V != c.version {
			t.Errorf("%s's comment is recorded waking %v carrying %v at version %d, want a "+
				"notice carrying %v at %d", c.actor.Handle, record.Notify, record.ActorApps,
				record.V, c.want, c.version)
		}
	}

	if _, err := r.store.EvictNode(t.Context(), token, "op-evict", "node-b"); err != nil {
		t.Fatalf("the token's eviction was refused: %v", err)
	}
	record, err := doclet.Decode(r.lastRecord())
	if err != nil {
		t.Fatalf("decode the eviction: %v", err)
	}
	if record.Op != doclet.OpEviction || len(record.ActorApps) != 0 || record.V != doclet.GateRecordVersion {
		t.Errorf("the token's eviction is %s carrying %v at version %d, want a gate carrying no "+
			"apps at %d", record.Op, record.ActorApps, record.V, doclet.GateRecordVersion)
	}
}
