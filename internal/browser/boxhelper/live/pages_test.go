package live_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/procgroup"
)

// frontOf is the url of state's tab in front, "" where none is.
func frontOf(s browser.State) string {
	for _, tab := range s.Tabs {
		if tab.Front {
			return tab.URL
		}
	}
	return ""
}

// onePage opens a popup window on a click, as a page does.
const onePage = `<!doctype html><html><head><title>One</title></head><body>
<button id=pop onclick="window.open('/pop', 'pop', 'width=400,height=300')">Pop</button></body></html>`

// THE SCREEN FOLLOWS THE TAB THE MODEL LAST CHOSE: no page's own state says
// which is shown, so the page in front is the model's: its first tab as its
// connection begins; the tab browser_tabs new opens, in the default context
// or an isolated one; the tab its select chooses; a popup a click opens, as
// it opens; and, as that popup closes, the tab the server makes current
// then; and a second connection begins on the first tab again. Through the
// shim and the pinned server, on each pin, each with its Node, so a bump
// that changes the server's tab order or its answer fails here.
//
// Mutations, each red: take the newest page as in front; ignore select.
func TestTheScreenFollowsTheTabTheModelLastChose(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		s := newSite(t, map[string]string{"/one": onePage, "/two": "<title>Two</title>",
			"/pop": "<title>Pop</title>", "/iso": "<title>Iso</title>"})
		w := watch(t, env, boxhelpertest.Options{}, s.URL("/one"))
		front := func(what, path string) {
			t.Helper()
			w.nextState(t, what, func(st browser.State) bool { return frontOf(st) == s.URL(path) })
		}
		tabs := func(args map[string]any, what string) { acted(t, w.model.Call("browser_tabs", args), what) }
		front("the first tab in front as the connection begins", "/one")
		tabs(map[string]any{"action": "new", "url": s.URL("/two")}, "a new tab")
		front("the new tab in front", "/two")
		tabs(map[string]any{"action": "select", "index": 0}, "selecting the first tab")
		front("the first tab in front once selected", "/one")
		acted(t, w.model.Call("browser_click", map[string]any{"target": "#pop"}), "the click that opens a popup")
		front("the popup in front as it opens", "/pop")
		tabs(map[string]any{"action": "close", "index": 2}, "closing the popup")
		front("the first tab in front once the popup closed", "/one")
		tabs(map[string]any{"action": "new", "url": s.URL("/iso"), "isolatedContext": "iso"}, "an isolated tab")
		front("the isolated tab in front", "/iso")
		w.model.Close()
		w.model.AwaitClosed()
		next := w.box.MCP(t, jobB)
		next.Initialize("2025-11-25", map[string]any{})
		front("the first tab in front as the second connection begins", "/one")
	})
}

