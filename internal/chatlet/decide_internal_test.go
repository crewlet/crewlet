package chatlet

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE OP TABLE IS THE GATE, NOT A DESCRIPTION OF ONE.
//
// Every gesture picks its own subject constructor and the applier dispatches on
// (kind, op) on its own, so the only thing that makes the two agree is [decide]
// refusing a record formed on a kind [SubjectKind] does not place its op on. A
// post on a room's subject would otherwise publish cleanly and fault the
// domain on every node at apply, over a pair nobody meant to write.
func TestDecideRefusesAnOpOnASubjectTheTableDoesNotPlaceItOn(t *testing.T) {
	t.Parallel()
	stamp := statelog.Stamp{Gen: 1, Writer: "node-a"}
	actor := Actor{Handle: "eng", Kind: AuthorAgent}
	at := time.Unix(1_700_000_000, 0).UTC()
	room := uuid.NewString()
	post := MessagePost{V: 1, MessageID: uuid.NewString(), Body: "hello",
		Author: "eng", AuthorKind: AuthorAgent}
	create := ChannelCreate{V: 1, ChannelID: room, Kind: KindPublic, Name: "launch",
		Members: []Member{{Handle: "eng"}}, CreatedBy: "eng", CreatedByKind: AuthorAgent}

	for name, tc := range map[string]struct {
		subject Subject
		op      OpKind
		payload Payload
		refused bool
	}{
		"a post on its room's message subject": {
			subject: MessageSubject(room), op: OpPost, payload: post,
		},
		"a post on the room's own subject": {
			subject: ChannelSubject(room), op: OpPost, payload: post, refused: true,
		},
		"a named room's create on its own id rather than its name": {
			subject: ChannelSubject(room), op: OpCreate, payload: create, refused: true,
		},
		"a named room's create on its name": {
			subject: ChannelNameSubject("launch"), op: OpCreate, payload: create,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := decide(stamp, actor, tc.subject, tc.op,
				ScopeSet{Subject: true}, statelog.NewOpID(at, "decide"),
				tc.payload, nil, at)
			switch {
			case tc.refused && err == nil:
				t.Fatalf("a %s record on a %s subject was formed — it would "+
					"publish and fault every node's applier", tc.op, tc.subject.Kind)
			case tc.refused && !strings.Contains(err.Error(), "op table"):
				t.Fatalf("refused for another reason than the op table: %v", err)
			case !tc.refused && err != nil:
				t.Fatalf("the pair the op table places was refused: %v", err)
			}
		})
	}
}
