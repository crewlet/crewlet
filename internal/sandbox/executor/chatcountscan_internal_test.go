package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// chatCountHarness is how the differential counts one harness's chat file: a
// fresh count on the walk and its whole-line oracle, its fixtures and
// variants, the archive member its take names, and its release.
type chatCountHarness struct {
	name             string
	walk             func() chatCounter
	whole            func() countWhole
	fixtures         []chatFixture
	base             string
	variants, twices []string
	member           func(string) bool
	memberName       string
	release          string
}

// chatCountHarnesses is Qwen Code's and Gemini CLI's.
func chatCountHarnesses(t *testing.T) []chatCountHarness {
	t.Helper()
	return []chatCountHarness{
		{name: "qwen", walk: func() chatCounter { return new(qwenCounting) },
			whole:    func() countWhole { return new(qwenCountingWhole) },
			fixtures: chatFixtures(t, "qwen/0.28.4", "chat-*.jsonl"), base: qwenCountBase,
			variants: qwenCountVariants(), twices: qwenCountTwices(), member: qwenChatMember,
			memberName: qwenMember.Name, release: qwenRelease},
		{name: "gemini", walk: func() chatCounter { return newGeminiCounting() },
			whole:    func() countWhole { return newGeminiCountingWhole() },
			fixtures: chatFixtures(t, "gemini/0.70.0", "session-*.jsonl"), base: geminiCountBase,
			variants: geminiCountVariants(), twices: geminiCountTwices(), member: geminiChatMember,
			memberName: geminiMember.Name, release: geminiRelease},
	}
}

// chatCountHarnessOf is the harness of [chatCountHarnesses] named name.
func chatCountHarnessOf(t *testing.T, name string) chatCountHarness {
	t.Helper()
	i := slices.IndexFunc(chatCountHarnesses(t), func(h chatCountHarness) bool { return h.name == name })
	if i < 0 {
		t.Fatalf("no count harness %q", name)
	}
	return chatCountHarnesses(t)[i]
}

// qwenCountBase is a Qwen Code reply that anchors a count.
const qwenCountBase = `{"uuid":"a0","type":"assistant","message":{"role":"model","parts":[{"text":"ok"}]},` +
	`"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":10,"totalTokenCount":1010}}`

