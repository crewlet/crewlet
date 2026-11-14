package collect_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
)

// A NAME A VENDOR'S CONVENTIONS WRITE IS A LABEL THE ATLAS TAKES: brackets
// become parentheses, the other markup characters go, and a long name is cut
// to a label's length.
func TestAVendorsNameIsMadeALabel(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"[prod] API 5xx rate > 5%": "(prod) API 5xx rate 5%",
		"  plain   name ":          "plain name",
		"`disk` <full>":            "disk full",
	} {
		if got := collect.PlainLabel(raw); got != want {
			t.Errorf("%q is %q, want %q", raw, got, want)
		}
	}
	long := collect.PlainLabel(strings.Repeat("é", 300))
	if n := len([]rune(long)); n != atlas.MaxLabelRunes {
		t.Errorf("a long name is cut to %d characters, want %d", n, atlas.MaxLabelRunes)
	}
	if _, err := atlas.FactName.Value(long); err != nil {
		t.Errorf("the cut name is not a label: %v", err)
	}
}

// A VENDOR'S NAME IS ONE KEY SEGMENT, EXACT BOTH WAYS: what a key's grammar
// takes stays as it is, every other byte is written as its two hexadecimal
// digits, and two names never share a segment.
//
// Mutation: drop the escape of '~' itself, and "a~20b" and "a b" are one key.
func TestAVendorsNameIsOneKeySegment(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"Production": "Production", "eu west/1": "eu~20west~2f1", "a~20b": "a~7e20b", "review/é": "review~2f~c3~a9",
	} {
		got := collect.KeySegment(raw)
		if got != want {
			t.Errorf("%q is %q, want %q", raw, got, want)
		}
		if _, err := atlas.NewRef(atlas.KindPipeline, "gitlab.com/acme/api#environment:"+got); err != nil {
			t.Errorf("%q is no part of a pipeline's key: %v", got, err)
		}
	}
}
