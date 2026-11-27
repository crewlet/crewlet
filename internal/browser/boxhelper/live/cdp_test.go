package live_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/live"
	"github.com/crewlet/crewlet/internal/browser/chromiumtest"
)

// table is cdp.mjs's table as the probe reads it: each command the live
// view sends with its parameters, each event it reads, and the flow pair.
type table struct {
	Sends map[string][]string `json:"sends"`
	Reads []string            `json:"reads"`
	Flow  []string            `json:"flow"`
}

// EVERY DEVTOOLS METHOD THE HELPER SENDS IS IN BOTH PINS: every command the
// live view's table lists, each of its parameters, and every event it
// reads, is one each pinned Chromium's own description names, but the
// screencast's flow pair, which the newest declares and the floor, below
// the flow major, declares neither of; so a pin bump that drops one fails
// here. The names come from the table and each pin's description: no Go
// text spells one.
//
// Mutations, each red: list a command no pin has; give a command a
// parameter no pin has.
func TestEveryDevToolsMethodTheHelperSendsIsInBothPins(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		var tab table
		box.Probe(t).Do(map[string]any{"op": "cdp"}, &tab)
		if len(tab.Sends) == 0 || len(tab.Reads) == 0 || len(tab.Flow) != 2 {
			t.Fatalf("the table is %+v: it read nothing, or a flow pair of %d", tab, len(tab.Flow))
		}
		d := chromiumtest.Protocol(t, env.Chrome)
		methods, params := d.Methods(), d.Parameters()
		flowArm := majorOf(t, env.Chrome) >= live.ScreencastFlowMajor
		flowFound := 0
		for method, ps := range tab.Sends {
			if !methods[method] {
				t.Errorf("the live view sends %s, which %s does not describe", method, env.Chrome)
			}
			for _, p := range ps {
				declared := params[method+"."+p]
				if slices.Contains(tab.Flow, p) {
					if declared {
						flowFound++
					}
					continue
				}
				if !declared {
					t.Errorf("the live view gives %s %s, which %s does not describe", method, p, env.Chrome)
				}
			}
		}
		if want := map[bool]int{true: 2, false: 0}[flowArm]; flowFound != want {
			t.Errorf("%s declares %d of the flow pair, and its arm (flow %v) wants %d", env.Chrome, flowFound, flowArm, want)
		}
		for _, event := range tab.Reads {
			if !methods[event] {
				t.Errorf("the live view reads %s, which %s does not describe", event, env.Chrome)
			}
		}
	})
}

// cdpSent is the probe's answer to cdp_send.
type cdpSent struct {
	Sent     []map[string]any `json:"sent"`
	Refused  string           `json:"refused"`
	OffTable bool             `json:"off_table"`
}

// THE HELPER SENDS NOTHING ITS TABLE DOES NOT LIST: a command the table
// lists, with its parameters, reaches the session; a command it does not
// list, or a listed one given a parameter it does not list, is refused as
// off the table and sends nothing. On each Node.
//
// Mutation: let send pass a call off the table.
func TestTheHelperSendsNothingItsTableDoesNotList(t *testing.T) {
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		probe := box.Probe(t)
		var tab table
		probe.Do(map[string]any{"op": "cdp"}, &tab)
		var listed string
		for method, ps := range tab.Sends {
			if len(ps) > 0 && !slices.ContainsFunc(ps, func(p string) bool { return slices.Contains(tab.Flow, p) }) {
				listed = method
				break
			}
		}
		if listed == "" {
			t.Fatal("the table lists no command with a parameter to give it")
		}
		var got cdpSent
		probe.Do(map[string]any{"op": "cdp_send", "method": listed, "params": map[string]any{tab.Sends[listed][0]: 1}}, &got)
		if got.Refused != "" || len(got.Sent) != 1 {
			t.Errorf("a call the table lists was answered %+v, want it sent", got)
		}
		for what, call := range map[string]map[string]any{
			"a command it does not list": {"op": "cdp_send", "method": listed + "Elsewhere", "params": map[string]any{}},
			"a parameter it does not list": {"op": "cdp_send", "method": listed,
				"params": map[string]any{"notAParameterOfIt": true}},
		} {
			var refused cdpSent
			probe.Do(call, &refused)
			if !refused.OffTable || len(refused.Sent) != 0 {
				t.Errorf("%s was answered %+v, want it refused off the table and nothing sent", what, refused)
			}
		}
	})
}
