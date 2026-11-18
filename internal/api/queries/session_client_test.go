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
// A write through `/operator/act` answers with where its record landed; the
// dashboard re-asks the affected questions at `read_level=session&min_position`.
// `SESSION_QUERIES` (`contract/domains.ts`) lists them; both directions fail
// silently (a listed kind with no floor is drawn as it was, an unlisted one
// is never asked to wait). Floors are taken by [sessionQuestions] and CHAT'S
// WHOLE FAMILY, [chatQuestions]; a bare `session` must be refused.
//
// A QUESTION READ FROM SEVERAL LOGS IS LISTED UNDER EACH, at the floor of each
// log the tab wrote (`protocol/session.ts`).
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
