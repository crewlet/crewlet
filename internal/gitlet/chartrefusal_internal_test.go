package gitlet

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// A CHART'S REFUSAL OF WHOM A WRITE IS MADE FOR IS LOGGED AS THE RACE IT IS,
// AND EVERY OTHER AS A SENDER'S BUG (ADR-0109). Each decide that judges a
// seat's write on its epoch's chart — the committer's of a change's lifecycle
// and of a dequeue, the change writer's of a conversation — refuses from that
// chart alone a seat's carrier with no ceiling that the chart does not place or
// holds as an agent's, and a person's seat under that seat's ceiling: a chart
// that moved under the turn, or under its run, with no sender at fault, which
// the node logs at WARN, naming the seat. A carrier no chart makes right — an
// agent's write that states nothing, a credential with no ceiling, a
// colleague's ceiling beside another colleague, nobody's beside a seat — it
// logs at ERROR. Either is refused as no proposal a committer can decide, the
// class every refusal of the rule keeps, with no refusal of the answer's own.
//
// Mutations: a log site at ERROR whatever the cause — the change's, the
// conversation's or the dequeue's chart rows at ERROR, red; every refusal at
// WARN — the sender's rows at WARN, red; the chart's refusal of a person's seat
// under its ceiling left untyped — that row at ERROR, red.
func TestAChartsRefusalOfWhomAWriteIsForIsLoggedAsARaceAndEveryOtherAsABug(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	ctx := t.Context()
	dev := Principal{Kind: PrincipalAgent, Seat: "dev"}
	changeID, entryID := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	edit := func(c *WriteCeiling, f types.WrittenFor) ChangeProposal {
		return ChangeProposal{OpID: "op-edit", Asker: dev, Ceiling: c, WrittenFor: f,
			Edit: &ChangeEdit{RepoID: decideRepo, ChangeID: changeID}}
	}
	comment := func(c *WriteCeiling, f types.WrittenFor) ConversationWrite {
		return ConversationWrite{OpID: "op-comment", Asker: dev, Ceiling: c, WrittenFor: f,
			Comment: &Comment{RepoID: decideRepo, ChangeID: changeID}}
	}
	dequeue := func(c *WriteCeiling, f types.WrittenFor) QueueProposal {
		return QueueProposal{OpID: "op-dequeue", Asker: dev, Ceiling: c, WrittenFor: f,
			Dequeue: &Dequeue{RepoID: decideRepo, EntryID: entryID}}
	}
	// THE THREE DECIDES, each over e's rows — none read: whom the write is
	// made for is judged before anything is — and the log site of the node
	// that runs it, answering the refusal of its answer's own and its error.
	sites := []struct {
		event  string
		decide func(tx *sql.Tx, c *WriteCeiling, f types.WrittenFor) error
		answer func(logger *slog.Logger, c *WriteCeiling, f types.WrittenFor, err error) (string, error)
	}{
		{"code_change_invalid",
			func(tx *sql.Tx, c *WriteCeiling, f types.WrittenFor) error {
				_, err := decideChange(ctx, tx, edit(c, f), onTheChart, DefaultDeletedRetention)
				return err
			},
			func(logger *slog.Logger, c *WriteCeiling, f types.WrittenFor, err error) (string, error) {
				res, err := (&Committer{log: logger}).refusedChange(ctx, edit(c, f), err)
				return string(res.Refusal), err
			}},
		{"code_conversation_invalid",
			func(tx *sql.Tx, c *WriteCeiling, f types.WrittenFor) error {
				_, err := decideConversation(ctx, tx, comment(c, f), onTheChart, DefaultDeletedRetention, nil)
				return err
			},
			func(logger *slog.Logger, c *WriteCeiling, f types.WrittenFor, err error) (string, error) {
				res, err := (&ChangeWriter{log: logger}).refused(ctx, comment(c, f), err)
				return string(res.Refusal), err
			}},
		{"code_queue_invalid",
			func(tx *sql.Tx, c *WriteCeiling, f types.WrittenFor) error {
				_, err := decideQueue(ctx, tx, dequeue(c, f), onTheChart, DefaultLimits(), DefaultDeletedRetention,
					MaxQueueEntries, decideAt)
				return err
			},
			func(logger *slog.Logger, c *WriteCeiling, f types.WrittenFor, err error) (string, error) {
				res, err := (&Committer{log: logger}).refusedQueue(ctx, dequeue(c, f), err)
				return string(res.Refusal), err
			}},
	}
	seat := func(handle string) *WriteCeiling { return &WriteCeiling{Kind: WriteCeilingSeat, Seat: handle} }
	forSeat := func(handle string) types.WrittenFor {
		return types.ForTurn(&types.WorkingFor{Kind: types.WorkingForSeat, Seat: handle})
	}
	credential := types.ForTurn(&types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci",
		Apps: []iam.Grant{iam.GrantGitlet}})
	for _, tc := range []struct {
		name       string
		ceiling    *WriteCeiling
		writtenFor types.WrittenFor
		// level is what the refusal is logged at, and says what it says.
		level slog.Level
		says  string
	}{
		{"a seat the chart does not place, with no ceiling", nil, forSeat("ghost"), slog.LevelWarn,
			"this node's chart does not place seat ghost"},
		{"a seat the chart holds as an agent's, with no ceiling", nil, forSeat("reader"), slog.LevelWarn,
			"seat reader, which this node's chart holds as an agent's"},
		{"a person's seat under that seat's ceiling", seat("founder"), forSeat("founder"), slog.LevelWarn,
			"seat founder is a person's"},
		{"an agent's write that states nothing", nil, types.WrittenFor{}, slog.LevelError, "written_for"},
		{"a credential with no ceiling", nil, credential, slog.LevelError,
			"credential token:ci carries the ceiling of nobody"},
		{"a colleague's ceiling beside another colleague", seat("reader"), forSeat("vp-sales"), slog.LevelError,
			"held to seat reader's ceiling"},
		{"nobody's ceiling beside a seat", &WriteCeiling{Kind: WriteCeilingNobody}, forSeat("reader"),
			slog.LevelError, "held to the ceiling of nobody"},
	} {
		for _, s := range sites {
			var refused error
			if err := e.db.Read(ctx, func(tx *sql.Tx) error {
				refused = s.decide(tx, tc.ceiling, tc.writtenFor)
				return nil
			}); err != nil {
				t.Fatalf("read the rows: %v", err)
			}
			if !errors.Is(refused, ErrInvalidProposal) || !strings.Contains(refused.Error(), tc.says) {
				t.Errorf("%s: the decide of %s refused %v, want ErrInvalidProposal saying %q", s.event, tc.name,
					refused, tc.says)
				continue
			}
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			refusal, answered := s.answer(logger, tc.ceiling, tc.writtenFor, refused)
			if !errors.Is(answered, ErrInvalidProposal) || refusal != "" {
				t.Errorf("%s: %s was answered %q, %v, want ErrInvalidProposal and no refusal of the answer's own",
					s.event, tc.name, refusal, answered)
			}
			var line struct{ Level, Msg, Error string }
			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &line) != nil || line.Msg != s.event ||
				line.Level != tc.level.String() || !strings.Contains(line.Error, tc.says) {
				t.Errorf("%s was logged %q, want one %s line at %s saying %q", tc.name, logs.String(), s.event,
					tc.level, tc.says)
			}
		}
	}
}