// qwenCountVariants is a line of every shape Qwen Code's count reads, each
// member in every shape its whole-line decode took or refused.
func qwenCountVariants() []string {
	long, past := strings.Repeat("i", 300), strings.Repeat("r", sandbox.RestartIDMaxBytes+1)
	at := strings.Repeat("r", sandbox.RestartIDMaxBytes)
	msg := func(kind, message string) string {
		return `{"uuid":"u1","type":"` + kind + `","message":` + message + `}`
	}
	var out []string
	for _, m := range []string{`null`, `"words"`, `5`, `[1,{"parts":[]}]`, `true`, `{}`, `{"parts":null}`,
		`{"parts":[]}`, `{"parts":"x"}`, `{"parts":[1]}`, `{"parts":[null]}`, `{"role":"user","parts":[{"text":"hi é 😀 \u0001"}]}`,
		` { "role" : "user" , "parts" : [ { "text" : "spaced" } ] } `,
		`{"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}`,
		`{"parts":[{"fileData":{"fileUri":"f"}},{"inlineData":null},{"inlineData":{},"fileData":{}}]}`,
		`{"parts":[{"functionCall":{"id":"c1","name":"n","args":{"a":1}}}]}`,
		`{"parts":[{"functionCall":{"id":"c2"}},{"functionCall":{"id":"c3"}}]}`,
		`{"parts":[{"functionResponse":{"id":"c1","name":"n","response":{"output":"o"}}}]}`,
		`{"parts":[{"functionResponse":{"id":"c2","parts":[{"inlineData":{}},null,{"fileData":{}}]}}]}`,
		`{"parts":[{"functionResponse":{"id":"c3","parts":null}}]}`,
		`{"parts":[{"functionResponse":{"id":"c3","parts":"x"}}]}`,
		// Images to the compaction's, which count, and as many beside a part
		// that does not read, which do not.
		`{"parts":[` + strings.Repeat(`{"inlineData":{}},`, qwenImageCompaction-1) + `{"fileData":{}}]}`,
		`{"parts":[` + strings.Repeat(`{"inlineData":{}},`, qwenImageCompaction) +
			`{"functionResponse":{"id":"c3","parts":"x"}}]}`,
		`{"parts":[{"functionResponse":{"id":"c3","parts":[1]}}]}`,
		`{"parts":[{"functionResponse":"an answer of words"},{"functionCall":{"id":"c9"}}]}`,
		`{"parts":[{"functionResponse":[1,2]}]}`, `{"parts":[{"functionResponse":5}]}`,
		`{"parts":[{"functionResponse":null},{"functionCall":null}]}`,
		`{"parts":[{"functionCall":{"id":5}}]}`, `{"parts":[{"functionCall":{}}]}`, `{"parts":[{"functionCall":"x"}]}`,
		`{"parts":[{"functionResponse":{"id":5}},{"inlineData":{}}]}`,
		`{"parts":[{"functionCall":{"id":"c4"},"functionResponse":{"id":"c4"}}]}`,
		`{"parts":[{"functionCall":{"id":"` + long + `"}}]}`,
		`{"parts":[{"functionResponse":{"id":"` + long + `","response":{}}}]}`,
		`{"parts":[{"functionResponse":{"id":"c1","response":{"output":"a \"quoted\" \\ ☃ answer"}},"text":"t"}]}`,
	} {
		for _, kind := range []string{"user", "assistant", "tool_result"} {
			out = append(out, msg(kind, m))
		}
	}
	for _, usage := range []string{`null`, `"x"`, `{}`, `{"promptTokenCount":null}`, `{"promptTokenCount":1.5}`,
		`{"promptTokenCount":"1"}`, `{"promptTokenCount":0,"totalTokenCount":500}`, `{"totalTokenCount":500}`,
		`{"promptTokenCount":100,"candidatesTokenCount":-5,"thoughtsTokenCount":3,"totalTokenCount":50}`,
		`{"promptTokenCount":100,"totalTokenCount":5000}`, `{"promptTokenCount":1e2}`, `5`,
		`{"promptTokenCount":-1,"totalTokenCount":9}`, `{"totalTokenCount":null}`, `{"promptTokenCount":200,"x":"y"}`} {
		out = append(out, `{"type":"assistant","message":{"parts":[{"text":"r"}]},"usageMetadata":`+usage+`}`,
			`{"usageMetadata":`+usage+`,"message":{"parts":[]},"type":"assistant"}`)
	}
	history := `[{"role":"user","parts":[{"text":"<state_snapshot>s</state_snapshot>"}]},` +
		`{"role":"model","parts":[{"functionCall":{"id":"h1"}},{"inlineData":{}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"id":"h1","response":{"o":"x"},"parts":[{"inlineData":{}}]}}]},` +
		`null,"a string",7,{"parts":"bad"}]`
	for _, payload := range []string{``, `null`, `"x"`, `{}`, `{"info":null,"compressedHistory":[]}`,
		`{"info":{"newTokenCount":500},"compressedHistory":` + history + `}`,
		`{"compressedHistory":` + history + `,"info":{"newTokenCount":500,"originalTokenCount":9}}`,
		`{"info":"x","compressedHistory":[]}`, `{"info":{"newTokenCount":1.5},"compressedHistory":[]}`,
		`{"info":{"newTokenCount":0},"compressedHistory":[]}`, `{"info":{"newTokenCount":null},"compressedHistory":[]}`,
		`{"info":{"newTokenCount":-3},"compressedHistory":[]}`,
		`{"info":{"newTokenCount":500},"compressedHistory":"x"}`, `{"info":{"newTokenCount":500},"compressedHistory":null}`,
		`{"info":{"newTokenCount":500}}`} {
		member := ``
		if payload != `` {
			member = `,"systemPayload":` + payload
		}
		for _, uuid := range []string{`"c0"`, `"` + at + `"`, `"` + past + `"`, `"` + long + `"`, `5`, `null`} {
			out = append(out, `{"uuid":`+uuid+`,"type":"system","subtype":"chat_compression"`+member+`}`)
		}
		out = append(out, `{"subtype":"chat_compression"`+member+`,"uuid":"c1","type":"system"}`)
	}
	return append(out, `{"uuid":"u","message":{"parts":[{"text":"no type"}]}}`,
		`{"type":null,"message":{"parts":[]}}`, `{"type":5}`, `{"type":"`+long+`"}`, `{"type":"system","subtype":5}`,
		`{"type":"system","subtype":"slash_command","message":{"parts":[{"text":"not counted"}]}}`,
		`{"type":"info","message":{"parts":[{"functionCall":{"id":"z"}}]}}`,
		`{"message":{"parts":[{"functionCall":{"id":"late"}}]},"type":"assistant"}`,
		`{"type":"user","Message":{"parts":[{"text":"another key"}]}}`)
}

// qwenCountTwices is a line of every member Qwen Code's count reads written
// twice ([twice]).
func qwenCountTwices() []string {
	return []string{
		`{"type":"user","type":"user","message":{"parts":[]}}`,
		`{"uuid":"a","uuid":"b","type":"system","subtype":"chat_compression","systemPayload":{}}`,
		`{"type":"system","subtype":"x","subtype":"chat_compression","systemPayload":{}}`,
		`{"type":"user","message":{"parts":[]},"message":{"parts":[{"text":"x"}]}}`,
		`{"type":"assistant","usageMetadata":{},"usageMetadata":{"promptTokenCount":5}}`,
		`{"type":"assistant","usageMetadata":{"promptTokenCount":5,"promptTokenCount":9}}`,
		`{"type":"system","subtype":"chat_compression","systemPayload":{},"systemPayload":{"info":{}}}`,
		`{"type":"system","subtype":"chat_compression","systemPayload":{"info":{"newTokenCount":5},"info":{}}}`,
		`{"type":"system","subtype":"chat_compression","systemPayload":{"info":{"newTokenCount":5,"newTokenCount":6},` +
			`"compressedHistory":[]}}`,
		`{"type":"system","subtype":"chat_compression","systemPayload":{"compressedHistory":[],"compressedHistory":[]}}`,
		`{"type":"user","message":{"parts":[],"parts":[]}}`,
		`{"type":"user","message":{"parts":[{"inlineData":{},"inlineData":null}]}}`,
		`{"type":"user","message":{"parts":[{"fileData":{},"fileData":null}]}}`,
		`{"type":"user","message":{"parts":[{"functionCall":{"id":"a"},"functionCall":{}}]}}`,
		`{"type":"user","message":{"parts":[{"functionCall":{"id":"a","id":"b"}}]}}`,
		`{"type":"user","message":{"parts":[{"functionResponse":{},"functionResponse":{"id":"a"}}]}}`,
		`{"type":"user","message":{"parts":[{"functionResponse":{"id":"a","id":"b"}}]}}`,
		`{"type":"user","message":{"parts":[{"functionResponse":{"parts":[],"parts":[]}}]}}`,
		`{"type":"user","message":{"parts":[{"functionResponse":{"parts":[{"inlineData":{},"inlineData":{}}]}}]}}`,
		`{"type":"user","message":{"parts":[{"functionResponse":{"parts":[{"fileData":{},"fileData":{}}]}}]}}`,
	}
}

