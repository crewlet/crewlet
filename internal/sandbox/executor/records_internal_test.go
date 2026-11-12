package executor

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"
)

// The records a harness writes, as the readers that are no thread layer's
// read them: a run's event stream ([claudeEvents]), its result
// ([ClaudeCode.Parse]) and a person's transcript of a Claude Code or an
// OpenCode session ([transcriptBuilder]). The lists the records hold are
// lists_internal_test.go's.

// recordsOpening is the assistant line the stream reads before every line a
// case hands it: a message whose id and usage the line may name again, so
// the id the line's own message reads as is counted where the spend is.
const recordsOpening = `{"type":"assistant","message":{"id":"msg_test","role":"assistant","content":[` +
	`{"type":"text","text":"an opening"}],"usage":{"input_tokens":100,"cache_creation_input_tokens":10,` +
	`"cache_read_input_tokens":1000,"output_tokens":20}}}`

// recordsEntries is a transcript's entries, as text.
func recordsEntries(b *transcriptBuilder) string {
	shown := make([]string, 0, len(b.entries))
	for _, e := range b.entries {
		shown = append(shown, fmt.Sprintf("%s at %s, tool %q, failed %t: %q", e.Kind,
			e.At.Format(time.RFC3339Nano), e.Tool, e.Failed, e.Text))
	}
	return strings.Join(shown, " | ")
}

// readClaudeStream is what the run's event stream makes of line, read after
// [recordsOpening], as text: its transcript, every token its model calls
// reported, and the run's own spend its results stated.
func readClaudeStream(_ *testing.T, line string) string {
	d := &claudeEvents{tools: map[string]string{}}
	d.Line([]byte(recordsOpening))
	d.Line([]byte(line))
	own, stated := d.runSpend()
	return fmt.Sprintf("stream: %q, spent %d, own %+v (stated %t)\n", d.Result().Transcript, d.Spent(), own, stated)
}

// readClaudeRecord is everything the readers of a Claude Code record make of
// line, as text: the run's event stream's reading ([readClaudeStream]), the
// line read as the run's result, and a person's transcript of it.
func readClaudeRecord(t *testing.T, line string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(readClaudeStream(t, line))
	res := ClaudeCode{}.Parse(line)
	fmt.Fprintf(&b, "result: text %q, success %t, error %q, session %q, cost %v, tokens %d %d %d %d, turns %d, "+
		"context %t\n", res.Text, res.Success, res.Error, res.SessionID, res.CostUSD, res.InputTokens,
		res.OutputTokens, res.CacheReadTokens, res.CacheWriteTokens, res.Turns, res.ContextExhausted)
	builder := listBuilder()
	err := builder.claudeLine(t.Context(), []byte(line))
	fmt.Fprintf(&b, "transcript: %s; %v\n", recordsEntries(builder), err)
	return b.String()
}

// readOpenCodeMessage is what a person's transcript makes of message, one
// message of an OpenCode export.
func readOpenCodeMessage(t *testing.T, message string) string {
	t.Helper()
	b := listBuilder()
	err := b.openCodeNext(t.Context(), json.NewDecoder(strings.NewReader(message)))
	return fmt.Sprintf("transcript: %s; %v", recordsEntries(b), err)
}

