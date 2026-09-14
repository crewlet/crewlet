package config_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// tree decodes a JSON document the way a write holds one.
func tree(t *testing.T, raw string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

// carried is written after carrying stored into it, re-encoded and decoded
// again, so a case compares documents rather than map identities.
func carried(t *testing.T, stored, written string) map[string]any {
	t.Helper()
	into := tree(t, written)
	config.CarryUnknown(tree(t, stored), into)
	raw, err := json.Marshal(into)
	if err != nil {
		t.Fatal(err)
	}
	return tree(t, string(raw))
}

// A WRITE KEEPS EVERY KEY THIS BUILD CANNOT REPRESENT, wherever it sits, and
// no key it can.
//
// Each case is a stored document a newer build wrote and the document a write
// of this build produced from it; the second is what a write stores after the
// carry. The keys named `future` are ones no type of this build has.
func TestAWriteKeepsWhatThisBuildCannotRepresent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, stored, written, want string
	}{{
		name:    "at the document root",
		stored:  `{"name": "Acme", "future": {"depth": 9007199254740993}}`,
		written: `{"name": "Renamed"}`,
		want:    `{"name": "Renamed", "future": {"depth": 9007199254740993}}`,
	}, {
		name:    "inside a known block and a map entry",
		stored:  `{"providers": {"llm": {"zulu": {"type": "anthropic", "future": true}}}}`,
		written: `{"providers": {"llm": {"zulu": {"type": "openai"}}}}`,
		want:    `{"providers": {"llm": {"zulu": {"type": "openai", "future": true}}}}`,
	}, {
		name: "on a seat that moved into a unit and changed places",
		stored: `{"roles": [{"name": "CEO", "future": "ceo"}, {"name": "CTO", "handle": "cto", "future": "cto"}],
		          "units": [{"name": "Eng"}]}`,
		written: `{"roles": [{"name": "CEO"}], "units": [{"name": "Eng", "roles": [{"name": "Chief Technology", "handle": "cto"}]}]}`,
		want: `{"roles": [{"name": "CEO", "future": "ceo"}],
		        "units": [{"name": "Eng", "roles": [{"name": "Chief Technology", "handle": "cto", "future": "cto"}]}]}`,
	}, {
		name:    "on a unit moved under another",
		stored:  `{"units": [{"name": "Eng"}, {"name": "Platform", "future": "platform"}]}`,
		written: `{"units": [{"name": "Eng", "children": [{"name": "Platform"}]}]}`,
		want:    `{"units": [{"name": "Eng", "children": [{"name": "Platform", "future": "platform"}]}]}`,
	}, {
		name:    "on a member of a list identified by name, reordered",
		stored:  `{"mcp_servers": [{"name": "a", "future": "a"}, {"name": "b", "future": "b"}]}`,
		written: `{"mcp_servers": [{"name": "b"}, {"name": "a"}]}`,
		want:    `{"mcp_servers": [{"name": "b", "future": "b"}, {"name": "a", "future": "a"}]}`,
	}, {
		name:    "inside a seat's nested block",
		stored:  `{"roles": [{"name": "Dev", "sandbox": {"enabled": true, "future": 1, "setup": [{"name": "deps", "future": "deps"}]}}]}`,
		written: `{"roles": [{"name": "Dev", "sandbox": {"enabled": false, "setup": [{"name": "deps"}]}}]}`,
		want:    `{"roles": [{"name": "Dev", "sandbox": {"enabled": false, "future": 1, "setup": [{"name": "deps", "future": "deps"}]}}]}`,
	}, {
		// A key this build knows and the write left out was removed on
		// purpose, and a block the write removed takes its unknown keys
		// with it.
		name:    "never a known key the write left out",
		stored:  `{"mission": "old", "roles": [{"name": "CEO", "goal": "old goal"}], "providers": {"llm": {"gone": {"future": true}}}}`,
		written: `{"roles": [{"name": "CEO"}], "providers": {"llm": {}}}`,
		want:    `{"roles": [{"name": "CEO"}], "providers": {"llm": {}}}`,
	}, {
		// A renamed seat is a different identity, so nothing of the old
		// one reaches it.
		name:    "never onto a different identity",
		stored:  `{"roles": [{"name": "CEO", "future": "ceo"}]}`,
		written: `{"roles": [{"name": "Chief Executive"}]}`,
		want:    `{"roles": [{"name": "Chief Executive"}]}`,
	}, {
		// Two stored units of one name give nothing to tell them apart by,
		// and neither is guessed; their seats, uniquely identified, keep
		// their own.
		name: "never from an identity held twice",
		stored: `{"units": [{"name": "Platform", "future": "first", "roles": [{"name": "A", "future": "a"}]},
		                    {"name": "Platform", "future": "second", "roles": [{"name": "B", "future": "b"}]}]}`,
		written: `{"units": [{"name": "Platform", "roles": [{"name": "A"}]}, {"name": "Platform", "roles": [{"name": "B"}]}]}`,
		want:    `{"units": [{"name": "Platform", "roles": [{"name": "A", "future": "a"}]}, {"name": "Platform", "roles": [{"name": "B", "future": "b"}]}]}`,
	}, {
		name:    "never from a list member held twice in its list",
		stored:  `{"mcp_servers": [{"name": "a", "future": "one"}, {"name": "a", "future": "two"}]}`,
		written: `{"mcp_servers": [{"name": "a"}]}`,
		want:    `{"mcp_servers": [{"name": "a"}]}`,
	}, {
		// A schedule has no identity, so a position would be a guess.
		name:    "never into a list whose members have no identity",
		stored:  `{"roles": [{"name": "Dev", "schedules": [{"name": "daily", "future": "x"}]}]}`,
		written: `{"roles": [{"name": "Dev", "schedules": [{"name": "daily"}]}]}`,
		want:    `{"roles": [{"name": "Dev", "schedules": [{"name": "daily"}]}]}`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := carried(t, tc.stored, tc.written)
			if want := tree(t, tc.want); !reflect.DeepEqual(got, want) {
				gotRaw, _ := json.Marshal(got)
				t.Errorf("carried\n  %s\nwant\n  %s", gotRaw, tc.want)
			}
		})
	}
}
