package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
)

// AN ITEM'S IDENTITY NAMES ITS TRACKER, because the bare id does not.
//
// A native task id and a Jira issue id are drawn from different spaces, and
// nothing stops the same string turning up in both. Everything that decides
// whether two turns were on one item compares the ref, so a ref that dropped
// the backend would merge two different items' spend into one row.
func TestAWorkItemRefIsBackendQualified(t *testing.T) {
	t.Parallel()
	native := WorkItem{Backend: WorkNative, ID: "10042", Key: "ENG-1", Project: "ENG"}
	jira := WorkItem{Backend: WorkJira, ID: "10042", Key: "OPS-9", Project: "OPS"}

	if got := native.Ref(); got != "native:10042" {
		t.Errorf("the native item's ref is %q, want native:10042", got)
	}
	if native.Ref() == jira.Ref() {
		t.Errorf("two items on different trackers sharing an id have one ref, %q", native.Ref())
	}
	// THE KEY IS A LABEL, not the identity: a project rename changes it, and
	// the same item must still be the same item afterwards.
	renamed := native
	renamed.Key, renamed.Project = "CORE-1", "CORE"
	if renamed.Ref() != native.Ref() {
		t.Errorf("renaming the item's project moved its ref from %q to %q",
			native.Ref(), renamed.Ref())
	}
}

// A BACKEND OR A BASIS THIS BUILD DOES NOT KNOW IS A VALUE, not a panic and
// not a failed decode.
//
// A newer node can name an item in a tracker this build has never heard of,
// and a rolling upgrade puts that event in front of this build's readers. The
// event has to decode, re-publish unchanged, and answer "not valid" to a
// reader that asks — which is what lets a screen render it as an unknown
// source instead of dropping the turn.
func TestAnUnknownWorkBackendIsAValueNotAPanic(t *testing.T) {
	t.Parallel()
	for _, known := range []WorkBackend{WorkNative, WorkJira, WorkGitHub, WorkGitLab} {
		if !known.Valid() {
			t.Errorf("%q is not valid", known)
		}
	}
	for _, known := range []WorkItemBasis{BasisTrigger, BasisAskedBy, BasisResume, BasisSoleWrite} {
		if !known.Valid() {
			t.Errorf("basis %q is not valid", known)
		}
	}
	if WorkBackend("").Valid() || WorkItemBasis("").Valid() {
		t.Error("the empty value is valid, so an absent field would read as a real one")
	}

	const raw = `{"id":"6f1c3d2e-0000-4000-8000-000000000001","type":"agent_turn_started",` +
		`"timestamp":"2026-09-24T09:00:00Z","source":"engine","agent_id":"a","role":"CTO",` +
		`"turn_id":"run-1","work_item":{"backend":"linear","id":"LIN-7","key":"LIN-7","project":"core"},` +
		`"work_item_basis":"telepathy","started_at":"2026-09-24T09:00:00Z"}`
	var ev events.Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("an unknown backend failed the decode: %v", err)
	}
	started, ok := events.DataAs[*AgentTurnStarted](&ev)
	if !ok {
		t.Fatalf("the event decoded as %T, not a turn start", ev.Data)
	}
	if started.WorkItem == nil || started.WorkItem.Backend != "linear" || started.WorkItem.Backend.Valid() {
		t.Errorf("the item arrived as %+v, want backend \"linear\", carried and not valid",
			started.WorkItem)
	}
	if started.WorkItemBasis != "telepathy" || started.WorkItemBasis.Valid() {
		t.Errorf("the basis arrived as %q, want \"telepathy\", carried and not valid",
			started.WorkItemBasis)
	}
	out, err := json.Marshal(&ev)
	if err != nil {
		t.Fatalf("re-publish: %v", err)
	}
	for _, want := range []string{`"backend":"linear"`, `"work_item_basis":"telepathy"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the re-published event lost %s\n%s", want, out)
		}
	}
}

// A TURN START WITHOUT ITS NEWEST FIELDS DECODES AS A TURN ON NOTHING, and
// stays that way through a round trip.
//
// The work item is resolved by rules that land after the event does, and an
// unattributed turn — a chat wake — is the ordinary case besides. Either way a
// reader tells "on no item" by the MISSING key, so a decode that invented an
// empty item, or a re-encode that wrote `"work_item":null`, would turn every
// such turn into one on an item with no identity.
func TestAgentTurnStartedRoundTripsWithoutItsNewestFields(t *testing.T) {
	t.Parallel()
	const raw = `{"id":"6f1c3d2e-0000-4000-8000-000000000002","type":"agent_turn_started",` +
		`"timestamp":"2026-09-24T09:00:00Z","source":"engine","agent_id":"a",` +
		`"agent_handle":"cto","role":"CTO","turn_id":"run-1","work_key":"wk-1",` +
		`"trigger":{"id":"t","type":"external_notification","summary":"s","actor":"x",` +
		`"timestamp":"2026-09-24T08:59:59Z"},"conversation_key":"slack:C1",` +
		`"started_at":"2026-09-24T09:00:00Z","resumed":false}`
	var ev events.Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	started, ok := events.DataAs[*AgentTurnStarted](&ev)
	if !ok {
		t.Fatalf("the event decoded as %T, not a turn start", ev.Data)
	}
	if started.WorkItem != nil || started.WorkItemBasis != "" {
		t.Errorf("a start naming no item decoded as on %+v by %q", started.WorkItem,
			started.WorkItemBasis)
	}
	if started.TurnID != "run-1" || started.ConversationKey != "slack:C1" {
		t.Errorf("the fields it did carry arrived as %+v", *started)
	}
	if len(ev.Extra) != 0 {
		t.Errorf("a known type left %v in Extra", ev.Extra)
	}

	out, err := json.Marshal(&ev)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(out, &keys); err != nil {
		t.Fatalf("remap: %v", err)
	}
	if item, present := keys["work_item"]; present {
		t.Errorf("a turn on no item re-encoded with work_item %s — absent is how "+
			"every reader tells an unattributed turn", item)
	}
}
