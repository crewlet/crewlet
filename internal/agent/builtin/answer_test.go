package builtin_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY ANSWER SHAPE FITS THE CEILING AT ITS OWN MAXIMUM, and this is the
// measurement the design asked for and the tree did not have.
//
// A tool answer is read by a MODEL: every byte lands in a context window
// beside the system prompt, the conversation and whatever the turn has already
// accumulated. The caps that keep these shapes small are scattered across the
// packages that own them — a comment page here, a history limit there, an
// option list somewhere else — and nothing measured the COMPOSED result until
// this. A shape that grew past the ceiling would not fail anywhere: it would
// quietly spend most of a turn's budget on one call.
//
// The maxima below are the engine's own declared caps, so this goes red when a
// cap is raised without the shape that carries it being reconsidered.
func TestEveryToolAnswerFitsToolAnswerBytes(t *testing.T) {
	t.Parallel()
	whole := maximalDetail()
	for _, c := range []struct {
		name string
		// answer is one shape a caller can actually ask for.
		answer any
	}{
		// EACH PART OF A DETAIL READ ON ITS OWN, which is the promise
		// `include` makes: a caller told to narrow has to get an answer
		// from doing so, or the advice is a dead end.
		{"get_work_item(include=comments)", onlyComments(whole)},
		{"get_work_item(include=history)", onlyHistory(whole)},
		{"get_work_item(include=links)", onlyLinks(whole)},
		// ONE COMMENT BY ID, at [worklet.MaxCommentBody]. A MaxCommentBody
		// raised past this would leave a thread page — which always
		// carries at least one whole comment — too heavy to send.
		{"get_work_item(comment=…)", oneWholeComment(whole)},
		// THE DESCRIPTION, WHOLE, which is now a part of its own: a body
		// at [worklet.MaxBody] beside a page holding one comment at its
		// cap is a full answer before anything else is said, so the two
		// can only both be whole if each can be asked for alone.
		{"get_work_item(include=body)", onlyBody(whole)},
		{"get_work_catalogue", maximalCatalogue()},
		// THE PROJECT LISTING AS A SEAT PAGES IT. Its own cap is the
		// SCREEN's — two hundred rows, ≈ 93 KiB here, refused for weight
		// so the tool asks for [worklet.MaxProjectsPerToolAnswer] and
		// says `total` beside `truncated`. Nothing measured this until
		// the row grew a field, and what a cap that does not fit buys a
		// model is not a large answer but advice to narrow and no
		// projects at all.
		{"list_projects", maximalProjects()},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := builtin.EncodeAnswer(c.answer)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if len(encoded) > builtin.ToolAnswerBytes {
				t.Errorf("a maximal %s is %d KiB and the ceiling is %d KiB — "+
					"this shape would spend most of a turn's context on one "+
					"call, and nothing on its path would say so",
					c.name, len(encoded)>>10, builtin.ToolAnswerBytes>>10)
			}
			t.Logf("%s at its maximum: %d KiB of %d", c.name,
				len(encoded)>>10, builtin.ToolAnswerBytes>>10)
		})
	}
}

// AND THE WHOLE OF A MAXIMAL ITEM IS OVER THE CEILING, which is not a defect
// but the case the ceiling exists for — stated here so it is a measured fact
// rather than an assumption.
//
// A task carrying twenty long comments, fifty history rows and sixty-four
// links is genuinely too much for one answer, and the honest response is the
// refusal that names `include` rather than a truncation a model would read as
// the whole. What must hold is the case above: every part a caller can narrow
// TO fits, so the advice works.
//
// IT FAILS WHEN THE ITEM FITS, which is the opposite arm of the same assertion
// and the reason this is a test rather than a note. It was written with a
// t.Skipf on that arm and a t.Logf on the other, so NO INPUT could turn it red:
// a ceiling raised past the maximal shape would have made the refusal path dead
// code and this would have reported a pass either way. The shape to copy is
// internal/store/capability_test.go, which fails when its measurement moves in
// EITHER direction.
func TestAMaximalItemIsRefusedRatherThanTruncated(t *testing.T) {
	t.Parallel()
	encoded, err := builtin.EncodeAnswer(maximalDetail())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(encoded) <= builtin.ToolAnswerBytes {
		t.Fatalf("a maximal item now FITS (%d KiB of %d B), so the refusal that "+
			"names `include` is unreachable and the paragraph above it describes "+
			"a path nothing takes. Either the ceiling moved or the maximal shape "+
			"shrank: reconcile builtin.ToolAnswerBytes with maximalDetail() and "+
			"rewrite this case to state whichever is now true.",
			len(encoded)>>10, builtin.ToolAnswerBytes)
	}
	t.Logf("a maximal item is %d KiB against a %d KiB ceiling, and the refusal "+
		"is what names `include`", len(encoded)>>10, builtin.ToolAnswerBytes>>10)
}

