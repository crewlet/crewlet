package queries_test

import (
	"errors"
	"slices"
	"sort"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// EVERY QUESTION THE DASHBOARD READS A WRITE BACK THROUGH TAKES A FLOOR, AND
// EVERY QUESTION THAT TAKES ONE IS LISTED UNDER EACH LOG IT READS.
//
// A write through `/operator/act` answers with where its record landed, and
// the dashboard raises a per-tab floor for that domain and asks the affected
// questions again at `read_level=session&min_position=<floor>` — so the
// screen that pressed a button never shows the state from before the press.
// `contract/domains.ts` `SESSION_QUERIES` is the list it does that for, and
// both directions of it fail silently:
//
//   - a kind listed that takes no floor is a screen that believes it waited
//     and did not: this node answers from whatever it holds and the row the
//     person just changed is drawn as it was;
//   - a kind that takes one and is not listed is a screen that is never asked
//     to wait at all, with the same result.
//
// The set of questions that take a floor is [sessionQuestions], the table
// [TestEveryNativeQuestionResolvesTheCallersOwnLevel] proves the behaviour
// over, and CHAT'S WHOLE FAMILY, [chatQuestions], which
// [TestTheCallersFreshnessReachesEveryChatRead] proves it over: a chat read is
// asked for a viewer through its own stubs, so its six live in that table
// rather than in one whose stubs are the tracker's and the wiki's. This gate
// holds the client's list against both per domain, and asks every listed kind
// a bare `session` itself — refused, because a floor it honours is a floor it
// demands — so a kind added to both lists without the behaviour behind it is
// red here too.
//
// A QUESTION READ FROM SEVERAL LOGS IS LISTED UNDER EACH, as the engine serves
// it — the company feed under the tracker's and the knowledge base's — so a
// write to either log asks it again, at the floor of each log the tab wrote
// (`protocol/session.ts` names one `min_position` per listed domain). Listed
// under one, the half on the other log is read from before its own write;
// listed under a log it does not read, its floor there is refused.
func TestEverySessionQueryTakesAFreshnessFloor(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "SESSION_QUERIES")
	if err != nil {
		t.Fatal(err)
	}
	domains, err := clientsource.Keys(body)
	if err != nil {
		t.Fatal(err)
	}
	engine := map[string][]string{}
	for _, q := range sessionQuestions {
		for _, domain := range q.domains {
			engine[domain] = append(engine[domain], q.what)
		}
	}
	// THE CHAT LOG'S, every one of its reads: each takes the caller's
	// freshness ([TestTheCallersFreshnessReachesEveryChatRead]).
	engine["chat"] = slices.Clone(chatQuestions)
	engineDomains := make([]string, 0, len(engine))
	for domain := range engine {
		engineDomains = append(engineDomains, domain)
	}
	sort.Strings(engineDomains)
	sorted := slices.Sorted(slices.Values(domains))
	if !slices.Equal(sorted, engineDomains) {
		t.Errorf("SESSION_QUERIES is keyed by %v; the engine serves floors over %v",
			sorted, engineDomains)
	}

	listed := 0
	for _, domain := range domains {
		list, err := clientsource.Property(body, domain)
		if err != nil {
			t.Errorf("%s: %v", domain, err)
			continue
		}
		client := clientsource.Strings(list)
		listed += len(client)
		for _, kind := range client {
			if !slices.Contains(engine[domain], kind) {
				t.Errorf("SESSION_QUERIES.%s lists %q, which the engine does not "+
					"serve at a %s floor — a refetch of it after a write reads "+
					"whatever this node holds", domain, kind, domain)
			}
			if _, err := askFloorless(t, kind); !errors.Is(err, queries.ErrBadParams) {
				t.Errorf("%s accepted read_level=session with no floor (answered %v) — "+
					"a question that does not demand the floor does not honour it",
					kind, err)
			}
		}
		for _, kind := range engine[domain] {
			if !slices.Contains(client, kind) {
				t.Errorf("%q takes a %s floor and SESSION_QUERIES.%s does not list "+
					"it — the screen reading it stays stale after its own write",
					kind, domain, domain)
			}
		}
	}
	// A FLOOR, because a list read as empty agrees with an empty engine.
	if listed < 10 {
		t.Errorf("SESSION_QUERIES lists %d questions; a reader that stopped "+
			"reading it would certify nothing", listed)
	}
}

// askFloorless asks one listed question for a session read with no floor,
// with the arguments [sessionQuestions] says it needs to answer at all.
func askFloorless(t *testing.T, kind string) (any, error) {
	t.Helper()
	if slices.Contains(chatQuestions, kind) {
		// AS A PERSON, through chat's own stubs: a chat read is refused
		// before anything else for a caller no seat names, so asked as
		// nobody it would answer that refusal rather than the floor's.
		params := chatAsked(kind)
		params["read_level"] = "session"
		return askChat(t, chatSources(t, newStubChat(), &stubChatSearch{}, nil), kind, params)
	}
	i := slices.IndexFunc(sessionQuestions, func(q sessionQuestion) bool { return q.what == kind })
	if i < 0 {
		// Reported by the listing check; asked with no arguments so an
		// unknown kind still gives an answer to report.
		return askNative(t, newStubs(sessionQuestion{}).sources(), kind,
			map[string]any{"read_level": "session"})
	}
	q := sessionQuestions[i]
	params := map[string]any{"read_level": "session"}
	for k, v := range q.args {
		params[k] = v
	}
	ask := askNative
	if personalQuestions[kind] {
		ask = askAsOperator
	}
	return ask(t, newStubs(q).sources(), kind, params)
}
