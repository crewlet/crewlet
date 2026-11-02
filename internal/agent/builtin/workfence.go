package builtin

import (
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/worklet"
)

// What the tracker's reads hand a model that somebody else wrote, and whose
// words each piece is (ADR-0040).
//
// Three shapes recur across the tracker's answers, and each is labelled ONE
// way wherever it appears, so a change reads the same in the activity feed,
// in a task's history and in a person's inbox:
//
//   - A CHANGE'S EXCERPT — a feed row's, a history entry's, an inbox notice's —
//     is the text of the change its row records: the comment, the description
//     a task was filed with, the note a hand-off or a purge carried. The row
//     names who made the change, and that is who wrote it, so it is fenced
//     under them, as the surface the change's kind says ([worklet.ExcerptSource])
//     — except a line summing up an answer that chose, which quotes the
//     asker's option before the answerer's words and so names the answerer as
//     one writer of several ([worklet.ExcerptQuotesAChoice]).
//   - A NAME — a task's title, a project's name and purpose, a view's name, a
//     file's path, a checklist line, the values a change moved — is somebody's
//     words too, short ones, and the row that carries it records no writer of
//     it: a title may have been edited by anybody since it was filed. It is
//     neutralised in place, as every string a fenced answer holds is, and the
//     answer's envelope says the company's writers it does not name had a hand
//     in it ([namesBy]), which reads as a colleague's and never as the reader's
//     own.
//   - AN ASK'S DECISION is the asker's options, read beside a change that may
//     be somebody else's; the row names the asker nowhere, so it is a name.

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
