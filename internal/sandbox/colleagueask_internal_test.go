package sandbox

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// credentials is a provider's key, a GitHub token and a password from redact's
// corpus, the password as its value alone.
func credentials(t *testing.T) (key, token, password string) {
	t.Helper()
	sample := map[string]string{}
	for _, s := range redacttest.Shapes() {
		sample[s.Name] = s.Sample
	}
	key, token = sample["anthropic-key"], sample["github-token"]
	password = strings.TrimPrefix(sample["password"], "password: ")
	if key == "" || token == "" || password == "" {
		t.Fatal("redact's corpus no longer has a provider's key, a GitHub token and a password")
	}
	return key, token, password
}

// A COLLEAGUE'S QUESTION IS SERVED REDACTED AND STORED AS ASKED. Every JSON
// encoding of an ask, of a row holding one and of a listing of rows (what any
// handler, live state or observer that served one would write) shows each
// credential as a marker and says the question is redacted; the raw words come
// back only through RawQuestion. The run's record holds them as asked under
// `question` and round-trips them, and a served copy never decodes into a row:
// the record's codec refuses it, and a plain decode refuses every ask.
//
// Mutation: write a.question in ColleagueAsk.MarshalJSON, and the served
// encodings hold the secrets.
func TestAColleaguesQuestionIsServedRedactedAndStoredAsAsked(t *testing.T) {
	t.Parallel()
	key, token, password := credentials(t)
	secrets := []string{key, token, password}
	question := "can you deploy with " + key + "?\npassword: " + password + "\nor push as " + token
	ask := ColleagueAsk{ChannelID: "a2a-1", Chain: []string{"ceo"}, TraceID: "tr-ask"}.WithQuestion(question)
	run := PendingRun{TurnID: "t1", AgentHandle: "swe", Status: StatusRunning, ColleagueAsk: &ask}

	if ask.RawQuestion() != question {
		t.Fatalf("RawQuestion = %q, want the words as asked", ask.RawQuestion())
	}
	if shown := ask.Question(); !strings.Contains(shown, redact.Marker) || !strings.HasPrefix(shown, "can you deploy with ") {
		t.Errorf("Question = %q, want the words with each credential as a marker", shown)
	}
	served := map[string]any{
		"an ask": ask, "a pointer to an ask": &ask, "a row": run, "a pointer to a row": &run,
		"a listing": []PendingRun{run}, "a row inside an answer": map[string]any{"runs": []*PendingRun{&run}},
	}
	for what, v := range served {
		body, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("encoding %s: %v", what, err)
		}
		for _, secret := range secrets {
			if strings.Contains(string(body), secret) {
				t.Errorf("%s served holds %.24q…: %s", what, secret, body)
			}
		}
		if !strings.Contains(string(body), `"question_redacted":true`) ||
			!strings.Contains(string(body), "can you deploy with [REDACTED:") {
			t.Errorf("%s served is %s, want the question as markers, said to be redacted", what, body)
		}
	}

	stored, err := encodeRun(run)
	if err != nil {
		t.Fatal(err)
	}
	asStored, _ := json.Marshal(question)
	if !strings.Contains(string(stored), `"question":`+string(asStored)) || strings.Contains(string(stored), "question_redacted") {
		t.Errorf("the record stores %s, want the question as asked under its wire name", stored)
	}
	back, err := decodeRun(coord.Record{Key: "t1", Value: stored})
	if err != nil {
		t.Fatalf("decoding the stored record: %v", err)
	}
	if got := back.ColleagueAsk; got == nil || got.RawQuestion() != question || got.ChannelID != "a2a-1" ||
		len(got.Chain) != 1 || got.TraceID != "tr-ask" {
		t.Errorf("the record reads back ask %+v, want the one stored", got)
	}

	servedRow, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRun(coord.Record{Key: "t1", Value: servedRow}); !errors.Is(err, errServedAsk) {
		t.Errorf("decoding a served row as a record = %v, want %v", err, errServedAsk)
	}
	for what, body := range map[string][]byte{"a served row": servedRow, "a stored record": stored} {
		var row PendingRun
		if err := json.Unmarshal(body, &row); !errors.Is(err, errAskNotStored) {
			t.Errorf("a plain decode of %s = %v, want %v", what, err, errAskNotStored)
		}
	}
}

