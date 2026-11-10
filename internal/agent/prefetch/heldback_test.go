package prefetch_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/learning"
)

// A NOTE AN UPGRADE OR AN ACCESS CHANGE MAY HAVE MADE FALSE IS HELD BACK. A
// platform note another build wrote and a capability note written before the
// seat's latest told access change are never put in front of the filter or the
// seat, and the block says how many were held back and how to keep one again;
// a note of this build, a later capability note and a company note are offered
// as before.
//
// Mutation: offer a held note to the filter, or say nothing of it, and this
// goes red.
func TestANoteThatMayHaveBecomeFalseIsHeldBack(t *testing.T) {
	t.Parallel()
	changed := time.Now().Add(-2 * time.Hour)
	note := func(id, content string, about learning.About, build string, at time.Time) learning.DiaryEntry {
		e := memory(id, content)
		e.About, e.EngineBuild, e.CreatedAt = about, build, at
		return e
	}
	notes := []learning.DiaryEntry{
		note("m1", "no seat has an execution sandbox", learning.AboutCapability, "v2", changed.Add(-time.Hour)),
		note("m2", "the tracker refuses long titles", learning.AboutPlatform, "v1", changed.Add(time.Hour)),
		note("m3", "my box has a browser now", learning.AboutCapability, "v2", changed.Add(time.Hour)),
		note("m4", "the engine posts replies in threads", learning.AboutPlatform, "v2", changed.Add(-time.Hour)),
		note("m5", "deploys go out on thursdays", learning.AboutCompany, "v1", changed.Add(-time.Hour)),
	}
	model := &aux{answers: []string{"[0, 1, 2]"}}
	r := request(t)
	r.AccessChangedAt = changed
	got := fetch(t, prefetch.Sources{
		Build:  "v2",
		Diary:  diary{recent: notes},
		Models: models{provider: model},
	}, r).PersonalMemory

	prompt := model.prompts()[0]
	for _, held := range []string{"no seat has an execution sandbox", "the tracker refuses long titles"} {
		if strings.Contains(prompt, held) || strings.Contains(got, held) {
			t.Errorf("the held-back note %q reached the filter or the seat", held)
		}
	}
	for _, offered := range []string{"my box has a browser now", "the engine posts replies in threads",
		"deploys go out on thursdays"} {
		if !strings.Contains(prompt, offered) {
			t.Errorf("the note %q was not offered to the filter", offered)
		}
	}
	if !strings.Contains(got, "2 notes about the platform or your access held back") ||
		!strings.Contains(got, "reflect_and_persist") {
		t.Errorf("the block does not say what it held back:\n%s", got)
	}
}