// The records the cases below read, shaped as Claude Code and OpenCode write
// them, abridged to the keys these readers read; every id is synthetic.
const (
	recordsStamp = `"timestamp":"2026-01-02T03:04:05.000Z"`
	recordsUsage = `"usage":{"input_tokens":100,"cache_creation_input_tokens":10,"cache_read_input_tokens":1000,` +
		`"output_tokens":20}`
	// recordsReply is a model's reply of one text block, its message the one
	// recordsOpening names.
	recordsReply = `{"type":"assistant","message":{"id":"msg_test","role":"assistant","content":[{"type":"text",` +
		`"text":"a reply"}],` + recordsUsage + `},` + recordsStamp + `}`
	// recordsCall is a model's call of a shell command.
	recordsCall = `{"type":"assistant","message":{"id":"msg_call","role":"assistant","content":[{"type":"tool_use",` +
		`"id":"toolu_test","name":"Bash","input":{"description":"a test","command":"echo a"}}]},` + recordsStamp + `}`
	recordsBrief = `{"type":"user","message":{"role":"user","content":"the brief"},` + recordsStamp + `}`
	recordsMeta  = `{"type":"user","isMeta":true,"message":{"role":"user","content":"a reminder"},` + recordsStamp + `}`
	// recordsSummary is the record a compaction hands its summary on in.
	recordsSummary = `{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"the summary"},` +
		recordsStamp + `}`
	// recordsSteer is the line the run's own hook prints as it hands its model
	// a note.
	recordsSteer = `{"type":"system","subtype":"informational","content":"PostToolBatch says: crewlet: a note ` +
		`reached the run","level":"notice"}`
	// recordsEnding is a run's result that did not succeed: its errors, the
	// session's tokens and price, and what the run spent model by model.
	recordsEnding = `{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["it ran out"],` +
		`"session_id":"5e55100a-0000-4000-8000-000000000001","total_cost_usd":0.5,"num_turns":3,` +
		`"terminal_reason":"completed","usage":{"input_tokens":5,"output_tokens":6,"cache_read_input_tokens":7,` +
		`"cache_creation_input_tokens":8},"model_usage_since_last_result":{"claude-test":{"inputTokens":1,` +
		`"outputTokens":2,"cacheReadInputTokens":3,"cacheCreationInputTokens":4,"costUSD":0.25}}}`
	// recordsSpent is recordsEnding's per-model spend.
	recordsSpent = `"claude-test":{"inputTokens":1,"outputTokens":2,"cacheReadInputTokens":3,` +
		`"cacheCreationInputTokens":4,"costUSD":0.25}`

	recordsOpenCodeBrief = `{"type":"user","text":"the brief","time":{"created":1793361600000}}`
	recordsCompaction    = `{"type":"compaction","status":"completed","summary":"the summary",` +
		`"time":{"created":1793361600000}}`
	// recordsStep is an assistant step whose one tool failed.
	recordsStep = `{"type":"assistant","time":{"created":1793361600000},"content":[{"type":"tool","name":"bash",` +
		`"state":{"status":"error","input":{"command":"x"},"error":{"type":"aborted","message":"it stopped"}},` +
		`"time":{"created":1793361600000,"ran":1793361601000,"completed":1793361602000}}]}`
)

