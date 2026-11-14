package executor_test

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A list answer's cursor, read by the script off the answer's end
// ([executor.OpenCodeCursorTail]), over the fake opencode of
// opencode_script_test.go ([fakeOpenCode]).

// cutInsideACharacter is a page of the session's message list whose cursor
// names next, and whose one message — a failed step's, so the script asks the
// page after it — holds a text of three-byte characters running up to where
// the script's read of the answer's end begins, so that read begins inside one.
func cutInsideACharacter(t *testing.T, next string) string {
	t.Helper()
	for pad := range 3 {
		text := strings.Repeat("界", 400) + strings.Repeat("a", pad)
		page := openCodePageTo(next, fmt.Sprintf(`{"id":"msg_2","time":{"created":1794632161091,`+
			`"completed":1794632161101},"type":"assistant","content":[{"type":"text","text":%q}],`+
			`"finish":"error","error":{"type":"aborted","message":"Step interrupted"}}`, text))
		if cut := len(page) - executor.OpenCodeCursorTail; cut > 0 && !utf8.RuneStart(page[cut]) {
			return page
		}
	}
	t.Fatal("no padding put the start of the answer's end inside a character")
	return ""
}

// A CURSOR CUT INSIDE A CHARACTER IS STILL READ. The script reads a list
// answer's cursor off the answer's last kibibyte, and a message's text can put
// a multi-byte character across where that read begins. Under a UTF-8 locale
// GNU sed's `.` matches no part of such a character, so the match would begin
// after it and print the cut bytes in front of the cursor, which would then
// name no page. Under every sh a box may run, in a box whose locale is UTF-8,
// the page the cursor names is asked by that cursor exactly.
//
// Mutation: read the cursor without LC_ALL=C, and under GNU sed the page after
// is asked by a cursor with bytes in front of it.
func TestACursorCutInsideACharacterIsStillRead(t *testing.T) {
	t.Parallel()
	const next = "c2Vjb25k"
	first := cutInsideACharacter(t, next)
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, env: []string{"LC_ALL=C.UTF-8", "LANG=C.UTF-8"},
			pages: map[string]string{"first": first, next: openCodePage(openCodeSettled("msg_1", 1015, 200, 0))}})
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		got := listed(t, run)
		if len(got) != 2 || !strings.HasSuffix(got[1], " --param cursor="+next) {
			t.Errorf("the list was asked\n%s\nwant a second page asked by the cursor %s exactly",
				strings.Join(got, "\n"), next)
		}
	})
}
