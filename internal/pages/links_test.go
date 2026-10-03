package pages_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/pages"
)

// A BACKLINK IS AN ID IN ONE OF TWO ADDRESSES, and nothing else: every case
// below names the grammar it holds, and the ones that must NOT be found are
// as load-bearing as the ones that must — a page documenting the address
// grammar in a code block would otherwise be "linked from" every page it uses
// as an example.
func TestABacklinkIsAPageIdInOneOfTwoAddresses(t *testing.T) {
	t.Parallel()
	const (
		a = "0b6f5a4e-6a41-4b6e-9d8c-3f1e2d4c5b6a"
		b = "1c7e6b5f-7b52-4c7f-8e9d-4a2f3e5d6c7b"
		c = "2d8f7c6a-8c63-4d8a-9fae-5b3a4f6e7d8c"
	)
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"the dashboard address", "See [the runbook](" + pages.AddressPrefix + a + ").", []string{a}},
		{"the path", "Read /pages/" + a + " first.", []string{a}},
		{"a deployment URL carries both, and is one link",
			"https://crewlet.example.com/" + pages.AddressPrefix + a, []string{a}},
		{"uppercase is the same id, lowercased", "/pages/" + "0B6F5A4E-6A41-4B6E-9D8C-3F1E2D4C5B6A", []string{a}},
		{"repeats collapse and the ids are sorted",
			"/pages/" + c + " and /pages/" + a + " and again " + pages.AddressPrefix + c,
			[]string{a, c}},
		{"a percent-encoded address is found",
			"https://chat.example.com/redirect?to=%23%2Fknowledge%2Fpages%2F" + b, []string{b}},
		{"a bare uuid is a value, not a link", "The id is " + a + ".", nil},
		{"a name is an address a rename moves", "Read ENG/Provisioner runbook.", nil},
		{"an id that runs on is some other path", "/pages/" + a + "x", nil},
		{"a fenced block is an example",
			"Before.\n```\ncurl https://x/pages/" + a + "\n```\nAfter /pages/" + b, []string{b}},
		{"a tilde fence with an info string",
			"~~~md\n" + pages.AddressPrefix + a + "\n~~~\n", nil},
		{"a longer fence is closed only by one as long",
			"````\n```\n/pages/" + a + "\n```\n/pages/" + c + "\n````\n/pages/" + b, []string{b}},
		{"an unclosed fence runs to the end", "```\n/pages/" + a, nil},
		{"an inline code span is an example", "Link as `/pages/" + a + "`, like /pages/" + b, []string{b}},
		{"an unclosed backtick is literal text", "a ` stray, /pages/" + a, []string{a}},
		{"nothing to find", "Plain prose about pages in general.", nil},
		{"empty", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pages.Links(tc.body); !slices.Equal(got, tc.want) {
				t.Errorf("Links(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// THE DASHBOARD MINTS THE ADDRESS THE BACKLINKS READ, read out of its own
// source. The editor's "Link a page" writes `PAGE_ADDRESS_PREFIX` + an id into
// a body, and [pages.Links] is what finds it there: a route moved on one side
// alone would leave every link written afterwards out of "linked from", with
// nothing on any screen to say so.
func TestTheDashboardAddressesAPageTheWayTheBacklinksReadIt(t *testing.T) {
	t.Parallel()
	prefix, err := clientsource.Scalar(clientsource.Tree(t), "PAGE_ADDRESS_PREFIX")
	if err != nil {
		t.Fatal(err)
	}
	if prefix != pages.AddressPrefix {
		t.Errorf("the dashboard addresses a page as %q and the backlinks read %q — "+
			"change PAGE_ADDRESS_PREFIX in contract/links.ts", prefix, pages.AddressPrefix)
	}
	const id = "0b6f5a4e-6a41-4b6e-9d8c-3f1e2d4c5b6a"
	if got := pages.Links("[the runbook](" + prefix + id + ")"); !slices.Equal(got, []string{id}) {
		t.Errorf("a link the dashboard mints is read as %v, want the page", got)
	}
}
