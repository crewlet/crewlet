package llmapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// A CHECK'S ANSWER IS KEPT WITHIN ITS RECORD'S BOUND, EVERY LINE IN IT. The
// doctor's lines quote what the harness said, which a model answering at
// length makes long, and a record past its bound is refused by the store, so
// the answer would be lost whole. Cut, it keeps its verdict and every line,
// the longest cut first and the cut marked; an answer within the bound is
// kept byte for byte.
//
// Mutation: drop the cut, cut every line alike rather than the longest, or
// cut the caller's lines rather than a copy, and this is red.
func TestACheckAnswerIsKeptWithinItsRecordsBoundEveryLineInIt(t *testing.T) {
	t.Parallel()
	short := CheckAnswer{Node: "node-a", OK: true, Lines: []CheckLine{{Check: "binary", OK: true, Detail: "claude"}}}
	raw, err := boundedAnswer(short)
	if want, _ := json.Marshal(short); err != nil || string(raw) != string(want) {
		t.Errorf("an answer within its bound = %s (%v), want it byte for byte", raw, err)
	}

	said := strings.Repeat("the model answered at length. ", coord.MaxModelCheckAnswerBytes/10)
	long := CheckAnswer{Node: "node-a", Failure: "config", Lines: []CheckLine{
		{Check: "binary", OK: true, Detail: "claude at /usr/local/bin/claude"},
		{Check: "smoke", Detail: "failed — It said: " + said, Kind: "config", Remedy: said},
		{Check: "web", OK: true, Detail: "allowed; ok"},
	}}
	before := len(long.Lines[1].Detail)
	raw, err = boundedAnswer(long)
	if err != nil {
		t.Fatalf("boundedAnswer: %v", err)
	}
	if len(long.Lines[1].Detail) != before {
		t.Errorf("the cut reached the caller's answer: its smoke line is %d bytes, was %d",
			len(long.Lines[1].Detail), before)
	}
	if len(raw) > coord.MaxModelCheckAnswerBytes {
		t.Fatalf("the answer is %d bytes, past its record's %d", len(raw), coord.MaxModelCheckAnswerBytes)
	}
	var kept CheckAnswer
	if err := json.Unmarshal(raw, &kept); err != nil {
		t.Fatal(err)
	}
	if kept.Failure != "config" || len(kept.Lines) != 3 || kept.Lines[0] != long.Lines[0] ||
		kept.Lines[2] != long.Lines[2] {
		t.Errorf("the cut answer = %+v, want its verdict and the short lines untouched", kept)
	}
	smoke := kept.Lines[1]
	if !strings.HasPrefix(smoke.Detail, "failed — It said: ") || len(smoke.Detail) >= len(long.Lines[1].Detail) ||
		!strings.HasSuffix(smoke.Detail, "…") {
		t.Errorf("the smoke line kept %d bytes ending %q, want its opening with the cut marked",
			len(smoke.Detail), smoke.Detail[max(0, len(smoke.Detail)-12):])
	}
}
