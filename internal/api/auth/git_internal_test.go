package auth

import "testing"

// A PRODUCTION BUILD FOLDS A SENTENCE OF TWO LINES onto one rather than fail a
// refusal over its punctuation, and a test build refuses it (strict): the two
// halves of GitText's one rule, the fold being the half no test binary can
// reach through GitText itself.
func TestOneLineFoldsOutsideATestBuild(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"this node is draining", "this node is draining"},
		{"this node is draining\n\n", "this node is draining"},
		{"this node is draining\nretry in 30 s\n", "this node is draining retry in 30 s"},
	} {
		if got := oneLine(tc.in, false); got != tc.want {
			t.Errorf("oneLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("a strict oneLine wrote a sentence of two lines rather than refuse it")
		}
	}()
	oneLine("this node is draining\nretry in 30 s", true)
}
