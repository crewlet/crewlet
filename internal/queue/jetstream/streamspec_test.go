package jetstream

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

// EVERY FIELD OF streamSpec IS CLASSIFIED, by reflection over the struct
// rather than against a list somebody maintains.
//
// A hand-written list is what this replaces, and it had two holes: `retention`
// and `subjects` were both missing, and both are fields whose difference
// changes what every message on the stream means. A list cannot notice a field
// that was added and not classified; this can.
//
// Mutation: add a field to streamSpec and leave classifyStreamSpec alone —
// red, naming the field.
func TestEveryStreamSpecFieldIsClassified(t *testing.T) {
	t.Parallel()
	classes := classifyStreamSpec()
	specType := reflect.TypeOf(streamSpec{})

	var unclassified []string
	for i := range specType.NumField() {
		name := specType.Field(i).Name
		if _, ok := classes[name]; !ok {
			unclassified = append(unclassified, name)
		}
	}
	if len(unclassified) > 0 {
		t.Errorf("streamSpec field(s) %s carry no class: a boot compares a "+
			"running stream against this spec field by field, and a field "+
			"with no class silently joins whichever class its default "+
			"happens to match — which is how `retention` and `subjects` "+
			"were both missing from the list this replaces",
			strings.Join(unclassified, ", "))
	}

	// AND NOTHING IS CLASSIFIED THAT IS NOT A FIELD, which is the other
	// half of exhaustive: a renamed field leaves its old name behind in
	// the map, still looking classified.
	fields := map[string]bool{}
	for i := range specType.NumField() {
		fields[specType.Field(i).Name] = true
	}
	for name := range classes {
		if !fields[name] {
			t.Errorf("classifyStreamSpec names %q, which is not a streamSpec "+
				"field: a class left behind by a rename reads as coverage "+
				"the struct does not have", name)
		}
	}
}

// EXACTLY ONE FIELD IS IDENTITY, because the comparison's whole shape depends
// on it: identity is what decides WHICH stream is being compared, so a second
// identity field would make "the same stream" ambiguous.
func TestExactlyOneStreamSpecFieldIsIdentity(t *testing.T) {
	t.Parallel()
	var identity []string
	for name, class := range classifyStreamSpec() {
		if class == classIdentity {
			identity = append(identity, name)
		}
	}
	if len(identity) != 1 || identity[0] != "name" {
		t.Errorf("identity fields = %v, want exactly [name]", identity)
	}
}

// A DOMAIN CANNOT WEAKEN THE SAFETY FIELDS. DomainStream declares four things
// and the rest are the framework's, identical for every domain — because they
// are the fields that decide whether an ordered log is an ordered log.
//
// Mutation: let a domain set discard and one could build a log that drops its
// oldest record to accept a new one, which is the state loss the whole design
// is against.
func TestADomainStreamCannotWeakenTheSafetyFields(t *testing.T) {
	t.Parallel()
	got := DomainStream{Name: "CREWLET_PROBE_LOG", Subjects: []string{"p.>"}}.spec()

	if got.discard != jetstream.DiscardNew {
		t.Errorf("discard = %v, want DiscardNew: a full log refuses the append, "+
			"it does not drop the company's oldest record to accept a new one",
			got.discard)
	}
	if !got.denyDelete {
		t.Error("denyDelete is false: a plain stream is deletable by any client " +
			"holding the API unless it says otherwise, and a KV bucket gets " +
			"this for free where a stream does not")
	}
	if got.allowRollup {
		t.Error("allowRollup is true: a rollup is a delete of everything below " +
			"it, spelled as a publish")
	}
	if got.allowDirect || got.mirrorDirect {
		t.Error("a direct get is allowed: it is served from replica state that " +
			"may be behind an acknowledged write, so the last-message " +
			"discriminator would answer \"no message here\" for a message the " +
			"quorum has")
	}
	if got.maxAge != 0 {
		t.Errorf("maxAge = %v, want none: an age bound deletes records a node "+
			"has not applied", got.maxAge)
	}
	if got.maxPerSubject != 0 {
		t.Errorf("maxPerSubject = %d, want unlimited: one message per subject "+
			"is a keyed table, and this is a log", got.maxPerSubject)
	}
}