// A HARNESS'S RECORD IS READ BY ITS KEYS EXACTLY AS WRITTEN, the record's own
// as much as its lists', as Claude Code and OpenCode read their records with
// JSON.parse: a key that differs from one a reader reads only in case is
// another key, after that one, before it or alone, and a key written twice
// holds its last value. So one record is read by one rule, and the run's
// event stream and a person's transcript read one record alike: a message
// written twice is the last one written, to both. Every key these readers
// read is held so: a Claude Code record's type, subtype, timestamp, isMeta,
// isCompactSummary, message and content; a message's content, id and usage,
// and each count of a usage; a call's input as the stream names it; a
// result's type, subtype, is_error, result, errors, session_id,
// total_cost_usd, num_turns, terminal_reason, usage and per-run spend, and
// each figure of a model's spend; an OpenCode message's type, time, text,
// content, status and summary, and a failed call's failure. Each record reads
// through every reader as it reads without the other key, but for a call's
// input, which a person's transcript shows whole, as written, and which is
// held to the stream's reading of it.
//
// Mutation: decode the stream's line, a message, a usage, a call's input, the
// result, a model's spend, a transcript's record or message, an OpenCode
// message, its time or a failure by encoding/json, which takes a key that
// differs only in case for the one it names, and the record holding that key
// reads as the other key says; decode the stream's message into a struct, and
// a message written twice reads as the two merged.
func TestAHarnesssRecordIsReadByItsKeysExactlyAsWritten(t *testing.T) {
	t.Parallel()
	without := func(record, key string) string { return ctReplace(t, record, key, "") }
	noMessage := without(recordsReply, `"message":{"id":"msg_test","role":"assistant","content":[{"type":"text",`+
		`"text":"a reply"}],`+recordsUsage+`},`)
	quiet := `"message":{"id":"msg_test","role":"assistant",` + recordsUsage + `}`
	silent := ctReplace(t, recordsReply, `"message":{"id":"msg_test","role":"assistant","content":[{"type":"text",`+
		`"text":"a reply"}],`+recordsUsage+`}`, quiet)
	uncommanded := ctReplace(t, recordsCall, `,"command":"echo a"`, "")
	unerred := without(recordsEnding, `"errors":["it ran out"],`)
	// failedCall is a result a run reports as a success whose model call failed:
	// its is_error alone says it did not succeed.
	failedCall := ctReplace(t, recordsEnding, `"subtype":"error_during_execution"`, `"subtype":"success"`)
	for name, tc := range map[string]struct {
		record, from, to string
		// read is the readers the record is held to: every one, unless it
		// says otherwise.
		read func(*testing.T, string) string
	}{
		// A Claude Code record.
		"a record's type, then Type": {record: recordsReply, from: `"type":"assistant"`,
			to: `"type":"assistant","Type":"user"`},
		"a record's Type, then type": {record: recordsReply, from: `"type":"assistant"`,
			to: `"Type":"user","type":"assistant"`},
		"a record's Type alone": {record: without(recordsReply, `"type":"assistant",`), from: `"message"`,
			to: `"Type":"assistant","message"`},
		"a record's subtype, then Subtype": {record: recordsSteer, from: `"subtype":"informational"`,
			to: `"subtype":"informational","Subtype":"api_retry"`},
		"a system line's content, then Content": {record: recordsSteer, from: `,"level"`,
			to: `,"Content":"Stop says: crewlet: another note","level"`},
		"a record's timestamp, then Timestamp": {record: recordsBrief, from: recordsStamp,
			to: recordsStamp + `,"Timestamp":"2030-01-02T03:04:05.000Z"`},
		"a record's isMeta, then IsMeta": {record: recordsMeta, from: `"isMeta":true`,
			to: `"isMeta":true,"IsMeta":false`},
		"a record's IsMeta alone": {record: recordsBrief, from: `"type":"user"`, to: `"type":"user","IsMeta":true`},
		"a record's isCompactSummary, then IsCompactSummary": {record: recordsSummary,
			from: `"isCompactSummary":true`, to: `"isCompactSummary":true,"IsCompactSummary":false`},
		"a record's message, then Message": {record: recordsReply, from: `,` + recordsStamp,
			to: `,"Message":{"role":"assistant","content":[{"type":"text","text":"another reply"}]},` + recordsStamp},
		"a record's Message alone": {record: noMessage, from: recordsStamp,
			to: `"Message":{"role":"assistant","content":[{"type":"text","text":"another reply"}]},` + recordsStamp},
		"a record's message written twice, the last with no content": {record: silent, from: quiet,
			to: `"message":{"id":"msg_test","role":"assistant","content":[{"type":"text","text":"a reply"}],` +
				recordsUsage + `},` + quiet},
		// A message.
		"a message's content, then Content": {record: recordsReply, from: `"text":"a reply"}]`,
			to: `"text":"a reply"}],"Content":[{"type":"text","text":"another reply"}]`},
		"a message's id, then ID": {record: recordsReply, from: `"id":"msg_test"`,
			to: `"id":"msg_test","ID":"msg_other"`},
		"a message's usage, then Usage": {record: recordsReply, from: recordsUsage,
			to: recordsUsage + `,"Usage":{"input_tokens":5000,"output_tokens":7}`},
		"a usage's output_tokens, then Output_Tokens": {record: recordsReply, from: `"output_tokens":20`,
			to: `"output_tokens":20,"Output_Tokens":2000`},
		"a call's command, then COMMAND": {record: recordsCall, from: `"command":"echo a"`,
			to: `"command":"echo a","COMMAND":"echo another"`, read: readClaudeStream},
		"a call's COMMAND alone": {record: uncommanded, from: `"description":"a test"`,
			to: `"description":"a test","COMMAND":"echo another"`, read: readClaudeStream},
		// A result.
		"a result's type, then Type": {record: recordsEnding, from: `"type":"result"`,
			to: `"type":"result","Type":"assistant"`},
		"a result's subtype, then Subtype": {record: recordsEnding, from: `"subtype":"error_during_execution"`,
			to: `"subtype":"error_during_execution","Subtype":"error_max_turns"`},
		"a result's is_error, then Is_Error": {record: failedCall, from: `"is_error":true`,
			to: `"is_error":true,"Is_Error":false`},
		"a result's Result alone": {record: recordsEnding, from: `"is_error":true`,
			to: `"is_error":true,"Result":"a text under Result"`},
		"a result's errors, then Errors": {record: recordsEnding, from: `"errors":["it ran out"]`,
			to: `"errors":["it ran out"],"Errors":["another error"]`},
		"a result's Errors alone": {record: unerred, from: `"is_error":true`,
			to: `"is_error":true,"Errors":["an error under Errors"]`},
		"a result's session_id, then Session_Id": {record: recordsEnding,
			from: `"session_id":"5e55100a-0000-4000-8000-000000000001"`,
			to:   `"session_id":"5e55100a-0000-4000-8000-000000000001","Session_Id":"5e55100a-0000-4000-8000-000000000002"`},
		"a result's total_cost_usd, then Total_Cost_USD": {record: recordsEnding, from: `"total_cost_usd":0.5`,
			to: `"total_cost_usd":0.5,"Total_Cost_USD":9.5`},
		"a result's num_turns, then Num_Turns": {record: recordsEnding, from: `"num_turns":3`,
			to: `"num_turns":3,"Num_Turns":30`},
		"a result's terminal_reason, then Terminal_Reason": {record: recordsEnding,
			from: `"terminal_reason":"completed"`, to: `"terminal_reason":"completed","Terminal_Reason":"prompt_too_long"`},
		"a result's usage, then Usage": {record: recordsEnding, from: `"cache_creation_input_tokens":8}`,
			to: `"cache_creation_input_tokens":8},"Usage":{"input_tokens":50}`},
		"a result's per-run spend, then Model_Usage_Since_Last_Result": {record: recordsEnding,
			from: recordsSpent + `}`, to: recordsSpent + `},"Model_Usage_Since_Last_Result":{"claude-other":{"inputTokens":9}}`},
		"a model's inputTokens, then InputTokens": {record: recordsEnding, from: `"inputTokens":1`,
			to: `"inputTokens":1,"InputTokens":10`},
		"a model's spend named twice": {record: recordsEnding, from: recordsSpent,
			to: `"claude-test":{"inputTokens":90,"costUSD":9},` + recordsSpent},
	} {
		read := tc.read
		if read == nil {
			read = readClaudeRecord
		}
		variant := ctReplace(t, tc.record, tc.from, tc.to)
		if got, want := read(t, variant), read(t, tc.record); got != want {
			t.Errorf("%s reads as\n%swant\n%s", name, got, want)
		}
	}

	untyped := without(recordsOpenCodeBrief, `"type":"user",`)
	unsaid := ctReplace(t, recordsStep, `,"message":"it stopped"`, "")
	for name, tc := range map[string]struct {
		message, from, to string
	}{
		"an OpenCode message's type, then Type": {message: recordsOpenCodeBrief, from: `"type":"user"`,
			to: `"type":"user","Type":"assistant"`},
		"an OpenCode message's Type alone": {message: untyped, from: `"text"`, to: `"Type":"user","text"`},
		"an OpenCode message's text, then Text": {message: recordsOpenCodeBrief, from: `"text":"the brief"`,
			to: `"text":"the brief","Text":"another brief"`},
		"an OpenCode message's time, then Time": {message: recordsOpenCodeBrief, from: `"time":{"created":1793361600000}`,
			to: `"time":{"created":1793361600000},"Time":{"created":1893361600000}`},
		"an OpenCode message's created, then Created": {message: recordsOpenCodeBrief, from: `"created":1793361600000`,
			to: `"created":1793361600000,"Created":1893361600000`},
		"an OpenCode message's status, then Status": {message: recordsCompaction, from: `"status":"completed"`,
			to: `"status":"completed","Status":"running"`},
		"an OpenCode message's summary, then Summary": {message: recordsCompaction, from: `"summary":"the summary"`,
			to: `"summary":"the summary","Summary":"another summary"`},
		"an OpenCode message's content, then Content": {message: recordsStep, from: `]}`,
			to: `],"Content":[{"type":"text","text":"another step"}]}`},
		"a failure's message, then Message": {message: recordsStep, from: `"message":"it stopped"`,
			to: `"message":"it stopped","Message":"another failure"`},
		"a failure's Message alone": {message: unsaid, from: `"type":"aborted"`,
			to: `"type":"aborted","Message":"it stopped"`},
	} {
		variant := ctReplace(t, tc.message, tc.from, tc.to)
		if got, want := readOpenCodeMessage(t, variant), readOpenCodeMessage(t, tc.message); got != want {
			t.Errorf("%s reads as\n%s\nwant\n%s", name, got, want)
		}
	}
}

