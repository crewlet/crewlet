package builtin

import (
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Whose words each piece of a tracker read is (ADR-0040), labelled ONE way
// wherever it appears (the activity feed, a task's history, an inbox):
//   - A CHANGE'S EXCERPT (a comment, a filed description, a hand-off's or a
//     purge's note) is fenced under the change's maker, as the surface its kind
//     says ([worklet.ExcerptSource]); a line summing up a choosing answer quotes
//     the asker's option, so names the answerer one writer of several
//     ([worklet.ExcerptQuotesAChoice]).
//   - A NAME (a title, a project's name and purpose, a file path, a checklist
//     line, the values a change moved) records no writer, since anybody may
//     have edited it: neutralised in place, its envelope crediting unnamed
//     colleagues and never the reader ([namesBy]).
//   - AN ASK'S DECISION is the asker's options, whom the row never names: a name.

// namesBy records on read's envelope that the answer carries names somebody
// wrote — writers where the row says who had a hand in them, nobody where it
// does not — which are never vouched for as every writer of them.
func namesBy(read *foreignRead, writers ...provenance.Author) {
	read.attribute(writers, true)
}

// fenceExcerpt fences a change's excerpt at path under the writer who made the
// change, as the surface its kind says.
func fenceExcerpt(read *foreignRead, path fieldPath, kind worklet.ChangeKind,
	actor string, actorKind worklet.AuthorKind, excerpt string) {

	read.fence(path, worklet.ExcerptSource(kind),
		[]provenance.Author{read.writer(actor, iam.ActorKind(actorKind))},
		worklet.ExcerptQuotesAChoice(excerpt), excerpt)
}

// rowNames records the names a page of task rows carries: each row's title,
// which its row records no writer of.
func rowNames(read *foreignRead, rows []worklet.TaskRow) {
	if len(rows) > 0 {
		namesBy(read)
	}
}