// A COLLEAGUE'S QUESTION PAST ITS SHARE IS NEVER CUT INSIDE A CREDENTIAL. The
// row keeps the question raw and serves its redaction, so a credential of each
// shape redact's corpus holds, begun at every offset across the cut, is kept
// whole or not at all: what the row serves holds no piece of it, the row keeps
// its share, and a question with nothing to redact is cut where a title is.
//
// Mutation: keep the question as keptTitle does, and a piece of a key is
// served past the cut.
func TestAColleaguesQuestionPastItsShareIsNeverCutInsideACredential(t *testing.T) {
	t.Parallel()
	plain, ok := rowRecord.Opening(keptTitle(strings.Repeat("a", 2*MaxRunTitleBytes)), 2*MaxRunTitleBytes)
	if !ok {
		t.Fatal("a question twice the row's share was kept whole")
	}
	const head = "which deploy key works?\n"
	rest := strings.Repeat(" and the rest of the ask", 16)
	for _, shape := range redacttest.Shapes() {
		t.Run(shape.Name, func(t *testing.T) {
			t.Parallel()
			secret, before, after := secretOf(shape), " ", " and"
			if strings.Contains(shape.Sample, "\n") {
				before, after = "\n", "\nand"
			}
			asked := func(at int) string {
				return head + strings.Repeat("a", at-len(head)-len(before)) + before + shape.Sample + after + rest
			}
			if piece := pieceOf(secret, ColleagueAsk{}.WithQuestion(asked(len(plain))).Question()); piece != "" {
				t.Fatalf("the whole question is served holding %q: the corpus's sample is no credential here", piece)
			}
			for at := len(plain) - len(shape.Sample) - 16; at <= len(plain)+16; at++ {
				question := asked(at)
				got := keptQuestion(question)
				if _, cut := rowRecord.Opening(got, len(question)); !cut {
					t.Fatalf("at %d: the question was kept whole; this test is of one the row cuts", at)
				}
				if kept.Quoted(got) > MaxRunTitleBytes || !strings.HasPrefix(got, head) {
					t.Fatalf("at %d: the row keeps %d bytes opening %.40q, want its first line within %d",
						at, kept.Quoted(got), got, MaxRunTitleBytes)
				}
				if piece := pieceOf(secret, ColleagueAsk{}.WithQuestion(got).Question()); piece != "" {
					t.Fatalf("at %d: the question served holds %q of the credential", at, piece)
				}
			}
		})
	}
	clean := head + strings.Repeat("<line>", 2*MaxRunTitleBytes/6)
	if got, want := keptQuestion(clean), keptTitle(clean); got != want {
		t.Errorf("a question with nothing to redact is kept as %d bytes, want %d as a title is", len(got), len(want))
	}
}

// secretOf is the part of a corpus sample its redaction hides: the value
// after a key the marker keeps, a key block's body, or the whole sample.
func secretOf(shape redacttest.Shape) string {
	switch sample := shape.Sample; {
	case strings.Contains(sample, "\n"):
		return strings.Split(sample, "\n")[1]
	case strings.ContainsAny(sample, " ="):
		value := strings.TrimLeft(sample[strings.IndexAny(sample, " =")+1:], " =")
		return strings.TrimRight(value, "=")
	default:
		return sample
	}
}

// markers is a marker the redaction writes, whose name may share bytes with a
// credential of its shape (`github_pat_`'s marker names GitHub).
var markers = regexp.MustCompile(regexp.QuoteMeta(redact.Marker) + `[^\]]*\]`)

// pieceOf is the first six bytes of secret that shown holds outside the
// markers, "" for none.
func pieceOf(secret, shown string) string {
	shown = markers.ReplaceAllString(shown, "")
	for i := 0; i+6 <= len(secret); i++ {
		if strings.Contains(shown, secret[i:i+6]) {
			return secret[i : i+6]
		}
	}
	return ""
}
