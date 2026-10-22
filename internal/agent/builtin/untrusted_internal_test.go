package builtin

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/tools"
)

// declaredRecord is a record as a store's read hands one over: a long field
// somebody wrote, a short one, and one nobody did.
type declaredRecord struct {
	ID    string            `json:"id"`
	Body  string            `json:"body" untrusted:"fence"`
	Title string            `json:"title,omitempty" untrusted:"name"`
	Tags  []string          `json:"tags,omitempty" untrusted:"name"`
	Inner *declaredRecord   `json:"inner,omitempty"`
	Under map[string]string `json:"under,omitempty" untrusted:"name"`
	embeddedRecord
}

type embeddedRecord struct {
	Note string `json:"note,omitempty" untrusted:"fence"`
}

// A RECORD'S DECLARED WORDS ARE FOUND WHERE ITS ENCODING WRITES THEM: under
// their JSON names, through a pointer, a list and a map, an embedded struct's
// as the outer one's, and only where they hold something.
func TestDeclaredFieldsAreFoundWhereTheEncodingWritesThem(t *testing.T) {
	t.Parallel()
	got, err := declaredIn(map[string]any{"rows": []declaredRecord{{
		ID: "r1", Body: "the body", Tags: []string{"", "ops"},
		Inner:          &declaredRecord{Title: "inner title"},
		embeddedRecord: embeddedRecord{Note: "a note"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range got {
		paths = append(paths, readablePath(f.path.id())+"="+f.kind)
	}
	want := []string{"rows.0.body=fence", "rows.0.tags=name", "rows.0.inner.title=name",
		"rows.0.note=fence"}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Errorf("found %v, want %v", paths, want)
	}
}

// A RECORD'S DECLARED WORDS ARE NEVER ANSWERED UNLABELLED: the plain encoder
// refuses an answer carrying one at all, and a fenced read refuses one it
// answers outside a fence, or as a name with no envelope saying whose it is —
// so a tool that hands a model a record without labelling what the record
// declares is refused rather than heard from in a transcript.
//
// Mutation: drop either check and its case answers.
func TestARecordsDeclaredWordsAreNeverAnsweredUnlabelled(t *testing.T) {
	t.Parallel()
	turn := &turnctx.Turn{Boundary: provenance.NewBoundary()}
	record := declaredRecord{ID: "r1", Body: "the body", Title: "a title"}

	res, err := jsonAnswer(map[string]any{"row": record}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Failed || tools.RefusalOf(res) != tools.RefusalInternalError {
		t.Errorf("the plain encoder answered a record's declared words: %q", res.Output)
	}

	unfenced := newForeignRead(turn, "eng")
	unfenced.attribute(nil, true)
	if _, err := unfenced.render(map[string]any{"row": record}); err == nil {
		t.Error("a fenced read answered a declared body outside its fence")
	}

	unnamed := newForeignRead(turn, "eng")
	unnamed.fence(fieldPath{}.key("row").key("body"), provenance.SourceWorkComment, nil, true, "")
	if _, err := unnamed.render(map[string]any{"row": declaredRecord{Title: "a title"}}); err == nil {
		t.Error("a fenced read answered a declared name with no envelope saying whose")
	}

	labelled := newForeignRead(turn, "eng")
	labelled.fence(fieldPath{}.key("row").key("body"), provenance.SourceWorkComment, nil, true, record.Body)
	if _, err := labelled.render(map[string]any{"row": record}); err != nil {
		t.Errorf("the control, every declared field labelled, was refused: %v", err)
	}

	// AND A RECORD DECLARING NOTHING THAT HOLDS SOMETHING is the plain
	// encoder's, as it always was.
	if res, err := jsonAnswer(map[string]any{"row": declaredRecord{ID: "r1"}}, ""); err != nil || res.Failed {
		t.Errorf("a record whose declared fields are empty was refused: %v %q", err, res.Output)
	}
}