// geminiCountBase is a Gemini CLI header and a person's record.
const geminiCountBase = `{"sessionId":"s","projectHash":"h","startTime":"t","kind":"main"}` + "\n" +
	`{"id":"b0","type":"user","content":[{"text":"hello"}]}`

// geminiCountVariants is a line of every shape Gemini CLI's count reads, each
// member in every shape its whole-line decode took or refused.
func geminiCountVariants() []string {
	long, past := strings.Repeat("i", 300), strings.Repeat("r", sandbox.RestartIDMaxBytes+1)
	at := strings.Repeat("r", sandbox.RestartIDMaxBytes)
	big := strings.Repeat("words ", 20_000)
	rec := func(id, kind, rest string) string { return `{"id":"` + id + `","type":"` + kind + `"` + rest + `}` }
	var out []string
	for _, content := range []string{`null`, `"a text é 😀"`, `"` + big + `"`, `"` + big + redact.Marker + `x]"`,
		`5`, `true`, `{"text":"one part"}`, `[]`, `[null]`, `[1]`, `[{"text":"t"},{"text":5}]`,
		`[{"text":"<state_snapshot>s</state_snapshot>"}]`, `[{"text":" \n\t<state_snapshot>s"}]`,
		`[{"text":" <state_snapshot>s"}]`, `[{"text":"<state_snap"}]`, `[{"text":"x"},{"text":"<state_snapshot>"}]`,
		`[{"text":"` + big + `"},{"text":"é` + big + `"},{"text":"` + big + redact.Marker + `"}]`,
		`[{"text":"a\u0000b\"c\\d\b\f\n\r\t\u001f \ud800 \udc00😀 ☃"}]`,
		`[{"functionResponse":{"id":"c1","name":"run","response":{"output":"o","n":[1,2.5,1e3,-0,true,null]}}}]`,
		`[{"functionResponse":{"name":"run","response":{"output":"no id"}}},{"functionResponse":{"id":"","response":"x"}}]`,
		`[{"functionResponse":{"id":5,"name":5}},{"functionResponse":{"id":"c2","response":null}},{"functionResponse":{"id":"c3"}}]`,
		`[{"functionResponse":{"id":"c4","response":{},"parts":[{"inlineData":{}}]}},{"functionResponse":{"id":"c5","parts":null}}]`,
		`[{"functionResponse":"x"}]`, `[{"functionResponse":null,"text":null,"inlineData":null,"fileData":null,"other":[1]}]`,
		`[{"inlineData":{"data":"AA=="}}]`, `[{"fileData":{"uri":"f"}}]`, `[{"text":"t","functionResponse":{"id":"c6"}}]`,
		`[{"functionCall":{"id":"c7","name":"n","args":{"a":[1,2.5e3,true,null,"s\n\u0001é😀"],"a":{"b":1.50}}}}]`,
		`[{"functionCall":{"name":"no id","args":{}}},{"functionCall":{"id":"","name":"empty"}},{"functionCall":{"id":5}}]`,
		`[{"functionCall":"x"}]`, `[{"functionCall":null,"thought":true}]`, `[{}]`, `[{"other":{"deep":[[[]]]}}]`,
		`[{"functionCall":{"id":"` + long + `","name":"` + long + `"}}]`,
		`[{"functionResponse":{"id":"` + long + `","name":"n","response":{}}}]`,
		`[{"functionCall":{"id":"c8","name":"n"},"functionResponse":{"id":"c8"}}]`,
	} {
		out = append(out, rec("r1", "user", `,"content":`+content), rec("r2", "gemini", `,"content":`+content))
	}
	for _, thoughts := range []string{`null`, `[]`, `[null]`, `"x"`, `[5]`, `[{"subject":"S","description":"D"}]`,
		`[{"subject":"","description":"only"}]`, `[{"description":"none"},{"subject":"s"}]`,
		`[{"subject":5,"description":"d"}]`, `[{"subject":"s","description":null}]`,
		`[{"subject":"` + big + `","description":"` + big + `"}]`, `[{"subject":"é","description":"` + big + `"}]`,
		`[{"subject":"` + redact.Marker + `","description":"` + big + `"}]`} {
		out = append(out, rec("r3", "gemini", `,"content":"","thoughts":`+thoughts))
	}
	for _, calls := range []string{`null`, `[]`, `[null]`, `"x"`, `[5]`,
		`[{"id":"t1","name":"n","args":{"q":"v"},"result":[{"functionResponse":{"id":"t1","name":"n","response":{"output":"o"}}}]}]`,
		`[{"id":"t2","args":{},"result":[{"functionResponse":{"name":"n","response":{}}}]}]`,
		`[{"id":"t3","name":null}]`, `[{"id":5,"name":"n","args":[1]}]`, `[{"name":"n"}]`, `[{"id":"t4","name":5}]`,
		`[{"id":"t5","name":"n","result":"x"}]`, `[{"id":"t6","name":"n","result":[1,null,{"functionResponse":null}]}]`,
		`[{"id":"t7","name":"n","result":[{"functionResponse":"x"}]}]`,
		`[{"id":"t8","name":"n","result":[{"functionResponse":{"parts":[1]}}]},{"id":"t9","name":"m"}]`,
		`[{"id":"` + long + `","name":"n","args":{}}]`} {
		out = append(out, rec("r4", "gemini", `,"content":"","toolCalls":`+calls))
	}
	for _, id := range []string{`"` + at + `"`, `"` + past + `"`, `"` + long + `"`, `""`, `5`, `null`} {
		out = append(out, geminiCompressionOf(id), `{"id":`+id+`,"type":"user","content":"plain"}`)
	}
	// A COMPRESSION IS ITS THREE LINES' FORMS ([geminiCompressionLines]): each
	// line but in its form, apart, or with white space about it.
	set, ack := `{"$set":{"sessionId":"s"}}`, `{"id":"a","timestamp":"t","type":"gemini","content":[{"text":"`+geminiAck+`"}]}`
	user := `{"id":"c","timestamp":"t","type":"user","content":[{"text":"<state_snapshot>s"}]}`
	out = append(out, set+"\n"+user, user+"\n"+ack, set+"\n"+user+"\n"+set+"\n"+ack,
		set+"\n"+`{"id":"c","type":"user","timestamp":"t","content":[{"text":"s"}]}`+"\n"+ack,
		set+"\n"+`{"id":"c","timestamp":"t","type":"user","content":"s"}`+"\n"+ack,
		`{"$set":{"sessionId":"s","x":1}}`+"\n"+user+"\n"+ack,
		set+"\n"+user+"\n"+`{"id":"a","timestamp":"t","type":"gemini","content":[{"text":"Got it."}]}`,
		set+"\n"+user+"\n"+strings.TrimSuffix(ack, "}")+`,"x":1}`,
		" "+set+" \n "+user+"\n"+ack+" ",
		set+"\n"+`{"id":"c","timestamp":"t","type":"user","content":[{"text":"`+strings.Repeat("w", 3*lineFormMaxBytes)+
			`"}]}`+"\n"+ack,
		set+"\n"+user+"\n"+ack+"\n"+set+"\n"+user+"\n"+ack)
	out = append(out, `{"type":"user","content":"no id"}`, rec("r5", "info", `,"content":"x"`),
		`{"id":"r6","content":"no type"}`, `{"id":"r7","type":null}`, `{"id":"r8","type":5}`,
		`{"content":[{"text":"type last"}],"id":"r9","type":"gemini"}`,
		`{"$set":{"lastUpdated":"t"}}`, `{"$set":null,"id":"r10","type":"user","content":"x"}`,
		`{"$rewindTo":"b0"}`, `{"sessionId":"s","type":"user","id":"r11","content":"x"}`, `{"sessionId":null}`,
		`{"$patch":{"updates":[{"id":"b0","content":[{"text":"rewritten"}]},{"id":"p1","type":"x",`+
			`"content":[{"functionCall":{"id":"pc","name":"n"}}]}],"removeIds":["p0","b0"]}}`,
		`{"$patch":{"removeIds":["b0",null]}}`, `{"$patch":{"removeIds":[5]}}`, `{"$patch":{"removeIds":"b0"}}`,
		`{"$patch":{"orderIds":["b0"]}}`, `{"$patch":{"orderIds":null,"updates":null}}`,
		`{"$patch":{"updates":[5]}}`, `{"$patch":{"updates":[null]}}`, `{"$patch":{"updates":"x"}}`, `{"$patch":5}`,
		`{"$patch":[]}`, `{"$patch":null,"id":"r12","type":"user","content":"x"}`,
		`{"$patch":{"updates":[{"content":"no id"}]}}`,
		`{"$patch":{"updates":[{"id":"`+long+`","content":[{"text":"<state_snapshot>"}]}]}}`,
		`{"id":"r13","type":"user","content":"a raw `+" "+` separator"}`,
		`{"id":"r14","type":"user","content":"an escaped   separator"}`,
		`{"id":"r15","type":"user","content":"an escaped \U2029 separator"}`,
		`{"id":"r16","type":"user","content":"six characters \\u2028"}`,
		`{"id":"r16","type":"user","content":"six characters \\U2029, the u upper case"}`,
		`{"id":"r17","type":"user","content":"a   space"}`,
		`{"id":"r18","type":"user","content":"`+"\xff"+` invalid"}`,
		`{"ID":"r19","type":"user","content":"another key"}`)
	out = append(out, geminiPieceEdgeLines()...)
	return append(out, geminiFormHeadEdgeLines()...)
}

