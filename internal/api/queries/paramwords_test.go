package queries_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/usage"
)

// A PARAMETER REFUSAL IS TOLD IN WORDS, NEVER IN GO'S.
//
// A refused parameter reaches a person as the detail of a `bad_params`, on
// the socket and over REST alike ([queries.RefusalDetail]), and several were
// the wrap of somebody else's error: a cursor's time as the time package's
// layout ("parsing time \"x\" as \"2006-01-02T15:04:05.999999999Z07:00\""),
// a spend window's dates as the tokens package's ("tokens: … is before …"),
// a histogram's window as the store's with Go's durations ("store: histogram
// window is too many buckets: minute over 2160h0m0s"), a datastore query as
// encoding/json's, an entity as the document service's ("configapi: unknown
// entity kind"), and a closed set as Go prints a slice ("[minute hour day]").
// Each row asks the real question with the parameter wrong and holds the
// detail to what it should say and to none of those words. Mutations: wrap
// the parse error again, list a set with %v, or pass the store's or the
// document service's error through, and its row goes red.
func TestAParameterRefusalIsToldInWordsNeverInGos(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	events := registryOver(t, queries.Sources{Events: fleetOf(db.Events())})
	spend := registryOver(t, queries.Sources{Usage: usage.Reader{Estate: db.Replicated()}})
	surface, _ := configSurface(t, companyDoc)
	config := registryOver(t, queries.Sources{Config: surface})
	memory := registryOver(t, queries.Sources{Company: companySource(t, company(t)),
		Memory: &stubMemory{}})
	data, _ := dataRegistry(t)
	long := time.Now().Add(-90 * 24 * time.Hour).UTC().Format(time.RFC3339)

	for _, tc := range []struct {
		name   string
		r      *queries.Registry
		what   string
		params map[string]any
		says   string
	}{
		{"a cursor's time that isn't one", events, "events",
			map[string]any{"before_id": "e1", "before_time": "yesterday"},
			`before_time must be an RFC 3339 instant (2026-04-16T09:00:00Z), and "yesterday" isn't`},
		{"a cursor with no time", events, "phases", map[string]any{"before_id": "e1"},
			"before_id needs a before_time"},
		{"a bucket nobody knows", events, "event_series", map[string]any{"bucket": "fortnight"},
			`the bucket parameter is one of "minute", "hour" or "day", and "fortnight" isn't`},
		{"a window past the bars a chart shows", events, "event_series",
			map[string]any{"bucket": "minute", "since": long},
			"Pick a coarser bucket or a shorter window"},
		{"a spend window that runs backwards", spend, "token_series",
			map[string]any{"since": "2026-06-08", "until": "2026-06-01"},
			`and "2026-06-08" to "2026-06-01" isn't`},
		{"a spend window that isn't dates", spend, "token_series",
			map[string]any{"since": "june", "until": "2026-06-01"},
			`and "june" to "2026-06-01" isn't`},
		{"a spend bucket nobody knows", spend, "token_series",
			map[string]any{"bucket": "hour"},
			`the bucket parameter is one of "day" or "week", and "hour" isn't`},
		{"a filter outside its set", events, "events",
			map[string]any{"failed": "maybe"},
			`the failed parameter is one of "true" or "false", and "maybe" isn't`},
		{"a spend grouping nobody knows", spend, "token_series",
			map[string]any{"group": "colour"},
			`the group parameter is one of "phase", "model", "provider", "seat", "unit" or "worker", and "colour" isn't`},
		{"a page's readers over too many days", spend, "page_reads",
			map[string]any{"page": "p1", "days": "400"},
			`the days parameter is "400", and a page's readers cover 1 to`},
		{"an entity kind nobody knows", config, "config_entities",
			map[string]any{"kind": "gadgets"}, `the kind parameter is one of `},
		{"an entity nobody declared", config, "config_entities",
			map[string]any{"kind": "roles", "id": "nobody-here"},
			`no seat is called "nobody-here"`},
		{"a skills order nobody knows", memory, "agent_memory",
			map[string]any{"id": "cto", "skills_order": "alphabetical"},
			`not "alphabetical"`},
		{"a datastore query that isn't JSON", data, "artifact_data_query",
			map[string]any{"id": dataSheet.String(), "collection": "review", "q": "{where"},
			"the q parameter isn't valid JSON: it goes wrong at byte 2. It takes a query as JSON"},
	} {
		_, err := tc.r.Answer(everyGrant(t), tc.what, tc.params)
		if !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("%s: answered %v, want bad params", tc.name, err)
			continue
		}
		said := queries.RefusalDetail(err)
		if !strings.Contains(said, tc.says) {
			t.Errorf("%s: told %q, want it to say %q", tc.name, said, tc.says)
		}
		// AS BOTH TRANSPORTS SEND IT, through httpjson.Sentence, which
		// capitalises a plain leading word: a parameter's name opened the
		// sentence, so a person read "Kind is one of …", a name the
		// parameter does not have.
		for _, name := range []string{"Bucket ", "Group ", "Days ", "Kind ", "Q "} {
			if strings.HasPrefix(httpjson.Sentence(said), name) {
				t.Errorf("%s: a person reads %q", tc.name, httpjson.Sentence(said))
			}
		}
		for _, goWords := range []string{"parsing time", "2006-01-02T15:04:05",
			"cannot parse", "tokens:", "store:", "configapi:", "json:",
			"invalid character", "[", "h0m0s"} {
			if strings.Contains(said, goWords) {
				t.Errorf("%s: told %q, which carries %q", tc.name, said, goWords)
			}
		}
	}
}
