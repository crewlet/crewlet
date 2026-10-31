package chatlet_test

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// TestTheDashboardsChatLimitsAreTheEngines holds the five bounds the chat
// screens enforce — in the composer, the name field, the room settings and the
// participant picker — against the ones this package actually refuses on.
//
// THE DASHBOARD CANNOT IMPORT A GO CONSTANT, so where a screen must refuse
// early it carries its own copy, in `dashboard/src/contract/chat.ts`, and every
// copy of an engine-owned value in this tree has drifted at least once (see
// [clientsource]). This pair drifts SILENTLY IN BOTH DIRECTIONS, and neither
// direction looks like a bug from the side that sees it:
//
//   - the client's copy too LOW greys out `send` on a message the engine would
//     have taken. Nothing is logged and nothing is refused, so the person
//     concludes the limit is whatever the screen says it is.
//   - the client's copy too HIGH accepts what the write path then rejects, so
//     the refusal arrives after the typing rather than during it — which for a
//     32 KiB body is the one case where losing the draft costs something.
//
// The engine side is where the check belongs, because the engine owns the
// value: the screen's copy is a convenience and this package's constant is the
// rule.
//
// The browse dialog's bound (MAX_BROWSE_ROOMS) is not one of these: it is one
// wave of the socket's queries, derived from the contract's
// MAX_IN_FLIGHT_QUERIES, which `internal/api/stream` holds to
// `stream.MaxInFlightQueries` — the package that owns that number.
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
// THE PATTERN IS THE FRAGILE HALF OF THIS MIRROR. A number that drifts is
// wrong by an amount somebody notices; a character class that drifts is wrong
// for one input in a thousand, and the direction decides who finds out:
//
//   - the client STRICTER than the engine makes a legal name unreachable from
//     this dashboard, with nothing anywhere to say so. `name.ts` names that as
//     the failure it must never have, and nothing was holding it to that.
//   - the client LOOSER costs a round trip, which is the cheap direction and
//     still not free: the point of the copy is that somebody learns while the
//     caret is in the field.
//
// COMPARED BY BEHAVIOUR RATHER THAN BY SPELLING. Two engines' regex dialects
// can write one rule two ways, so asserting the source text matched would fail
// on a change that altered nothing — and asserting it did NOT match would pass
// a rule that reads the same and decides differently. What both sides owe is
// the same ANSWER, so the probes are the edges of the rule: the first
// character's narrower class, the length ceiling, the case folding the
// normaliser is supposed to have already done, and the characters a subject
// token cannot carry.
//
// THE CLIENT'S PATTERN IS A STRING, read as one literal and compiled here the
// way `name.ts` compiles it, because a contract module declares data and
// nothing else — and a string is the one shape both dialects compile.
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
// A post answered `pending` was published and not yet decided on the node that
// answered, and a message is decided where it is applied — so the outbox
// (`routes/chatlet/outbox.ts`) asks again under the same operation key until the
// operation ledger answers with the verdict. Its first wait is
// [chatlet.DecisionWait], the read budget the answering node's applier is given
// to reach a record before the change feed stops waiting for it: asked sooner,
// the same node answers `pending` again; later, a refused post sits on screen
// as sent for longer than the engine took to decide it.
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