// A KEY OF ANOTHER KIND THAN ITS READER READS COSTS THAT KEY ALONE: a harness
// that reshapes a key costs the reader what that key said, and never the
// record around it, which reads as it reads without the key. A result's
// turns, its usage or a model's figure, a message's count, a record's isMeta
// and an OpenCode message's time, each of another kind, read as absent.
//
// Mutation: decode a result's num_turns or a record's isMeta into a field of
// its type, held strictly, and the result or the record that reshapes it is
// no record at all.
func TestAKeyOfAnotherKindCostsItsReaderThatKeyAlone(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		record, key, reshaped string
	}{
		"a result's num_turns, a string": {record: recordsEnding, key: `"num_turns":3,`,
			reshaped: `"num_turns":"3",`},
		"a result's usage, a string": {record: recordsEnding, key: `"usage":{"input_tokens":5,"output_tokens":6,` +
			`"cache_read_input_tokens":7,"cache_creation_input_tokens":8},`, reshaped: `"usage":"none",`},
		"a result's total_cost_usd, a list": {record: recordsEnding, key: `"total_cost_usd":0.5,`,
			reshaped: `"total_cost_usd":[0.5],`},
		"a model's inputTokens, a fraction": {record: recordsEnding, key: `"inputTokens":1,`,
			reshaped: `"inputTokens":1.5,`},
		"a message's output_tokens, a string": {record: recordsReply, key: `,"output_tokens":20`,
			reshaped: `,"output_tokens":"20"`},
		"a record's isMeta, a string":    {record: recordsMeta, key: `"isMeta":true,`, reshaped: `"isMeta":"true",`},
		"a record's timestamp, a number": {record: recordsBrief, key: `,` + recordsStamp, reshaped: `,"timestamp":5`},
	} {
		absent := ctReplace(t, tc.record, tc.key, "")
		reshaped := ctReplace(t, tc.record, tc.key, tc.reshaped)
		if got, want := readClaudeRecord(t, reshaped), readClaudeRecord(t, absent); got != want {
			t.Errorf("%s reads as\n%swant it read as without the key\n%s", name, got, want)
		}
	}
	created := `"time":{"created":1793361600000}`
	for name, reshaped := range map[string]string{
		"an OpenCode message's time, a string":  `"time":"soon"`,
		"an OpenCode message's created, a list": `"time":{"created":[1793361600000]}`,
	} {
		absent := ctReplace(t, recordsOpenCodeBrief, `,`+created, "")
		if got, want := readOpenCodeMessage(t, ctReplace(t, recordsOpenCodeBrief, created, reshaped)),
			readOpenCodeMessage(t, absent); got != want {
			t.Errorf("%s reads as\n%s\nwant it read as without the key\n%s", name, got, want)
		}
	}
}