// THE STATE NAMES EACH TAB ONCE AND FITS ITS BOUND: a box's state carries
// the title a script sets, a page loading and then loaded, an address
// without the user name and password it was opened with, a tab whose page
// crashed, and once the model has closed every tab no tab and none in
// front; each tab named once. Fitted by the box, 32 tabs of 2,048-byte
// addresses and 200-character titles fit the 64 KiB bound with their
// addresses cut to origins from the last tab, and the rev rises with each
// document. Newest alone: the state is the live view's, which no pin moves.
//
// Mutations, each red: keep an address's user name; cut addresses from the
// first tab; count rev by the tab's.
func TestTheStateNamesEachTabOnceAndFitsItsBound(t *testing.T) {
	s := newSite(t, map[string]string{"/a": "<title>A</title><p>a", "/slow": "<title>Slow</title><p>" +
		strings.Repeat("slow ", 400), "/user": "<title>User</title>"})
	release := s.Hold("/slow")
	defer release()
	w := watch(t, boxhelpertest.Newest(t), boxhelpertest.Options{}, s.URL("/a"))
	evaluate := func(fn, what string) {
		acted(t, w.model.Call("browser_evaluate", map[string]any{"function": fn}), what)
	}
	evaluate("() => { document.title = 'Set by a script'; return 1 }", "setting the title")
	w.nextState(t, "the title a script set", func(st browser.State) bool {
		return len(st.Tabs) == 1 && st.Tabs[0].Title == "Set by a script"
	})
	evaluate("() => { location.href = '/slow'; return 1 }", "opening the slow page")
	w.nextState(t, "the slow page loading", func(st browser.State) bool {
		return len(st.Tabs) == 1 && st.Tabs[0].Loading
	})
	release()
	w.nextState(t, "the slow page loaded", func(st browser.State) bool {
		return len(st.Tabs) == 1 && !st.Tabs[0].Loading && st.Tabs[0].URL == s.URL("/slow")
	})
	host := strings.TrimPrefix(s.URL(""), "http://")
	acted(t, w.model.Call("browser_navigate", map[string]any{"url": "http://u:p@" + host + "/user"}), "the page with a user")
	w.nextState(t, "the page opened with a user, without it", func(st browser.State) bool {
		return len(st.Tabs) == 1 && strings.HasSuffix(st.Tabs[0].URL, host+"/user")
	})
	w.model.Call("browser_tabs", map[string]any{"action": "new", "url": "chrome://crash"})
	crashed := w.nextState(t, "a tab crashed", func(st browser.State) bool {
		return slices.ContainsFunc(st.Tabs, func(tab browser.Tab) bool { return tab.Crashed })
	})
	ids := map[string]bool{}
	for _, tab := range crashed.Tabs {
		if ids[tab.ID] {
			t.Errorf("the state names %s twice: %+v", tab.ID, crashed.Tabs)
		}
		ids[tab.ID] = true
	}
	for range crashed.Tabs {
		w.model.Call("browser_tabs", map[string]any{"action": "close", "index": 0})
	}
	w.nextState(t, "no tab and none in front", func(st browser.State) bool {
		return len(st.Tabs) == 0 && frontOf(st) == ""
	})
	for _, rec := range w.relay.Records() {
		if rec.T == "state" && strings.Contains(string(rec.Header), "u:p@") {
			t.Errorf("a state carried the address's user name and password: %s", rec.Header)
		}
	}

	var tabs []map[string]any
	for i := range browser.MaxTabs {
		tabs = append(tabs, map[string]any{"id": fmt.Sprintf("t%d", i+1), "front": i == 0, "loading": false,
			"crashed": false, "title": strings.Repeat("é", browser.MaxTitleChars),
			"url": "https://example.com/" + strings.Repeat("a", browser.MaxURLBytes-len("https://example.com/"))})
	}
	var made struct {
		Doc   browser.State `json:"doc"`
		Bytes int           `json:"bytes"`
	}
	w.box.Probe(t).Do(map[string]any{"op": "state_make", "boot": w.boot, "running": "running", "tabs": tabs,
		"times": 3}, &made)
	if made.Bytes > browser.MaxStateBytes || made.Doc.Rev != 3 || len(made.Doc.Tabs) != browser.MaxTabs {
		t.Fatalf("the fitted state is %d bytes, rev %d, %d tabs: want at most %d, rev 3, every tab", made.Bytes,
			made.Doc.Rev, len(made.Doc.Tabs), browser.MaxStateBytes)
	}
	var user struct {
		Doc browser.State `json:"doc"`
	}
	w.box.Probe(t).Do(map[string]any{"op": "state_make", "boot": w.boot, "running": "running", "tabs": []any{
		map[string]any{"id": "t1", "front": true, "title": "", "url": "https://u:p@example.com/x?y#z"}}}, &user)
	if len(user.Doc.Tabs) != 1 || user.Doc.Tabs[0].URL != "https://example.com/x?y#z" || user.Doc.Tabs[0].URLCut {
		t.Errorf("an address with a user name and password reads %+v, want https://example.com/x?y#z", user.Doc.Tabs)
	}
	cut := 0
	for i, tab := range made.Doc.Tabs {
		if tab.URLCut != (tab.URL == "https://example.com") || tab.TitleCut {
			t.Errorf("tab %d is %+v: an address cut is its origin, and no title needs cutting", i, tab)
		}
		if tab.URLCut {
			cut++
		} else if cut > 0 {
			t.Errorf("tab %d kept its address after a later tab's was cut: cuts run from the last tab", i)
		}
	}
	if cut == 0 || cut == browser.MaxTabs {
		t.Errorf("%d addresses were cut, want those from the last tab that fitting needs", cut)
	}
}