// geminiPieceEdgeLines puts a separator or a marker across the edge of a
// line's first piece, at each byte it may split at: a separator is judged on
// the line's pieces, the first its first [lineChunkBytes], so `\u2028` and a
// raw U+2029 start [lineSeparatorMaxBytes] before it to one byte before; a
// marker on the text's, as long, so [redact.Marker] starts its length before
// the text's edge to one byte before. A text past 100,000 units holding a
// character not ASCII is charged otherwise for a marker.
func geminiPieceEdgeLines() []string {
	var out []string
	head := len(`{"id":"r20","type":"user","content":"`)
	for off := lineChunkBytes - head - lineSeparatorMaxBytes; off < lineChunkBytes-head; off++ {
		out = append(out, `{"id":"r20","type":"user","content":"`+strings.Repeat("a", off)+`\u2028"}`,
			`{"id":"r21","type":"user","content":"`+strings.Repeat("a", off)+"\u2029"+`"}`)
	}
	for off := lineChunkBytes - len(redact.Marker); off < lineChunkBytes; off++ {
		out = append(out, `{"id":"r22","type":"user","content":"`+strings.Repeat("a", off)+redact.Marker+
			strings.Repeat("b", 40_000)+`é"}`)
	}
	return out
}

// geminiFormHeadEdgeLines is a compression whose line meets the head a form
// is read within ([lineFormMaxBytes]): the summary's opening form ending a
// byte short of it, at it and a byte past it, and its reply, a whole form,
// led or trailed by white space to each of them, the line judged trimmed
// whole ([lineFormHead]); each torn as well (eachChatCountRecord).
func geminiFormHeadEdgeLines() []string {
	set := `{"$set":{"sessionId":"s"}}`
	ack := `{"id":"a","timestamp":"t","type":"gemini","content":[{"text":"` + geminiAck + `"}]}`
	user := `{"id":"c","timestamp":"t","type":"user","content":[{"text":"<state_snapshot>s"}]}`
	opening := len(`{"id":"c","timestamp":"` + `","type":"user","content":[`)
	var out []string
	for _, n := range []int{lineFormMaxBytes - 1, lineFormMaxBytes, lineFormMaxBytes + 1} {
		out = append(out, set+"\n"+`{"id":"c","timestamp":"`+strings.Repeat("t", n-opening)+
			`","type":"user","content":[{"text":"<state_snapshot>s"}]}`+"\n"+ack,
			set+"\n"+user+"\n"+strings.Repeat(" ", n-len(ack))+ack,
			set+"\n"+user+"\n"+ack+strings.Repeat(" ", n-len(ack)))
	}
	return out
}

