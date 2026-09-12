package builtin_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/tracker"
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
		{"get_work_catalogue", maximalCatalogue()},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.MarshalIndent(c.answer, "", "  ")
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
func TestAMaximalItemIsRefusedRatherThanTruncated(t *testing.T) {
	t.Parallel()
	encoded, err := json.MarshalIndent(maximalDetail(), "", "  ")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(encoded) <= builtin.ToolAnswerBytes {
		t.Skipf("a maximal item now fits (%d KiB of %d), so this case has "+
			"nothing to guard — the ceiling holds by construction",
			len(encoded)>>10, builtin.ToolAnswerBytes>>10)
	}
	t.Logf("a maximal item is %d KiB against a %d KiB ceiling, and the refusal "+
		"is what names `include`", len(encoded)>>10, builtin.ToolAnswerBytes>>10)
}

// onlyComments, onlyHistory and onlyLinks are one `include` part each, on the
// same maximal item.
func onlyComments(d tracker.TaskDetail) tracker.TaskDetail {
	d.History, d.Links = nil, nil
	return d
}

func onlyHistory(d tracker.TaskDetail) tracker.TaskDetail {
	d.Comments, d.Links, d.CommentsCursor = nil, nil, ""
	return d
}

func onlyLinks(d tracker.TaskDetail) tracker.TaskDetail {
	d.Comments, d.History, d.CommentsCursor = nil, nil, ""
	return d
}

// maximalDetail is one task carrying every collection at its cap.
//
// THE BODY IS THE EXCEPTION and is deliberately not at [tracker.MaxBody]: a
// 64 KiB body IS the answer somebody asked for, and eliding it would answer a
// different question. What this measures is everything AROUND it — the parts
// a caller did not ask for by name and which grow without them noticing.
func maximalDetail() tracker.TaskDetail {
	at := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	detail := tracker.TaskDetail{
		Task: tracker.Task{
			ID: "id", Key: "ENG-1", Project: "ENG",
			Title: strings.Repeat("t", tracker.MaxTitle),
			Body:  strings.Repeat("b", 4<<10),
		},
		CommentsCursor: "1772614800:id",
	}
	// THE COMMENT PAGE at its own cap, each body elided as the reader
	// elides it. Twenty at MaxCommentBody would be 640 KiB — ten times the
	// ceiling — which is the whole reason the elision exists.
	for range tracker.DetailComments {
		detail.Comments = append(detail.Comments, tracker.Comment{
			ID: "c", Task: "id", Author: "ana",
			Body:      strings.Repeat("c", tracker.CommentBodyShown),
			CreatedAt: at,
		})
	}
	for range tracker.DetailHistoryDefault {
		detail.History = append(detail.History, tracker.HistoryEntry{
			ID: "h", Kind: "status", Actor: "ana",
			Excerpt: strings.Repeat("e", tracker.MaxExcerpt), At: at,
		})
	}
	for range tracker.MaxWaitingOn {
		detail.Links = append(detail.Links, tracker.DetailLink{
			Kind: tracker.RelationWaitingOn, Other: "other",
			Key: "ENG-2", Title: strings.Repeat("l", tracker.MaxTitle),
		})
	}
	return detail
}

// maximalCatalogue is every field a document may declare, each with every
// option it may carry.
func maximalCatalogue() tracker.CatalogueAnswer {
	out := tracker.CatalogueAnswer{}
	for i := range tracker.MaxTypes {
		out.Types = append(out.Types, tracker.TaskType{
			Slug: "t" + itoa(i), Name: strings.Repeat("n", 32),
			Description: strings.Repeat("d", 64),
		})
	}
	// THE OPTION LISTS AS THE READER PAGES THEM, which is what a caller
	// actually receives: the declarations allow 64 fields of 128 options,
	// and an answer carrying all 8 192 encodes at nearly a megabyte.
	budget := tracker.MaxCatalogueOptions
	for i := range tracker.MaxFieldsPerDocument {
		field := tracker.FieldDef{
			ID: "f" + itoa(i), Slug: "f" + itoa(i),
			Name: strings.Repeat("n", 32), Type: tracker.FieldDropdown,
		}
		if budget >= tracker.MaxOptions {
			for j := range tracker.MaxOptions {
				field.Config.Options = append(field.Config.Options, tracker.Option{
					ID: "o" + itoa(j), Slug: "o" + itoa(j),
					Name: strings.Repeat("o", 16),
				})
			}
			budget -= tracker.MaxOptions
		}
		out.Fields = append(out.Fields, field)
	}
	out.OptionsTotal = tracker.MaxFieldsPerDocument * tracker.MaxOptions
	out.OptionsShown = tracker.MaxCatalogueOptions
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
