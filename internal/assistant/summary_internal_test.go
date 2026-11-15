package assistant

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
)

// A summary stands in for the messages it covers; the window still shows the
// last [HistoryMessages] of the rest, and any older ones are counted.
func TestASummaryStandsInForTheMessagesItCovers(t *testing.T) {
	t.Parallel()
	in := basePrompt()
	for i := range 25 {
		in.History = append(in.History, historyEntry{Role: wire.RoleUser, Text: fmt.Sprintf("message %d.", i)})
	}
	in.Summary = &historySummary{Text: "They chose a Worker route.", Through: 10}
	msgs := buildMessages(in)
	if !strings.Contains(msgs[1].Content, "first 10 messages") ||
		!strings.Contains(msgs[1].Content, "They chose a Worker route.") {
		t.Fatalf("the summary does not lead the history: %q", msgs[1].Content)
	}
	if !strings.Contains(msgs[2].Content, "3 earlier messages") {
		t.Errorf("the uncovered messages past the window are not counted: %q", msgs[2].Content)
	}
	all := ""
	for _, m := range msgs {
		all += m.Content + "\n"
	}
	for _, gone := range []string{"message 0.", "message 9.", "message 12."} {
		if strings.Contains(all, gone) {
			t.Errorf("%q is in the prompt beside the summary", gone)
		}
	}
	if !strings.Contains(all, "message 13.") || !strings.Contains(all, "message 24.") {
		t.Error("the window's messages are missing")
	}
}

// A summary is given to a reader only while they hold every app its messages
// read through, as a message is; one this build cannot read is none.
func TestASummaryIsOnlyGivenToAReaderWhoHoldsItsApps(t *testing.T) {
	t.Parallel()
	tr := assistantdomain.Transcript{Messages: make([]assistantdomain.Message, 12),
		Summary: `{"text":"Spend fell.","apps":["admin"]}`, SummaryThrough: 10}
	admin := iam.Principal{Kind: iam.KindPerson, Grants: []iam.Grant{iam.GrantAdmin}}
	member := iam.Principal{Kind: iam.KindPerson, Grants: []iam.Grant{iam.GrantWorklet}}
	if s := summaryOf(tr, admin); s == nil || s.Text != "Spend fell." || s.Through != 10 {
		t.Errorf("summaryOf(admin) = %+v", s)
	}
	if s := summaryOf(tr, member); s != nil {
		t.Errorf("a reader without admin was given %+v", s)
	}
	for name, bad := range map[string]assistantdomain.Transcript{
		"not JSON":       {Messages: tr.Messages, Summary: "plain", SummaryThrough: 10},
		"past the end":   {Messages: tr.Messages, Summary: tr.Summary, SummaryThrough: 13},
		"covers nothing": {Messages: tr.Messages, Summary: tr.Summary},
	} {
		if s := summaryOf(bad, admin); s != nil {
			t.Errorf("%s: summaryOf = %+v", name, s)
		}
	}
}