// geminiCompressionOf is a Gemini CLI compression's three lines, its summary's
// record of the id, as JSON, given ([geminiCompressionLines]).
func geminiCompressionOf(id string) string {
	return `{"$set":{"sessionId":"s"}}` + "\n" + `{"id":` + id +
		`,"timestamp":"t","type":"user","content":[{"text":"<state_snapshot>s"}]}` + "\n" +
		`{"id":"a","timestamp":"t","type":"gemini","content":[{"text":"` + geminiAck + `"}]}`
}

// geminiCountTwices is a line of every member Gemini CLI's count reads written
// twice ([twice]).
func geminiCountTwices() []string {
	rec := func(rest string) string { return `{"id":"t","type":"user"` + rest + `}` }
	return []string{
		`{"id":"a","id":"b","type":"user","content":"x"}`, `{"id":"a","type":"x","type":"user","content":"x"}`,
		rec(`,"content":"x","content":"y"`), rec(`,"thoughts":[],"thoughts":[]`), rec(`,"toolCalls":[],"toolCalls":[]`),
		rec(`,"$set":{},"$set":{}`), rec(`,"$rewindTo":"a","$rewindTo":"b"`), `{"sessionId":"a","sessionId":"b"}`,
		`{"$patch":{},"$patch":{}}`, `{"$patch":{"updates":[],"updates":[]}}`, `{"$patch":{"removeIds":[],"removeIds":[]}}`,
		`{"$patch":{"orderIds":null,"orderIds":null}}`, `{"$patch":{"updates":[{"id":"a","id":"b"}]}}`,
		rec(`,"content":[{"text":"a","text":"b"}]`), rec(`,"content":[{"functionResponse":{},"functionResponse":{}}]`),
		rec(`,"content":[{"functionCall":{},"functionCall":{}}]`), rec(`,"content":[{"inlineData":{},"inlineData":{}}]`),
		rec(`,"content":[{"fileData":{},"fileData":{}}]`), rec(`,"content":[{"functionCall":{"id":"a","id":"b"}}]`),
		rec(`,"content":[{"functionCall":{"name":"a","name":"b"}}]`),
		rec(`,"content":[{"functionResponse":{"id":"a","id":"b"}}]`),
		rec(`,"content":[{"functionResponse":{"name":"a","name":"b"}}]`),
		rec(`,"content":[{"functionResponse":{"response":1,"response":2}}]`),
		rec(`,"content":[{"functionResponse":{"parts":1,"parts":2}}]`),
		rec(`,"thoughts":[{"subject":"a","subject":"b"}]`), rec(`,"thoughts":[{"description":"a","description":"b"}]`),
		rec(`,"toolCalls":[{"id":"a","id":"b","name":"n"}]`), rec(`,"toolCalls":[{"name":"a","name":"b"}]`),
		rec(`,"toolCalls":[{"name":"n","args":1,"args":2}]`), rec(`,"toolCalls":[{"name":"n","result":[],"result":[]}]`),
		rec(`,"toolCalls":[{"name":"n","result":[{"functionResponse":{},"functionResponse":{}}]}]`),
	}
}

