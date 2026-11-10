package worklet

import (
	"reflect"
	"testing"
)

// NO REASON REACHES A CREDENTIAL. Every handle a snapshot can offer names one,
// and on every kind of change the record the writer leaves holds none among
// its candidates; the snapshot's other handles still hear.
//
// Mutation: leave any recipient field out of the filter, and the kind that
// reads it goes red.
func TestNoReasonReachesACredential(t *testing.T) {
	t.Parallel()
	const token = "token:ci"
	full := &Notify{Mentions: []string{token, "dee"}, Snapshot: Snapshot{
		Key: "ENG-1", Assignee: token, Reporter: token, PrevAssignee: token, CommentAsk: token,
		AnsweredAuthor: token, ParentAssignee: token, RoutedTo: token,
		Watchers: []string{token, "cy"}, Collaborators: []string{token}, ThreadParticipants: []string{token},
		ThreadOwing: []string{token}, ThreadAwaiting: []string{token}, RemovedWatchers: []string{token},
		ChecklistAssignees: []string{token},
		Unblocked:          []TaskParty{{Task: "t-2", Assignee: token}},
		Dependents:         []TaskParty{{Task: "t-3", Assignee: token}},
	}}
	// Every string field of the snapshot either names a handle the filter
	// clears or is one this list says is no recipient.
	notRecipients := map[string]bool{"Key": true, "Project": true, "Title": true, "Status": true,
		"StatusGroup": true, "PrevStatusGroup": true, "CommentAuthorKind": true, "ProjectLead": true,
		"RoutingUnitLead": true, "Person": true, "PrioritisedBy": true, "Task": true}
	cleared := full.withoutCredentials()
	v := reflect.ValueOf(cleared.Snapshot)
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		if f := v.Field(i); f.Kind() == reflect.String && f.String() == token && !notRecipients[name] {
			t.Errorf("Snapshot.%s still names the credential", name)
		}
	}
	for _, kind := range ChangeKinds {
		n := *cleared
		n.Kind = kind
		for _, c := range Candidates(&n) {
			if isCredential(c.Handle) {
				t.Errorf("a %s change offers the credential under %q", kind, c.Reason)
			}
		}
	}
	if full.Snapshot.Reporter != token {
		t.Error("the filter changed the notice it was handed")
	}
}
