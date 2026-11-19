package chatlet_test

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// TestTheDashboardsChatLimitsAreTheEngines holds the bounds the chat screens
// enforce against the ones this package actually refuses on.
//
// THE DASHBOARD CANNOT IMPORT A GO CONSTANT, so it carries its own copy in
// `dashboard/src/contract/chat.ts` (see [clientsource]), which drifts SILENTLY
// IN BOTH DIRECTIONS:
//
//   - too LOW greys out `send` on a message the engine would take, unlogged.
//   - too HIGH accepts what the write path then rejects, after the typing.
//
// The engine owns the value. The link previews' bound is not one of these: it
// is MAX_IN_FLIGHT_QUERIES, held by `internal/api/stream`.
func TestTheDashboardsChatLimitsAreTheEngines(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for name, engine := range map[string]int{
		"MAX_CHANNEL_NAME":        chatlet.MaxChannelName,
		"MAX_BODY_BYTES":          chatlet.MaxBody,
		"MAX_TOPIC_CHARS":         chatlet.MaxTopic,
		"MAX_PURPOSE_CHARS":       chatlet.MaxPurpose,
		"MAX_DIRECT_PARTICIPANTS": chatlet.MaxDMParticipants,
		"MAX_MESSAGE_LINKS":       chatlet.MaxLinks,
		"MAX_READ_PAGE":           chatlet.MaxLimit,
	} {
		raw, err := clientsource.Scalar(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		// THE NUMBER THE LITERAL SPELLS, separators and radix prefixes
		// included, so a change that altered only the spelling is not a
		// drift.
		client, err := strconv.ParseInt(raw, 0, 64)
		if err != nil {
			t.Fatalf("%s is %q, which is not an integer: %v", name, raw, err)
		}
		if int(client) != engine {
			t.Errorf("the dashboard enforces %s = %d and this package refuses "+
				"at %d. Too low silently greys out a gesture the engine would "+
				"take; too high moves the refusal from the form to the write "+
				"path. The engine owns the value, so contract/chat.ts is what "+
				"changes", name, client, engine)
		}
	}
}

// TestTheDashboardsChannelNameGrammarIsTheEngines holds the room-name rule the
// create dialog refuses on against [chatlet.ValidName].
//
// THE PATTERN IS THE FRAGILE HALF: a class drifts for one input in a thousand.
// A client STRICTER than the engine makes a legal name unreachable from this
// dashboard, silently; LOOSER costs a round trip.
//
// COMPARED BY BEHAVIOUR, NOT SPELLING: two regex dialects can write one rule
// two ways, so matching source text fails on a no-op change and passes a rule
// that reads the same and decides differently. The probes are the rule's edges
// (first character's class, length ceiling, case folding, subject-token
// characters). The pattern is a string compiled here as `name.ts` does.
func TestTheDashboardsChannelNameGrammarIsTheEngines(t *testing.T) {
	t.Parallel()
	src, err := clientsource.Scalar(clientsource.Tree(t), "CHANNEL_NAME_PATTERN")
	if err != nil {
		t.Fatal(err)
	}
	client, err := regexp.Compile(src)
	if err != nil {
		t.Fatalf("the dashboard's channel-name pattern %q does not compile "+
			"here, so this gate cannot hold it against the engine: %v", src, err)
	}

	for _, probe := range []string{
		"", "a", "0", "launch", "product-launch", "a-", "-launch", "1launch",
		"Launch", "product launch", "product_launch", "product.launch",
		"prodüct", "product#launch", "#launch", "launch/1", "launch*",
		strings.Repeat("a", 64), strings.Repeat("a", 65),
		strings.Repeat("a", 63) + "-", "a" + strings.Repeat("-", 63),
	} {
		engine, screen := chatlet.ValidName(probe), client.MatchString(probe)
		if engine == screen {
			continue
		}
		verdict := "the engine refuses it and the create dialog offers it, so " +
			"the refusal arrives from the server instead of from the field"
		if engine {
			verdict = "the engine ACCEPTS it and the create dialog refuses it, " +
				"which makes a legal room name unreachable from this dashboard " +
				"with nothing anywhere to say so"
		}
		t.Errorf("channel name %q (%d bytes): %s. The engine owns the grammar "+
			"(chatlet.ValidName); contract/chat.ts's copy is what changes",
			probe, len(probe), verdict)
	}
}

// THE DASHBOARD ASKS A PENDING POST AGAIN ONCE THE APPLIER HAD ITS WAIT.
//
// A post answered `pending` was published but not yet decided where it is
// applied, so the outbox (`routes/chatlet/outbox.ts`) asks again under the same
// operation key until the ledger answers. Its first wait is
// [chatlet.DecisionWait], the applier's read budget before the change feed
// stops waiting: sooner, the node answers `pending` again; later, a refused
// post sits on screen as sent longer than the engine took to decide.
//
// Mutation: change PENDING_REASK_MS in contract/chat.ts, or DecisionWait here,
// and this goes red.
func TestTheDashboardAsksAPendingPostAgainAfterTheApplierHadItsWait(t *testing.T) {
	t.Parallel()
	raw, err := clientsource.Scalar(clientsource.Tree(t), "PENDING_REASK_MS")
	if err != nil {
		t.Fatal(err)
	}
	client, err := strconv.ParseInt(raw, 0, 64)
	if err != nil {
		t.Fatalf("PENDING_REASK_MS is %q, which is not an integer: %v", raw, err)
	}
	if want := chatlet.DecisionWait.Milliseconds(); client != want {
		t.Errorf("the dashboard asks a pending post again after %d ms and the "+
			"engine gives the answering node's applier %d ms to decide it "+
			"(chatlet.DecisionWait); the engine owns the value, so "+
			"contract/chat.ts is what changes", client, want)
	}
}
