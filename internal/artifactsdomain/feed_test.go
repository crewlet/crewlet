package artifactsdomain_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/commentanchor"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
)

// commentRecord is a person's comment record on a thread of an artifact,
// carrying notify, as the log holds it.
func commentRecord(t *testing.T, op artifactsdomain.OpKind, payload any) (changefeed.Record, string) {
	t.Helper()
	artifact := artifacts.IDFor("launch deck")
	thread := uuid.NewSHA1(uuid.NameSpaceOID, []byte("thread"))
	subject := artifactsdomain.CommentSubject(artifact, thread)
	raw := handRecord(t, subject, op, "op-"+string(op), artifactsdomain.ScopeSet{Subject: true}, payload)
	return changefeed.Record{Payload: raw, Key: subject.String()}, artifact.String()
}

// snapshot wakes pm, addressed as the owner, and dev as an assignee, and
// tells jane; what it says of where the comment was made is the deck's.
func snapshot() *artifactsdomain.CommentNotify {
	return &artifactsdomain.CommentNotify{
		Agents: []artifactsdomain.Recipient{
			{Seat: "pm", Reason: artifactsdomain.NotifyOwner, Addressed: true},
			{Seat: "dev", Reason: artifactsdomain.NotifyAssignee},
		},
		People: []artifactsdomain.Recipient{{Seat: "jane", Reason: artifactsdomain.NotifyParticipant}},
		About: &artifactsdomain.CommentAbout{Title: "Launch deck", Kind: artifacts.KindSlides, Head: 4,
			Version: 3, Label: "Slide 2: “Results”", Quote: "Revenue grew 40%"},
	}
}

// A comment's wake is derived from its record (ADR-0012): only a snapshot naming
// an agent is relayed, and each agent is woken under its reason and addressing.
// Mutation: relay a record that wakes nobody, or drop the addressing.
func TestACommentWakesTheAgentsItsRecordNames(t *testing.T) {
	t.Parallel()
	rec, artifact := commentRecord(t, artifactsdomain.OpCommentReply, artifactsdomain.CommentReplyPayload{
		V: 1, Remark: uuid.NewString(), Body: "Say by how much.", MaxRemarks: artifacts.MaxRemarksPerThread,
		Notify: snapshot()})
	tr := artifactsdomain.NewTranslator()
	delivery, wakes, err := tr.Translate(t.Context(), rec)
	if err != nil || !wakes {
		t.Fatalf("a reply waking two agents was relayed %v (%v)", wakes, err)
	}
	if src := tr.Source(); src.Name != artifactsdomain.Source || src.Group != artifactsdomain.FeedGroup {
		t.Errorf("the feed runs as %+v", src)
	}
	routed, err := artifactsdomain.NewParser(artifactsdomain.ParserOptions{}).Parse(t.Context(),
		types.RawWebhook{Body: delivery.Body}, nil)
	if err != nil || len(routed) != 2 {
		t.Fatalf("the parser routed %+v (%v), want pm and dev", routed, err)
	}
	pm := routed[0]
	if pm.To.Handle != "pm" || pm.Metadata[artifactsdomain.MetaAddressed] != "true" ||
		pm.Metadata[artifactsdomain.MetaArtifact] != artifact || pm.Body != "Say by how much." ||
		pm.Metadata[notify.BodySourceField] != string(provenance.SourceArtifactComment) ||
		pm.Metadata[notify.BodyAuthorField] != "jane" || pm.Subject != "Launch deck" {
		t.Errorf("pm's wake is %+v", pm)
	}
	if routed[1].Metadata[artifactsdomain.MetaAddressed] != "" {
		t.Errorf("the assignee's wake is addressed: %+v", routed[1].Metadata)
	}
	if pm.WakeID == routed[1].WakeID || pm.WakeID != changefeed.WakeID("op-comment_reply", "pm") {
		t.Errorf("the wake ids are not derived per recipient from the record")
	}

	// A RECORD TELLING ONLY PEOPLE, AN EDIT, AND A RECORD OF ANOTHER KIND RELAY NOTHING.
	people := snapshot()
	people.Agents = nil
	for name, rec := range map[string]changefeed.Record{
		"people only": first(commentRecord(t, artifactsdomain.OpCommentReply, artifactsdomain.CommentReplyPayload{
			V: 1, Remark: uuid.NewString(), Body: "x", MaxRemarks: 1, Notify: people})),
		"an edit": first(commentRecord(t, artifactsdomain.OpCommentEdit, artifactsdomain.CommentEditPayload{
			V: 1, Remark: uuid.NewString(), Body: "y"})),
	} {
		if _, wakes, err := tr.Translate(t.Context(), rec); wakes || err != nil {
			t.Errorf("%s was relayed %v (%v)", name, wakes, err)
		}
	}
}

func first(rec changefeed.Record, _ string) changefeed.Record { return rec }

// A comment's prompt says who, where and how to answer, the quote fenced as the
// page's words; a burst on one artifact is one partition and one conversation.
func TestACommentsPromptSaysWhereItWasMadeAndHowToAnswer(t *testing.T) {
	t.Parallel()
	rec, artifact := commentRecord(t, artifactsdomain.OpCommentOpen, artifactsdomain.CommentOpenPayload{
		V: 1, Remark: uuid.NewString(), Body: "Too dark.", Version: 3, Anchor: commentanchor.Whole(),
		MaxThreads: artifacts.MaxThreadsPerArtifact, Notify: snapshot()})
	delivery, _, err := artifactsdomain.NewTranslator().Translate(t.Context(), rec)
	if err != nil {
		t.Fatal(err)
	}
	routed, err := artifactsdomain.NewParser(artifactsdomain.ParserOptions{}).Parse(t.Context(),
		types.RawWebhook{Body: delivery.Body}, nil)
	if err != nil || len(routed) == 0 {
		t.Fatalf("routed %v (%v)", routed, err)
	}
	in := routed[0].Inbound
	in.Fence = provenance.NewBoundary()
	p := artifactsdomain.Prompt{}
	text := notify.Render(p, in, nil)
	for _, want := range []string{"Launch deck (slides)", "**Thread made on version:** 3",
		"**Current version:** 4", "Slide 2: “Results”", "source=artifact_file",
		"list_artifact_comments", "publish_artifact", "`base_version: 4`", "resolve_artifact_comment",
		"comment_on_artifact", "source=artifact_comment"} {
		if !strings.Contains(text, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, text)
		}
	}
	if !p.Addressed(in) || p.PartitionKey(in.Metadata, in.Subject) != "artifact:"+artifact ||
		p.ConversationIdentity(in.Metadata, in.Subject) != "artifact:"+artifact {
		t.Errorf("the owner's wake is not addressed or not partitioned by its artifact")
	}
	if _, err := json.Marshal(in.Writer); err != nil || in.Writer == nil || in.Writer.Seat != "jane" {
		t.Errorf("the wake's writer is %+v", in.Writer)
	}
}