// countReplayMaxBytes bounds the count recordings each variant is replayed
// after, a record a variant. SAFE because a variant after a recording meets
// only the state the count keeps of it, never its bytes: the ids it kept,
// calls left open and patched or removed records, a compression open or a
// restart named. The five Gemini CLI recordings past it (0.9 to 22.7 MB)
// carry those same kinds, each also in a smaller one (g2hi's and m4a's
// captures in g3as and c5hi; patches, removals and compressions in
// compressed.run1 and run2), and every variant at once still follows each.
// Replayed a variant at a time, they were nine tenths of the time.
const countReplayMaxBytes = 512 << 10

// eachChatCountRecord hands each every chat file the differential counts for
// h, one at a time: each recording whole, each variant alone after the base,
// torn, and after each count recording within [countReplayMaxBytes], every
// variant at once after the base and after each larger count recording, and
// lines at and past the bound.
func eachChatCountRecord(h chatCountHarness, each func(name, record string)) {
	var replayed, larger []chatFixture
	for _, f := range h.fixtures {
		each(f.name, strings.Join(f.lines, "\n")+"\n")
		switch {
		case !strings.Contains(f.name, "/count/"):
		case len(strings.Join(f.lines, "\n")) <= countReplayMaxBytes:
			replayed = append(replayed, f)
		default:
			larger = append(larger, f)
		}
	}
	for i, v := range append(h.variants, boundedLines()...) {
		each(fmt.Sprintf("line %d %.80q", i, v), h.base+"\n"+v+"\n"+h.base+"\n")
		each(fmt.Sprintf("torn line %d", i), h.base+"\n"+v)
		for _, f := range replayed {
			each(fmt.Sprintf("line %d after %s", i, f.name), strings.Join(f.lines, "\n")+"\n"+v+"\n")
		}
	}
	every := strings.Join(h.variants, "\n") + "\n"
	each("every line", h.base+"\n"+every)
	for _, f := range larger {
		each("every line after "+f.name, strings.Join(f.lines, "\n")+"\n"+every)
	}
	pad := func(n int) string {
		line := h.base[strings.LastIndexByte(h.base, '\n')+1:]
		return line[:len(line)-1] + `,"pad":"` + strings.Repeat("x", n-len(line)-len(`,"pad":""`)) + `"}`
	}
	each("a line at the bound", h.base+"\n"+pad(codexRolloutLineMaxBytes)+"\n"+h.base+"\n")
	each("a line past the bound", h.base+"\n"+pad(codexRolloutLineMaxBytes+1)+"\n"+h.base+"\n")
	each("a last line past the bound, torn", h.base+"\n"+pad(codexRolloutLineMaxBytes+1))
}

// A CHAT COUNT ON THE WALK IS ITS WHOLE-LINE READ: Qwen Code's and Gemini
// CLI's counts, read on the walk holding no line, are their whole-line reads'
// (chatcountwhole_internal_test.go) over every recorded chat file and a line
// of every shape each reads, alone, torn, after each count recording within
// [countReplayMaxBytes] and all at once, at and past the line bound; with what a take's scrub took out and
// without; and the meter reads the same in 17-byte pieces. A COUNT BELOW ITS
// ORACLE'S would hand a call a held telling never owed: no case may differ.
//
// Mutation: a Qwen Code message a byte short, a Gemini CLI text a quarter
// short; a part's answer, a call, a removal or a summary dropped; a line one
// member refuses read; images counted in parts that do not read; a separator
// escaped in upper case, or one or a marker across two pieces, missed.
func TestAChatCountOnTheWalkIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	for _, h := range chatCountHarnesses(t) {
		eachChatCountRecord(h, func(name, record string) {
			walk := h.walk()
			marks, err := countChat(h.release, h.release, func() error {
				return chatFileLines(t.Context(), strings.NewReader(record), walk, new(passBudget), nil)
			})
			whole := h.whole()
			wantMarks, wantErr := countWholeOf(t.Context(), whole, []byte(record), h.release, h.release)
			// A RECORDING HOLDING A KEY TWICE (round.key-twice, a record's type)
			// is unread on the walk, whatever its oracle reads ([twice]).
			twiced := strings.Contains(name, "key-twice")
			for _, taken := range []int64{0, 7, 1 << 20} {
				got, want := walk.count(marks, taken), whole.count(wantMarks, taken)
				if twiced && got.Mark == sandbox.CountUnread {
					continue
				}
				if fmt.Sprint(err) != fmt.Sprint(wantErr) || !sameCount(got, want) {
					t.Errorf("%s %s, %d taken: counts %s (%v), want %s (%v)", h.name, name, taken, countString(got),
						err, countString(want), wantErr)
				}
			}
			read, err := chatMeter(t.Context(), h.walk(), &exportPieces{r: bytes.NewReader(tarGzOf(t, h.memberName,
				[]byte(record))), n: 17}, h.member, h.release, h.release, new(passBudget))
			if want := whole.count(wantMarks, 0); (!twiced || read.count.Mark != sandbox.CountUnread) &&
				(fmt.Sprint(err) != fmt.Sprint(wantErr) || !sameCount(read.count, want)) {
				t.Errorf("%s %s in pieces: the meter counts %s (%v), want %s (%v)", h.name, name,
					countString(read.count), err, countString(want), wantErr)
			}
		})
	}
}

