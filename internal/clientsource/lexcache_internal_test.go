package clientsource

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// A FILE IS LEXED ONCE HOWEVER MANY READ IT AT ONCE.
//
// The two or three dashboard gates of one package start together, and each
// reads the whole tree; a cache that looked a file up, missed, lexed it and
// only then stored the answer let every one of them lex every file at the
// same moment. So every concurrent caller of one file must be handed the SAME
// tokens — one lex's — and, for a file that does not scan, the same error.
func TestAFileIsLexedOnceHoweverManyReadItAtOnce(t *testing.T) {
	t.Parallel()
	// Content no other test lexes, and enough of it that one lex is still
	// running when the last caller arrives.
	var b strings.Builder
	fmt.Fprintf(&b, "// %s\n", t.Name())
	for i := range 5000 {
		fmt.Fprintf(&b, "const C%d = [\"a\", 'b', `c`] as const;\n", i)
	}
	src := b.String()
	broken := src + "const never = \"closed;\n"

	const callers = 8
	for _, tc := range []struct {
		name    string
		src     string
		wantErr bool
	}{{"a file that scans", src, false}, {"a file that does not", broken, true}} {
		start := make(chan struct{})
		first := make([]*token, callers)
		errs := make([]error, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Go(func() {
				<-start
				toks, err := lexed("routes/Once.tsx", tc.src, true)
				errs[i] = err
				if len(toks) > 0 {
					first[i] = &toks[0]
				}
			})
		}
		close(start)
		wg.Wait()
		for i := range callers {
			if (errs[i] != nil) != tc.wantErr || !errors.Is(errs[i], errs[0]) {
				t.Errorf("%s: caller %d was answered %v and caller 0 %v — every "+
					"caller is answered by the one lex", tc.name, i, errs[i], errs[0])
			}
			if first[i] != first[0] {
				t.Errorf("%s: caller %d was handed tokens of a lex of its own", tc.name, i)
			}
		}
	}
}