// onlyComments, onlyHistory, onlyLinks and onlyBody are one `include` part
// each, on the same maximal item. A part that is not asked for is not sent —
// the description included, which is what lets each of these fit.
func onlyComments(d worklet.TaskDetail) worklet.TaskDetail {
	d.History, d.Links, d.Task.Body = nil, nil, ""
	return d
}

func onlyHistory(d worklet.TaskDetail) worklet.TaskDetail {
	d.Comments, d.Links, d.CommentsCursor, d.Task.Body = nil, nil, "", ""
	return d
}

func onlyLinks(d worklet.TaskDetail) worklet.TaskDetail {
	d.Comments, d.History, d.CommentsCursor, d.Task.Body = nil, nil, "", ""
	return d
}

func onlyBody(d worklet.TaskDetail) worklet.TaskDetail {
	d.Comments, d.History, d.Links, d.CommentsCursor = nil, nil, nil, ""
	return d
}

// oneWholeComment is what `comment:` answers: that comment ALONE, at
// [worklet.MaxCommentBody], with no page, no cursor and no description.
func oneWholeComment(d worklet.TaskDetail) worklet.TaskDetail {
	d.History, d.Links, d.CommentsCursor, d.Task.Body = nil, nil, "", ""
	d.Comments = []worklet.Comment{{
		ID: "c", Task: "id", Author: "ana",
		Body:      strings.Repeat("c", worklet.MaxCommentBody),
		CreatedAt: time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC),
	}}
	return d
}

// maximalDetail is one task carrying every part at its cap, AS THE TOOL
// SENDS IT — and nothing on it is cut: the description is at
// [worklet.MaxBody], and the thread page is the heaviest the reader returns,
// one whole comment at [worklet.MaxCommentBody] (a page always carries at
// least one comment, and fills with whole ones up to
// [worklet.CommentPageBytes] otherwise).
func maximalDetail() worklet.TaskDetail {
	at := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	detail := worklet.TaskDetail{
		Task: worklet.Task{
			ID: "id", Key: "ENG-1", Project: "ENG",
			Title: strings.Repeat("t", worklet.MaxTitle),
			Body:  strings.Repeat("b", worklet.MaxBody),
		},
		CommentsCursor: "1772614800:id",
		Comments: []worklet.Comment{{
			ID: "c", Task: "id", Author: "ana",
			Body:      strings.Repeat("c", worklet.MaxCommentBody),
			CreatedAt: at,
		}},
	}
	for range worklet.DetailHistoryDefault {
		detail.History = append(detail.History, worklet.HistoryEntry{
			ID: "h", Kind: "status", Actor: "ana",
			Excerpt: strings.Repeat("e", worklet.MaxExcerpt), At: at,
		})
	}
	for range worklet.MaxWaitingOn {
		detail.Links = append(detail.Links, worklet.DetailLink{
			Kind: worklet.RelationWaitingOn, Other: "other",
			Key: "ENG-2", Title: strings.Repeat("l", worklet.MaxTitle),
		})
	}
	return detail
}