// A CHAT COUNT'S KEY WRITTEN TWICE LEAVES IT UNREAD: the whole-line decode
// merged an object written twice where the walk keeps the last, so neither
// reading is the one a harness's JSON.parse makes; no harness writes one, and
// a count that cannot say which is no bound. Every member either count reads
// is written twice, after an anchor.
//
// Mutation: drop the twice check of any one member: its row goes red.
func TestAChatCountsKeyWrittenTwiceLeavesItUnread(t *testing.T) {
	t.Parallel()
	for _, h := range chatCountHarnesses(t) {
		for _, line := range h.twices {
			walk := h.walk()
			if err := chatFileLines(t.Context(), strings.NewReader(h.base+"\n"+line+"\n"), walk, new(passBudget), nil); err != nil {
				t.Fatalf("%s %s: %v", h.name, line, err)
			}
			if got := walk.count(0, 0); got.Mark != sandbox.CountUnread {
				t.Errorf("%s %s: counts %s, want unread", h.name, line, countString(got))
			}
		}
	}
}

// A CHAT COUNT PAST ITS CALLS' BOUND IS UNREAD: a Qwen Code message of more
// than [qwenCountIDs] calls, or more left open, and a Gemini CLI record of
// more than [geminiCountBaseIDs] answers and calls, are unread rather than
// held; one at the bound is counted as its whole-line read counts it.
//
// Mutation: lift either bound, or count one more toward it: a case fails.
func TestAChatCountPastItsCallsBoundIsUnread(t *testing.T) {
	t.Parallel()
	calls := func(n int, part func(i int) string) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = part(i)
		}
		return strings.Join(parts, ",")
	}
	qwenOpen := func(from, n int) string {
		return `{"type":"assistant","message":{"parts":[` + calls(n, func(i int) string {
			return fmt.Sprintf(`{"functionCall":{"id":"c%d"}}`, from+i)
		}) + `]}}`
	}
	// ONE ID FOR EVERY CALL, so the record's entries pass the bound while the
	// count's ids stay far below its own ([geminiCountIDs]).
	geminiCall := func(int) string { return `{"functionCall":{"id":"c","name":"n"}}` }
	geminiRecord := func(n int) string {
		return `{"id":"g","type":"gemini","content":[` + calls(n, geminiCall) + `]}`
	}
	for _, c := range []struct {
		name   string
		h      int
		line   string
		unread bool
	}{
		{"a qwen message at the bound", 0, qwenOpen(0, qwenCountIDs), false},
		{"a qwen message past it", 0, qwenOpen(0, qwenCountIDs+1), true},
		{"qwen calls open past it", 0, qwenOpen(0, qwenCountIDs) + "\n" + qwenOpen(qwenCountIDs, 1), true},
		{"a gemini record at the bound", 1, geminiRecord(geminiCountBaseIDs), false},
		{"a gemini record past it", 1, geminiRecord(geminiCountBaseIDs + 1), true},
	} {
		h := chatCountHarnesses(t)[c.h]
		record := h.base + "\n" + c.line + "\n"
		walk := h.walk()
		if err := chatFileLines(t.Context(), strings.NewReader(record), walk, new(passBudget), nil); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := walk.count(0, 0)
		whole := h.whole()
		marks, _ := countWholeOf(t.Context(), whole, []byte(record), h.release, h.release)
		want := whole.count(marks, 0)
		switch {
		case c.unread && got.Mark != sandbox.CountUnread:
			t.Errorf("%s: counts %s, want unread", c.name, countString(got))
		case !c.unread && (got.Mark == sandbox.CountUnread || !sameCount(got, want)):
			t.Errorf("%s: counts %s, want %s", c.name, countString(got), countString(want))
		}
	}
}

// A GEMINI CLI COUNT PAST ITS IDS KEEPS NO MORE: once its ids pass
// [geminiCountIDs] the count is unread, and no record, answer, call or
// removal of a later line is held, so a session of any length holds the
// bound and the one line that passed it.
//
// Mutation: keep taking records or removals once full: the count holds more.
func TestAGeminiCountPastItsIDsKeepsNoMore(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString(geminiCountBase + "\n")
	for i := range geminiCountIDs + 1000 {
		fmt.Fprintf(&b, `{"id":"k%07d","type":"gemini","content":"x"}`+"\n", i)
	}
	b.WriteString(`{"$patch":{"removeIds":["k0000001","b0"]}}` + "\n")
	c := newGeminiCounting()
	if err := chatFileLines(t.Context(), strings.NewReader(b.String()), c, new(passBudget), nil); err != nil {
		t.Fatal(err)
	}
	held := len(c.quarters) + len(c.answers) + len(c.calls) + len(c.removed)
	if got := c.count(0, 0); got.Mark != sandbox.CountUnread || held > geminiCountIDs+1 {
		t.Errorf("a count past its ids reads %s holding %d ids; want unread, holding at most %d", countString(got),
			held, geminiCountIDs+1)
	}
}