// recordsModels is a result's spend model by model naming n models, each
// spending one input token, two output tokens, three cache reads, four cache
// writes and half a dollar; where twice says so, the first model is also
// written once before them all, with another spend.
func recordsModels(n int, twice bool) string {
	const spend = `{"inputTokens":1,"outputTokens":2,"cacheReadInputTokens":3,"cacheCreationInputTokens":4,` +
		`"costUSD":0.5}`
	models := make([]string, 0, n+1)
	if twice {
		models = append(models, `"claude-test-0":{"inputTokens":90,"costUSD":9}`)
	}
	for i := range n {
		models = append(models, fmt.Sprintf(`"claude-test-%d":%s`, i, spend))
	}
	return "{" + strings.Join(models, ",") + "}"
}

// A RUN'S SPEND IS UNREAD WHERE ONE RESULT NAMES MORE MODELS THAN A RESULT IS
// READ FOR: a result's spend model by model is read for at most
// claudeResultModelsMax models, and one naming more is read for none of them,
// so the run's own spend reads as stated by no result, whatever the results
// before or after it stated: never the sum of the ones it could read, nor of
// the models before the bound, either of which would charge the run for some
// of its calls and not the rest. A result naming exactly that many reads as it
// always did, every model's spend summed, and one of them named twice counts
// once toward the bound, by its last spend. The line costs its reader nothing
// else: the result's failure still reaches the run's transcript.
//
// Mutation: drop the bound, and the result naming one model more is summed;
// sum the models before the bound, and the run is charged for part of that
// result's calls; read the result past it as one that states nothing, or let
// a later result's spend read again, and the run is charged the other
// result's sum.
func TestARunsSpendIsUnreadWhereAResultNamesMoreModelsThanAResultIsReadFor(t *testing.T) {
	t.Parallel()
	ending := func(models string) string { return ctReplace(t, recordsEnding, "{"+recordsSpent+"}", models) }
	atBound := ending(recordsModels(claudeResultModelsMax, false))
	twice := ending(recordsModels(claudeResultModelsMax, true))
	past := ending(recordsModels(claudeResultModelsMax+1, false))
	const (
		failed = "[error] the run ended: error_during_execution: it ran out"
		read   = "own {input:64 cacheRead:192 cacheWrite:256 output:128 costUSD:32} (stated true)"
		unread = "own {input:0 cacheRead:0 cacheWrite:0 output:0 costUSD:0} (stated false)"
	)
	for name, tc := range map[string]struct {
		lines []string
		want  string
	}{
		"a result naming the most models a result is read for": {lines: []string{atBound},
			want: fmt.Sprintf("%q, %s", failed, read)},
		"the same, its first model named twice": {lines: []string{twice},
			want: fmt.Sprintf("%q, %s", failed, read)},
		"a result naming one model more": {lines: []string{past},
			want: fmt.Sprintf("%q, %s", failed, unread)},
		"a result that states its spend, then one naming one model more": {lines: []string{recordsEnding, past},
			want: fmt.Sprintf("%q, %s", failed+"\n"+failed, unread)},
		"a result naming one model more, then one that states its spend": {lines: []string{past, recordsEnding},
			want: fmt.Sprintf("%q, %s", failed+"\n"+failed, unread)},
	} {
		d := &claudeEvents{tools: map[string]string{}}
		for _, line := range tc.lines {
			d.Line([]byte(line))
		}
		own, stated := d.runSpend()
		if got := fmt.Sprintf("%q, own %+v (stated %t)", d.Result().Transcript, own, stated); got != tc.want {
			t.Errorf("%s reads as\n%s\nwant\n%s", name, got, tc.want)
		}
	}
}