// THE BOX READS A USER IN AN ADDRESS AS THE HUB DOES: over addresses the two
// readers could part on (an escape net/url refuses, %zz, a%2 or one in the
// host; an empty user; an upper-case scheme; an @ in a path, a query or a
// fragment; an opaque address; one no URL parser reads), the box keeps an
// address as it is exactly where internal/browser's ParseState takes it, and
// the hub takes every state the box makes, the addresses it changed among
// them. So no state is refused for a user the box's reader missed. On the
// probe, on each Node.
//
// Mutations, each red: the hub reads a user through net/url; the box reads
// one through the URL parser's username and password.
func TestTheBoxReadsAUserInAnAddressAsTheHubDoes(t *testing.T) {
	const boot = "00000000000000b1"
	addresses := []string{
		"https://u:p@example.com/", "https://u@example.com/", "https://@example.com/", "https://:@example.com/",
		"HTTPS://U:P@EXAMPLE.COM/", "https://user:secret@example.com/%zz", "https://user:secret@example.com/a%2",
		"https://user:secret@exa%41mple.com/", "https://u:p@[::1/", "ftp://u:p@example.com/f",
		"https://example.com/", "https://example.com/%zz", "https://example.com/?to=a@example.com",
		"https://example.com/a@b#c@d", "mailto:a@example.com", "data:,a@b", "about:blank",
	}
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		probe := boxhelpertest.Start(t, env, boxhelpertest.Options{}).Probe(t)
		for _, address := range addresses {
			var made struct {
				Doc browser.State `json:"doc"`
			}
			probe.Do(map[string]any{"op": "state_make", "boot": boot, "running": "running", "tabs": []any{
				map[string]any{"id": "t1", "front": true, "title": "", "url": address}}}, &made)
			if len(made.Doc.Tabs) != 1 {
				t.Fatalf("%s: the box made %+v, want its one tab", address, made.Doc)
			}
			kept := made.Doc.Tabs[0].URL
			as, err := json.Marshal(browser.State{V: 1, Boot: boot, Rev: 1,
				Running: browser.RunStateRunning, Tabs: []browser.Tab{{ID: "t1", URL: address, Front: true}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, refused := browser.ParseState(as); (kept == address) != (refused == nil) {
				t.Errorf("%s: the box made it %q, and the hub answers %v for it as it is", address, kept, refused)
			}
			body, err := json.Marshal(made.Doc)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := browser.ParseState(body); err != nil {
				t.Errorf("%s: the hub refuses the state the box made of it, its address %q: %v", address, kept, err)
			}
		}
	})
}

// TAB IDS RUN OUT INTO A NEW BOOT: a registry whose next number would take
// a tenth digit draws a new boot and numbers its live pages again from t1,
// the next page after them, and restarts no browser for it; one with no
// page left numbers the next t1. On the probe, on each Node.
//
// Mutation: a tenth digit (t1000000000).
func TestTabIdsRunOutIntoANewBoot(t *testing.T) {
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		probe := boxhelpertest.Start(t, env, boxhelpertest.Options{}).Probe(t)
		for _, tc := range []struct {
			next, live int
			numbered   []string
			ids        []string
			added      string
		}{
			{next: 999_999_998, live: 2, numbered: []string{"t999999998", "t999999999"}, ids: []string{"t1", "t2"},
				added: "t3"},
			{next: 1_000_000_000, live: 0, added: "t1"},
		} {
			var got struct {
				Numbered   []string `json:"numbered"`
				IDs        []string `json:"ids"`
				Added      string   `json:"added"`
				BootBefore string   `json:"boot_before"`
				Boot       string   `json:"boot"`
			}
			probe.Do(map[string]any{"op": "tabs_overflow", "next": tc.next, "live": tc.live}, &got)
			if !slices.Equal(got.Numbered, tc.numbered) || !slices.Equal(got.IDs, tc.ids) || got.Added != tc.added ||
				got.Boot == got.BootBefore {
				t.Errorf("from %d with %d live pages: numbered %q, then %q and %s under boot %s (was %s), want %q, then "+
					"%q and %s under a new boot", tc.next, tc.live, got.Numbered, got.IDs, got.Added, got.Boot,
					got.BootBefore, tc.numbered, tc.ids, tc.added)
			}
		}
	})
}