// A CHAT COUNT'S RESTART NO COUNT CARRIES LEAVES IT UNREAD: a Qwen Code
// compression's uuid, or a Gemini CLI summary's record id, past
// [sandbox.RestartIDMaxBytes] or past the walk's word bound names no restart
// a record carries, so the count is unread as it is read; one at the bound
// is its restart.
//
// Mutation: drop the length check from either count: a case fails.
func TestAChatCountsRestartNoCountCarriesLeavesItUnread(t *testing.T) {
	t.Parallel()
	hs := chatCountHarnesses(t)
	for _, n := range []int{sandbox.RestartIDMaxBytes, sandbox.RestartIDMaxBytes + 1, 300} {
		id := strings.Repeat("r", n)
		for _, c := range []struct {
			h      chatCountHarness
			record string
		}{
			{hs[0], `{"uuid":"` + id + `","type":"system","subtype":"chat_compression","systemPayload":` +
				`{"info":{"newTokenCount":500},"compressedHistory":[]}}` + "\n"},
			{hs[1], hs[1].base + "\n" + geminiCompressionOf(`"`+id+`"`) + "\n"},
		} {
			walk := c.h.walk()
			if err := chatFileLines(t.Context(), strings.NewReader(c.record), walk, new(passBudget), nil); err != nil {
				t.Fatal(err)
			}
			got := walk.count(0, 0)
			restart, named := got.Restarted()
			switch {
			case n <= sandbox.RestartIDMaxBytes && (got.Mark != "" || restart != id):
				t.Errorf("%s, an id of %d: counts %s, want a bound restarted at it", c.h.name, n, countString(got))
			case n > sandbox.RestartIDMaxBytes && (got.Mark != sandbox.CountUnread || named):
				t.Errorf("%s, an id of %d: counts %s, want unread", c.h.name, n, countString(got))
			}
		}
	}
}

// sameCount reports whether two counts are the same.
func sameCount(a, b *sandbox.ResumedCount) bool { return countString(a) == countString(b) }

// countString is a count as the differential compares it.
func countString(c *sandbox.ResumedCount) string {
	if c == nil {
		return "none"
	}
	restart := "<none>"
	if c.Restart != nil {
		restart = fmt.Sprintf("%q", *c.Restart)
	}
	return fmt.Sprintf("{tokens %d mark %q window %d restart %s}", c.Tokens, c.Mark, c.Window, restart)
}

// tarGzOf is a gzipped tar of one regular member, name, holding body.
func tarGzOf(t testing.TB, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)),
		Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// chatSlotWait is a context that says when a walk first waits on it, as a
// wait for the large slot does ([passBudget.hold]); the walk itself asks only
// its Err.
type chatSlotWait struct {
	context.Context
	once   sync.Once
	waited chan struct{}
}

func (w *chatSlotWait) Done() <-chan struct{} {
	w.once.Do(func() { close(w.waited) })
	return w.Context.Done()
}

// A GEMINI CLI COUNT PAST ITS BASE HOLDS THE LARGE SLOT: a count keeping
// [geminiCountBaseIDs] ids takes no slot, held elsewhere or not; one keeping
// one more waits for it, reads on once it is freed and holds nothing once its
// walk ends, and is unread ([errLargeWait]) where its caller leaves while it
// waits.
//
// Mutation: take no slot past the base, or keep it past the walk: a case
// goes red.
func TestAGeminiCountPastItsBaseHoldsTheLargeSlot(t *testing.T) {
	records := func(n int) string {
		var b strings.Builder
		b.WriteString(geminiCountBase + "\n")
		for i := range n - 1 {
			fmt.Fprintf(&b, `{"id":"r%d","type":"user","content":"x"}`+"\n", i)
		}
		return b.String()
	}
	free := holdLargeSlot(t)
	c := newGeminiCounting()
	ctx := &chatSlotWait{Context: t.Context(), waited: make(chan struct{})}
	if err := chatFileLines(ctx, strings.NewReader(records(geminiCountBaseIDs)), c, new(passBudget), nil); err != nil {
		t.Fatalf("a count at its base: %v", err)
	}
	select {
	case <-ctx.waited:
		t.Error("a count at its base waited for the large slot")
	default:
	}
	free()
	for _, leaves := range []bool{false, true} {
		free := holdLargeSlot(t)
		leaving, leave := context.WithCancel(t.Context())
		ctx := &chatSlotWait{Context: leaving, waited: make(chan struct{})}
		c := newGeminiCounting()
		var marks countMarks
		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			marks, err = countChat(geminiRelease, geminiRelease, func() error {
				return chatFileLines(ctx, strings.NewReader(records(geminiCountBaseIDs+1)), c, new(passBudget), nil)
			})
		}()
		select {
		case <-ctx.waited:
		case <-done:
			t.Fatalf("leaves %v: a count past its base read to its end (%v) with the slot held elsewhere", leaves, err)
		}
		if leaves {
			leave()
		} else {
			free()
		}
		<-done
		count := c.count(marks, 0)
		switch {
		case leaves && (!errors.Is(err, errLargeWait) || count.Mark != sandbox.CountUnread):
			t.Errorf("a count whose caller left while it waited: %s (%v), want unread on the wait's error",
				countString(count), err)
		case !leaves && (err != nil || count.Mark != ""):
			t.Errorf("a count freed to read on: %s (%v), want a bound", countString(count), err)
		}
		leave()
		free()
		if n := len(collectionLargeReads); n != 0 {
			t.Errorf("leaves %v: once the count ended, %d large slots are held, want none", leaves, n)
		}
	}
}
