package seataccess_test

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// THE DASHBOARD READS PER-SEAT ACCESS AS THE ENGINE WRITES IT: every interface
// in contract/access.ts declares exactly the members its wire type sends, with
// the same optionality. A member renamed on one side is a field the Agents tab
// reads as absent (a credential that never shows, a plan that never warns),
// and a member the engine omits when empty that the dashboard declared
// required is one the tab trusts to be there.
func TestTheDashboardReadsAccessAsTheEngineWritesIt(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for _, c := range []struct {
		client string
		engine reflect.Type
	}{
		{"AccessAnswer", reflect.TypeFor[seataccess.Answer]()},
		{"AccessNotice", reflect.TypeFor[seataccess.Notice]()},
		{"AccessOptions", reflect.TypeFor[seataccess.Options]()},
		{"AccessLevel", reflect.TypeFor[seataccess.Level]()},
		{"AccessPermission", reflect.TypeFor[seataccess.PermissionSpec]()},
		{"AccessSeat", reflect.TypeFor[seataccess.SeatAnswer]()},
		{"AccessSeatLane", reflect.TypeFor[seataccess.LaneAnswer]()},
		{"AccessDesired", reflect.TypeFor[seataccess.Desired]()},
		{"AccessObserved", reflect.TypeFor[seataccess.Observed]()},
		{"AccessCredential", reflect.TypeFor[seataccess.Credential]()},
		{"AccessStep", reflect.TypeFor[seataccess.Step]()},
		{"AccessStepOutcome", reflect.TypeFor[seataccess.StepOutcome]()},
		{"AccessFailure", reflect.TypeFor[seataccess.Failure]()},
		{"AccessSync", reflect.TypeFor[seataccess.SyncResult]()},
		{"AccessDraftRequest", reflect.TypeFor[seataccess.Draft]()},
		{"AccessPlanAnswer", reflect.TypeFor[seataccess.PlanAnswer]()},
		{"AccessSeatPlan", reflect.TypeFor[seataccess.SeatPlan]()},
		{"AccessInstallLink", reflect.TypeFor[seataccess.InstallLink]()},
	} {
		members, err := clientsource.Interface(tree, c.client)
		if err != nil {
			t.Error(err)
			continue
		}
		declared := map[string]bool{}
		for _, m := range members {
			declared[m.Name] = m.Optional
		}
		sent := wireMembers(c.engine)
		if len(sent) == 0 {
			t.Errorf("%s sends no members, so this gate certifies nothing", c.engine)
			continue
		}
		if !maps.Equal(declared, sent) {
			t.Errorf("contract/access.ts %s declares %v (member: optional) and the engine's "+
				"%s writes %v: change contract/access.ts to the engine's",
				c.client, declared, c.engine, sent)
		}
	}
}

// wireMembers is what encoding/json writes for typ: each field by its json
// name, optional where it is omitted when empty or zero, with an embedded
// struct's fields written in place as encoding/json does.
func wireMembers(typ reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := range typ.NumField() {
		f := typ.Field(i)
		tag, tagged := f.Tag.Lookup("json")
		if f.Anonymous && !tagged {
			maps.Copy(out, wireMembers(f.Type))
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" || !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero")
	}
	return out
}

// THE DASHBOARD KNOWS EXACTLY THE VALUES THE ENGINE SENDS AND TAKES: the lanes,
// GitHub's access words, the seat actions, the tool states and a step's op and
// verb. A value the dashboard names that the engine refuses is a control that
// is always refused; one the engine sends that the dashboard lacks is a row
// drawn with no words.
func TestTheDashboardKnowsExactlyTheAccessValues(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	strs := func(values ...string) []string { return values }
	for _, c := range []struct {
		client string
		engine []string
		valid  func(string) bool
	}{
		{"AccessLaneKey", lanes(), func(v string) bool { return seataccess.Lane(v).Valid() }},
		{"GitHubAccess", strs(string(config.GitHubRead), string(config.GitHubWrite)),
			func(v string) bool { return config.GitHubAccess(v).Valid() }},
		{"AccessSeatAction", strs(
			string(seataccess.ActionRotate), string(seataccess.ActionRemove),
			string(seataccess.ActionKeep), string(seataccess.ActionRestore),
			string(seataccess.ActionRetry), string(seataccess.ActionDone),
		), func(v string) bool { return seataccess.SeatAction(v).Valid() }},
		{"AccessToolState", strs(
			string(seataccess.ToolLive), string(seataccess.ToolBuiltin),
			string(seataccess.ToolWaiting), string(seataccess.ToolNoServer),
			string(seataccess.ToolOff),
		), nil},
		{"AccessOp", strs(
			string(seataccess.OpCreate), string(seataccess.OpEnable), string(seataccess.OpLevel),
			string(seataccess.OpScope), string(seataccess.OpMint), string(seataccess.OpDisable),
			string(seataccess.OpRemove), string(seataccess.OpRotate),
		), func(v string) bool { return seataccess.Op(v).Valid() }},
		{"AccessVerb", strs(
			string(seataccess.VerbCreate), string(seataccess.VerbGrant), string(seataccess.VerbMint),
			string(seataccess.VerbEnable), string(seataccess.VerbRerole), string(seataccess.VerbScope),
			string(seataccess.VerbDisable), string(seataccess.VerbRevoke), string(seataccess.VerbRemove),
			string(seataccess.VerbRotate), string(seataccess.VerbRestore), string(seataccess.VerbApprove),
		), func(v string) bool { return seataccess.StepVerb(v).Valid() }},
	} {
		client, err := clientsource.Union(tree, c.client)
		if err != nil {
			t.Error(err)
			continue
		}
		for _, v := range client {
			if c.valid != nil && !c.valid(v) {
				t.Errorf("contract/access.ts %s names %q, which the engine does not take", c.client, v)
			}
		}
		slices.Sort(client)
		engine := slices.Sorted(slices.Values(c.engine))
		if !slices.Equal(client, engine) {
			t.Errorf("contract/access.ts %s is %q and the engine's is %q: change "+
				"contract/access.ts to the engine's", c.client, client, engine)
		}
	}
}

func lanes() []string {
	var out []string
	for _, l := range seataccess.Lanes() {
		out = append(out, string(l))
	}
	return out
}