// THE VIEW READS ONLY A TAB LIST IT CAN TRUST: of a browser_tabs answer the
// view reads the Result section alone, its tabs numbered from 0 in order, at
// most one current; another section's list is no tab call's, and a list out
// of order or with two current is no list at all, so the view stays where
// it was. On the probe, on each Node.
//
// Mutation: read every section's tab lines.
func TestTheViewReadsOnlyATabListItCanTrust(t *testing.T) {
	answer := func(text string) map[string]any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
	}
	type current struct {
		Index int    `json:"index"`
		Line  string `json:"line"`
	}
	type listed struct {
		Count   int      `json:"count"`
		Current *current `json:"current,omitempty"`
	}
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		probe := boxhelpertest.Start(t, env, boxhelpertest.Options{}).Probe(t)
		for _, tc := range []struct {
			name string
			text string
			want *listed
		}{
			{"three tabs, the second current", "### Result\n- 0: [A](http://a/)\n- 1: (current) [B](http://b/)\n" +
				"- 2: [C](http://c/)\n### Page\n- Page URL: http://b/",
				&listed{3, &current{1, "- 1: (current) [B](http://b/)"}}},
			{"no tab", "### Result\nNo open tabs. Navigate to a URL to create one.", &listed{0, nil}},
			{"open tabs listed after the result", "### Result\n- 0: (current) [A](http://a/)\n### Open tabs\n" +
				"- 0: [A](http://a/)\n- 1: (current) [B](http://b/)", &listed{1, &current{0, "- 0: (current) [A](http://a/)"}}},
			{"tabs out of order", "### Result\n- 1: (current) [B](http://b/)\n- 0: [A](http://a/)", nil},
			{"two current", "### Result\n- 0: (current) [A](http://a/)\n- 1: (current) [B](http://b/)", nil},
			{"no result section", "### Open tabs\n- 0: (current) [A](http://a/)", nil},
		} {
			var got struct {
				Listed *listed `json:"listed"`
			}
			probe.Do(map[string]any{"op": "tabs_listed", "answer": answer(tc.text)}, &got)
			if a, b := mustJSON(t, got.Listed), mustJSON(t, tc.want); string(a) != string(b) {
				t.Errorf("%s: read as %s, want %s", tc.name, a, b)
			}
		}
	})
}

// A WATCHED BROWSER THAT CRASHES COMES BACK UNDER ITS BOOT: Chromium killed
// under a watched page, the state says restarting, listing no tab of the
// browser that crashed, then running with the relaunched browser's tab under
// the same boot, its id one never used before; and frames come again, their
// numbers rising on. Newest alone: the relaunch is the supervisor's, which no
// pin moves.
//
// Mutations, each red: draw a new boot at a relaunch; number tabs from t1 at
// a relaunch.
func TestAWatchedBrowserThatCrashesComesBackUnderItsBoot(t *testing.T) {
	s := newSite(t, map[string]string{"/": paintPage})
	w := watch(t, boxhelpertest.Newest(t), boxhelpertest.Options{}, s.URL("/"))
	running := w.nextState(t, "a state of the running browser", func(st browser.State) bool {
		return st.Running == browser.RunStateRunning && len(st.Tabs) == 1
	})
	w.relay.AckEach()
	w.relay.Watch(true, browser.WatchFPS)
	before, _ := w.nextFrame(t, "a frame before the crash", anyFrame)
	crashed := w.box.Supervisor(t).Chromium.PID
	if err := procgroup.Kill(crashed); err != nil {
		t.Fatal(err)
	}
	restarting := w.nextState(t, "the browser restarting", func(st browser.State) bool {
		return st.Running == browser.RunStateRestarting
	})
	for _, tab := range restarting.Tabs {
		if idNumber(t, tab.ID) <= idNumber(t, running.Tabs[0].ID) {
			t.Errorf("the restarting state lists %+v, a tab of the browser that crashed", tab)
		}
	}
	back := w.nextState(t, "the relaunched browser running", func(st browser.State) bool {
		return st.Running == browser.RunStateRunning && len(st.Tabs) == 1
	})
	if back.Boot != running.Boot || back.Boot != w.boot || back.Tabs[0].ID == running.Tabs[0].ID ||
		idNumber(t, back.Tabs[0].ID) <= idNumber(t, running.Tabs[0].ID) {
		t.Errorf("after the crash the state is %+v, and before it %+v: want the same boot and a tab id never used", back,
			running)
	}
	after, _ := w.nextFrame(t, "a frame of the relaunched browser", func(f browser.FrameMeta, _ []byte) bool {
		return f.Tab == back.Tabs[0].ID
	})
	if after.Boot != before.Boot || after.Seq <= before.Seq {
		t.Errorf("the relaunched browser's frame is %+v, and the crashed one's %+v: want the same boot, its number on",
			after, before)
	}
}

// idNumber is a tab id's number.
func idNumber(t *testing.T, id string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(id, "t%d", &n); err != nil {
		t.Fatalf("tab id %q: %v", id, err)
	}
	return n
}