// A RUN'S SPEND IS UNREAD WHERE A RESULT STATES IT AS NO OBJECT: a result whose
// spend model by model is there and is null, a list, a string, a number or a
// list holding a spend is read for none of it, so the run's own spend reads as
// stated by no result, whatever the result before or after it stated, never as
// that other result's alone, which would leave this one's calls out of the
// run's sum. A result whose spend is an empty object states a spend of
// nothing, and one that carries no spend at all states nothing, so beside a
// result that states its spend, either leaves the run's spend that result's.
//
// Mutation: read a spend that is there and no object as one that states
// nothing, and the run's spend reads as the other result's alone.
func TestARunsSpendIsUnreadWhereAResultStatesItAsNoObject(t *testing.T) {
	t.Parallel()
	const (
		failed = "[error] the run ended: error_during_execution: it ran out"
		read   = "own {input:1 cacheRead:3 cacheWrite:4 output:2 costUSD:0.25} (stated true)"
		unread = "own {input:0 cacheRead:0 cacheWrite:0 output:0 costUSD:0} (stated false)"
	)
	spent := `"model_usage_since_last_result":{` + recordsSpent + `}`
	for name, tc := range map[string]struct{ spend, want string }{
		"null":                {spend: `"model_usage_since_last_result":null`, want: unread},
		"an empty list":       {spend: `"model_usage_since_last_result":[]`, want: unread},
		"a list of one spend": {spend: `"model_usage_since_last_result":[{` + recordsSpent + `}]`, want: unread},
		"a string":            {spend: `"model_usage_since_last_result":"none"`, want: unread},
		"a number":            {spend: `"model_usage_since_last_result":0`, want: unread},
		"an empty object":     {spend: `"model_usage_since_last_result":{}`, want: read},
	} {
		other := ctReplace(t, recordsEnding, spent, tc.spend)
		for order, lines := range map[string][]string{
			"after a result that states its spend":  {recordsEnding, other},
			"before a result that states its spend": {other, recordsEnding},
		} {
			d := &claudeEvents{tools: map[string]string{}}
			for _, line := range lines {
				d.Line([]byte(line))
			}
			own, stated := d.runSpend()
			got := fmt.Sprintf("%q, own %+v (stated %t)", d.Result().Transcript, own, stated)
			if want := fmt.Sprintf("%q, %s", failed+"\n"+failed, tc.want); got != want {
				t.Errorf("a result whose spend is %s, %s, reads as\n%s\nwant\n%s", name, order, got, want)
			}
		}
	}
	d := &claudeEvents{tools: map[string]string{}}
	d.Line([]byte(recordsEnding))
	d.Line([]byte(ctReplace(t, recordsEnding, ","+spent, "")))
	own, stated := d.runSpend()
	if got := fmt.Sprintf("own %+v (stated %t)", own, stated); got != read {
		t.Errorf("a result that states its spend, then one that carries none, reads as %s, want %s", got, read)
	}
}

