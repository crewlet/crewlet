package caveat_test

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD READS WHAT A CAVEAT ASKS BY THE ENGINE'S WORDS.
//
// A press whose answer carries a caveat raises a toast that stays until it is
// dismissed when the caveat is `required`, and one that goes by itself when it
// is `none`. The dashboard reads the action off the answer against its own
// list of them; a list that drifted would read an action it does not know as
// something to merely know about, and a setting left broken would scroll away.
func TestTheDashboardReadsWhatACaveatAsks(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "CAVEAT_ACTIONS")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	var engine []string
	for _, a := range caveat.Actions() {
		engine = append(engine, string(a))
	}
	if len(client) == 0 {
		t.Fatal("CAVEAT_ACTIONS names nothing, so this gate certifies nothing")
	}
	if !slices.Equal(client, engine) {
		t.Errorf("the dashboard's CAVEAT_ACTIONS is %q and the engine's is %q, in "+
			"that order: change contract/caveats.ts to the engine's", client, engine)
	}
}

// THE DASHBOARD KNOWS EVERY SETTING A REMEDY NAMES.
//
// A refusal a limit caused carries a remedy naming the setting, and the
// dashboard resolves it to the screen and section that edit it. A setting the
// dashboard's list lacks is a refusal drawn with nothing to open, the incident
// remedies exist to end: "The assistant's budget for now is spent" with no way
// to the ceiling that spent it.
func TestTheDashboardKnowsEverySettingARemedyNames(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "LIMIT_SETTINGS")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	var engine []string
	for _, s := range caveat.Settings() {
		engine = append(engine, string(s))
	}
	if len(client) == 0 {
		t.Fatal("LIMIT_SETTINGS names nothing, so this gate certifies nothing")
	}
	if !slices.Equal(client, engine) {
		t.Errorf("the dashboard's LIMIT_SETTINGS is %q and the engine's is %q, in "+
			"that order: change contract/remedies.ts to the engine's", client, engine)
	}
}

// THE DASHBOARD READS A REMEDY AS THE ENGINE WRITES IT: the same members, with
// the same optionality. A member renamed on one side is a remedy the drawer
// reads as naming nothing, and a seat sent optionally that the client declared
// required would be read off every remedy that is not a seat's.
func TestTheDashboardReadsARemedyAsTheEngineWritesIt(t *testing.T) {
	t.Parallel()
	client, err := clientsource.Interface(clientsource.Tree(t), "Remedy")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, m := range client {
		declared[m.Name] = m.Optional
	}
	sent := map[string]bool{}
	typ := reflect.TypeFor[caveat.Remedy]()
	for i := range typ.NumField() {
		name, opts, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		sent[name] = strings.Contains(opts, "omitempty")
	}
	if !maps.Equal(declared, sent) {
		t.Errorf("contract/remedies.ts Remedy declares %v (member: optional) and the "+
			"engine writes %v", declared, sent)
	}
}
