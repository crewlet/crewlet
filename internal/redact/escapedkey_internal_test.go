package redact

import (
	"strings"
	"testing"
)

// AN AWS SECRET'S KEY AND SEPARATOR ARE CLAIMED IN PIECES AS THE WHOLE TEXT
// CLAIMS THEM, across escaped line breaks: a separator's spaces longer than a
// stage's reach run to a window's end, which can part a break's backslashes
// from its `n`, and the claim still ends where the whole text's reading ends
// it ([claimed]). Mutation: end the spaces at backslashes a window ends in
// (awsSpacesOpen), leave them out of the spaces a run reads on from
// (awsSpacesBack), or resume a run inside a break (carrySeparator), and a
// claim ends short of the whole text's.
func TestAnAWSSeparatorsClaimRunsOnAcrossAWindowsEndInsideAnEscapedBreak(t *testing.T) {
	t.Parallel()
	var aws rule
	for _, r := range rules() {
		if r.pending != nil && strings.Contains(r.with, "session-token") {
			aws = r
			break
		}
	}
	b64 := strings.Repeat("Ab3+", 10)
	for _, unit := range []string{"  ", ` \n`, `\n`, `\\n `, `\r\n`} {
		for pad := range 2 * len(unit) {
			// Past a stage's reach, so the spaces run to a window's end:
			// a separator that names no value, claimed all the same.
			text := strings.Repeat("x", pad) + `SessionToken\":` +
				strings.Repeat(unit, (stageReach+stageStep)/len(unit)) + b64 + "\n"
			want := map[int]int{}
			_, words, _ := claimed(text)
			for _, w := range words {
				want[w[0]] = w[1]
			}
			src := &claimSource{keys: &keyStage{hold: 1 << 40, floor: -1}}
			st := newRuleStage(ruleStage{r: aws, mode: finds, raw: &rawMap{}, src: src})
			st.putString(text)
			st.took(nil)
			st.close()
			if len(src.words) == 0 {
				t.Errorf("unit %q, pad %d: the stage claimed no key", unit, pad)
			}
			for _, w := range src.words {
				if sep, ok := want[w[0]]; !ok || sep != w[1] {
					t.Errorf("unit %q, pad %d: the stage claimed the key at %d to %d, the whole text to %d (%v)",
						unit, pad, w[0], w[1], sep, ok)
				}
			}
		}
	}
}