// A RESULT'S SPEND MODEL BY MODEL READS AS ITS WHOLE DECODE READ IT, up to the
// bound on the models it is read for: read a model at a time, every result
// reads as the decode of the whole object into a map read it, so a model
// whose name is escaped is the one written plainly, a name holding a lone
// surrogate or a byte that is no UTF-8 is the one holding U+FFFD in its place,
// a model named twice reads by its last spend, names that differ only in case
// are two models, a spend that is no object reads as none and a figure of
// another kind as absent; an empty object states a spend of nothing, and a
// result naming the most models it is read for reads every one of them.
//
// Mutation: compare names as written, and a model escaped and written plainly
// reads as two; keep a model's first spend, and the model named twice reads
// by its first.
func TestAResultsSpendModelByModelReadsAsItsWholeDecodeReadIt(t *testing.T) {
	t.Parallel()
	whole := func(object string) map[string]claudeModelUsage {
		var models map[string]json.RawMessage
		if err := wireDecode([]byte(object), &models); err != nil {
			t.Fatalf("the whole decode reads %.80s as no object: %v", object, err)
		}
		spent := make(map[string]claudeModelUsage, len(models))
		for name, raw := range models {
			spent[name] = looseModelUsage(raw)
		}
		return spent
	}
	for name, object := range map[string]string{
		"a model escaped and written plainly": `{"claude-\u0061":{"inputTokens":1},"claude-a":{"inputTokens":2}}`,
		"a name holding a lone surrogate, and one holding U+FFFD": `{"cut \ud83d":{"inputTokens":1},` +
			`"cut \ufffd":{"outputTokens":2}}`,
		"a name holding a byte that is no UTF-8, and one holding U+FFFD": `{"bad ` + "\xff" + `":{"inputTokens":1},` +
			`"bad \ufffd":{"inputTokens":3}}`,
		"a model named twice": `{"m":{"inputTokens":90,"costUSD":9},"n":{"inputTokens":1},` +
			`"m":{"inputTokens":1,"costUSD":0.25}}`,
		"names that differ only in case": `{"Claude":{"inputTokens":1},"claude":{"inputTokens":2}}`,
		"spends and figures of other kinds": `{"a":1,"b":"x","c":null,"d":[{"inputTokens":1}],` +
			`"e":{"inputTokens":"1","outputTokens":1.5,"costUSD":[0.5],"cacheReadInputTokens":2}}`,
		"an empty object":                      `{}`,
		"the most models a result is read for": recordsModels(claudeResultModelsMax, true),
	} {
		r, ok := decodeClaudeResult(`{"type":"result","subtype":"success","model_usage_since_last_result":` +
			object + `}`)
		if !ok {
			t.Fatalf("%s: the result is no result", name)
		}
		got, stated, err := r.sinceLastResult()
		if want := whole(object); err != nil || !stated || !maps.Equal(got, want) {
			t.Errorf("%s reads as %+v (stated %t, %v), want %+v as its whole decode read it", name, got, stated, err,
				want)
		}
	}
}

