package prefetch

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/provenance"
)

// A NOTE WRITTEN BEFORE A TOLD CHANGE THAT NAMES IT IS MARKED, by the newest
// such change, as a whole word in any case; a note written after it, or
// naming none of its terms, is shown as it was.
func TestANoteOlderThanAChangeNamingItIsMarked(t *testing.T) {
	t.Parallel()
	day := func(d int) time.Time { return time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC) }
	told := []ToldChange{
		{At: day(10), Vendor: "Cloudflare", Terms: []string{"Cloudflare", "Operate", "Secure"}},
		{At: day(20), Vendor: "SSH", Terms: []string{"SSH", "api-1"}},
		{At: day(25), Vendor: "Cloudflare", Terms: []string{"Cloudflare", "Secure", "Configure"}},
	}
	cases := []struct {
		name, content string
		created       time.Time
		want          string
	}{
		{"newest matching change", "Agents cannot purge the CLOUDFLARE cache.", day(1),
			" (written before your Cloudflare access changed on 2026-10-25; check Your access)"},
		{"a host id is a whole word", "Deploys to api-1 need a person.", day(1),
			" (written before your SSH access changed on 2026-10-20; check Your access)"},
		{"only changes after the note", "We have no SSH to api-1.", day(21), ""},
		{"a longer word is not the term", "Run cloudflared locally and api-12 is fine.", day(1), ""},
		{"no term at all", "Agents cannot touch production.", day(1), ""},
		{"an unknown instant", "Cloudflare is read only.", time.Time{}, ""},
	}
	for _, c := range cases {
		entry := learning.DiaryEntry{ID: "n", Content: c.content, CreatedAt: c.created}
		if got := WrittenBefore(entry, told); got != c.want {
			t.Errorf("%s: suffix = %q, want %q", c.name, got, c.want)
		}
	}
	entry := learning.DiaryEntry{ID: "n", Content: "Cloudflare is read only.", CreatedAt: day(1),
		Kind: learning.DiaryShort, TTLUntil: day(30)}
	got := renderMemory(provenance.NewBoundary(), entry, told)
	want := "- Cloudflare is read only. (written before your Cloudflare access changed on " +
		"2026-10-25; check Your access) _(short-lived; expires 2026-10-30)_"
	if got != want {
		t.Errorf("rendered = %q, want %q", got, want)
	}
	if got := renderMemory(provenance.NewBoundary(), entry, nil); strings.Contains(got, "written before") {
		t.Errorf("a note was marked with no told change: %q", got)
	}
}