// maximalProjects is a seat's page of projects, each row generous rather than
// typical.
//
// NOTHING CAPS A PROJECT'S PROSE — its name, purpose and unit are the org
// chart's own text — so "maximal" here is a measured row with room in it: a
// purpose of a hundred and twenty characters is three times what a company
// writes, and the whole point of the page size is that it holds even so.
func maximalProjects() worklet.ProjectListing {
	at := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	out := worklet.ProjectListing{
		Total: worklet.MaxProjectsPerAnswer, Truncated: true,
	}
	for i := range worklet.MaxProjectsPerToolAnswer {
		out.Projects = append(out.Projects, worklet.ProjectRow{
			Key: "PROJECT" + itoa(i), Name: strings.Repeat("n", 48),
			Purpose: strings.Repeat("p", 120),
			Unit: worklet.UnitRef{
				Key: strings.Repeat("u", 32), Name: strings.Repeat("U", 32),
				Resolved: true,
			},
			Lead: worklet.LeadRef{
				Handle: strings.Repeat("l", 24), Kind: worklet.AuthorHuman,
			},
			DefaultAssignee: strings.Repeat("a", 24),
			TargetDate:      "2026-12-18",
			Counts: worklet.TaskCounts{
				Todo: 312, Active: 100, Done: 1204, Closed: 98,
			},
			LastChange: &worklet.LastChange{
				At: at, Actor: strings.Repeat("l", 24),
				ActorKind: worklet.AuthorOperator,
			},
			Version: 1099511628032,
		})
	}
	return out
}

// maximalCatalogue is every field a document may declare, each with every
// option it may carry.
func maximalCatalogue() worklet.CatalogueAnswer {
	out := worklet.CatalogueAnswer{}
	for i := range worklet.MaxTypes {
		out.Types = append(out.Types, worklet.TaskType{
			Slug: "t" + itoa(i), Name: strings.Repeat("n", 32),
			Description: strings.Repeat("d", 64),
		})
	}
	// THE OPTION LISTS AS THE READER PAGES THEM, which is what a caller
	// actually receives: the declarations allow 64 fields of 128 options,
	// and an answer carrying all 8 192 encodes at nearly a megabyte.
	budget := worklet.MaxCatalogueOptions
	for i := range worklet.MaxFieldsPerDocument {
		field := worklet.FieldDef{
			ID: "f" + itoa(i), Slug: "f" + itoa(i),
			Name: strings.Repeat("n", 32), Type: worklet.FieldDropdown,
		}
		if budget >= worklet.MaxOptions {
			for j := range worklet.MaxOptions {
				field.Config.Options = append(field.Config.Options, worklet.Option{
					ID: "o" + itoa(j), Slug: "o" + itoa(j),
					Name: strings.Repeat("o", 16),
				})
			}
			budget -= worklet.MaxOptions
		}
		out.Fields = append(out.Fields, field)
	}
	out.OptionsTotal = worklet.MaxFieldsPerDocument * worklet.MaxOptions
	out.OptionsShown = worklet.MaxCatalogueOptions
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// A TOOL ANSWER CARRIES WHAT A PERSON WROTE AS THEY WROTE IT.
//
// The standard encoder escapes `<`, `>` and `&` for JSON spliced into an HTML
// page, and a tool answer is spliced into nothing: its reader is a model, shown
// `if a < b && c > d` as six-byte escapes it has to decode back into the code
// it is reasoning about, each one five more bytes of the turn's context and of
// the answer's ceiling. And the answer ends where its value does, with no
// newline the encoder appends.
func TestAToolAnswerCarriesMarkupAsWritten(t *testing.T) {
	t.Parallel()
	const said = `if a < b && c > d { return "<ok>" }`
	encoded, err := builtin.EncodeAnswer(map[string]any{"body": said, "n": 1})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	text := string(encoded)
	if !strings.Contains(text, `if a < b && c > d { return \"<ok>\" }`) ||
		strings.Contains(text, `\u003c`) || strings.Contains(text, `\u0026`) {
		t.Errorf("the answer carries %s, want the body's own `<`, `>` and `&`", text)
	}
	if strings.HasSuffix(text, "\n") {
		t.Errorf("the answer ends in a newline the encoder appended: %q", text)
	}
	if !strings.Contains(text, "\n  \"body\": ") {
		t.Errorf("the answer is not indented as every tool answer is: %s", text)
	}
	var back map[string]any
	if err := json.Unmarshal(encoded, &back); err != nil || back["body"] != said {
		t.Errorf("the answer does not decode to what was said: %v, %v", back, err)
	}
}