// A RESULT'S SPEND MODEL BY MODEL COSTS ITS READER A MODEL AT A TIME: the
// run's event stream reads a result's per-run spend a model at a time, for at
// most claudeResultModelsMax models, each model's spend once, so a result line
// of listBytes naming a model in every key it holds costs its reader no more
// allocations than a line naming that many models, and a line naming one
// model over and over no more than a line naming it once, each beside no more
// bytes than a few copies of itself. Decoded whole into a map, a line naming
// a model in every key cost an allocation or more for each of its tens of
// thousands of models.
//
// Not parallel: the counts are the process's, and a parallel test's
// allocations would be counted with this one's.
//
// Mutation: decode the spend whole into a map, or drop the bound on the models
// it is read for, and the line naming a model in every key costs an
// allocation or more for every model; read a model's spend each time the walk
// meets it, or copy it anew each time, and the line naming one model over and
// over costs one for every time.
func TestAResultsSpendModelByModelCostsItsReaderAModelAtATime(t *testing.T) {
	// Each read is handed bytes made once, outside what is measured, as a
	// reader is handed a line it did not copy.
	stream := func(line string) func() {
		raw := []byte(line)
		return func() { (&claudeEvents{tools: map[string]string{}}).Line(raw) }
	}
	const (
		head = `{"type":"result","subtype":"success","is_error":false,"result":"done",` +
			`"model_usage_since_last_result":{`
		tail = `}}`
	)
	model := func(i int) string { return fmt.Sprintf(`"m%06x":{"inputTokens":1}`, i) }
	every, models := ctKeysRecord(t, head, model, tail, listBytes)
	over, times := ctKeysRecord(t, head, func(int) string { return model(0) }, tail, listBytes)
	if models <= claudeResultModelsMax {
		t.Fatalf("a line of %d bytes names %d models, no more than the %d a result is read for", listBytes, models,
			claudeResultModelsMax)
	}
	atBound := make([]string, claudeResultModelsMax)
	for i := range atBound {
		atBound[i] = model(i)
	}
	for name, tc := range map[string]struct{ line, reference, says string }{
		"a model in every key": {line: every, reference: head + strings.Join(atBound, ",") + tail,
			says: fmt.Sprintf("%d models, and a line naming %d", models, claudeResultModelsMax)},
		"one model over and over": {line: over, reference: head + model(0) + tail,
			says: fmt.Sprintf("one model %d times, and a line naming it once", times)},
	} {
		fewObjects, fewBytes := allocationsOf(stream(tc.reference))
		manyObjects, manyBytes := allocationsOf(stream(tc.line))
		if manyObjects > fewObjects+listSlack {
			t.Errorf("%s: a line naming %s cost %d allocations and %d", name, tc.says, manyObjects, fewObjects)
		}
		if manyBytes > fewBytes+listCopies*uint64(len(tc.line)) {
			t.Errorf("%s: a line of %d bytes naming %s cost %d bytes and %d: more than %d copies of the line",
				name, len(tc.line), tc.says, manyBytes, fewBytes, listCopies)
		}
	}
}
