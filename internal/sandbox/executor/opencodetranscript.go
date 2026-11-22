package executor

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// transcript reads an OpenCode archive ([openCodeArchiveFormat]).
func (OpenCode) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return b.openCodeExport(ctx, archive)
}

// openCodeKind is which kind of message one message of an OpenCode export is:
// its `type`, the tag of OpenCode 2's `SessionMessage.Info`
// (packages/schema/src/session-message.ts), whose eleven kinds are the same at
// 2.0.32 and 2.2.5. Who writes each, and what a transcript makes of it, is
// [transcriptBuilder.openCodeMessage]'s.
type openCodeKind string

// The kinds of message an OpenCode export holds.
const (
	openCodeUser             openCodeKind = "user"
	openCodeAssistant        openCodeKind = "assistant"
	openCodeCompaction       openCodeKind = "compaction"
	openCodeSynthetic        openCodeKind = "synthetic"
	openCodeSystem           openCodeKind = "system"
	openCodeSkill            openCodeKind = "skill"
	openCodeShell            openCodeKind = "shell"
	openCodeIdle             openCodeKind = "idle"
	openCodeAgentSwitched    openCodeKind = "agent-switched"
	openCodeModelSwitched    openCodeKind = "model-switched"
	openCodeLocationSwitched openCodeKind = "location-switched"
)

// The statuses a tool call settles in (`SessionMessage.ToolState`): with its
// answer, or on a failure. A compaction that settled with its summary is
// `completed` too.
const (
	openCodeCompleted  = "completed"
	openCodeCallFailed = "error"
)

// openCodeMessage is one message of an OpenCode export: OpenCode 2's
// `SessionMessage.Info`, a flat object whose `type` is its kind
// ([openCodeKind]) and whose other fields are that kind's own. Decoded
// loosely: a retyped field reads as zero.
//
// THE CONTEXT A SESSION CARRIES reads it ([measuredPrompt]), off an export or
// a page of its message list; a person's transcript reads the same kinds off
// the one export walk ([openCodeReading]), so no two readers disagree about a
// message's kind. OpenCode 1.x's `{"info": …, "parts": …}` is not this
// document.
type openCodeMessage struct {
	// Type is the message's kind.
	Type openCodeKind `json:"type"`

	// Time is when OpenCode recorded the message, in milliseconds since the
	// epoch: when a person's message arrived, a step began, a compaction
	// started; and, on a step, when it settled, zero while it is in flight,
	// which only the measure of its request reads ([measuredPrompt]).
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`

	// Text is a person's words on a user message — and OpenCode's own on its
	// synthetic, system and skill messages, which no entry reads.
	Text string `json:"text"`

	// Content is an assistant step's, raw: the model's text, its reasoning and
	// the tools it called, in order ([openCodeContent]). Raw, so content that
	// is no list is said rather than read as a step that wrote nothing.
	Content json.RawMessage `json:"content"`

	// Status is a compaction's — running, completed or failed — and Summary
	// what a completed one hands the model in place of the conversation
	// before it.
	Status  string `json:"status"`
	Summary string `json:"summary"`

	// Tokens is what an assistant step's request measured, and Error, raw,
	// the failure the step ended on: absent, or null, where it ended without
	// one. The context a session carries is read off both ([measuredPrompt]).
	Tokens *openCodeTokens `json:"tokens"`
	Error  json.RawMessage `json:"error"`
}

// openCodeTokens is what one step's request measured (`TokenUsage.Info`):
// its prompt not served from a cache, and the cache's reads and writes beside
// it, which together are the whole prompt the request carried.
type openCodeTokens struct {
	Input int64 `json:"input"`
	Cache struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

// openCodeKeys is what one decode of an object of an export saw of its keys
// ([eachKeyInPlace]): which names of the decode's own set it met (seen, a bit
// a name), whether it met a name outside that set (other), and whether it met
// one of them twice (twice). The person reader reads none of it; the one
// export walk keeps the same facts of each frame ([scanExport]), by which a
// holdings reading holds a message and a part to their sets.
type openCodeKeys struct {
	seen         uint32
	other, twice bool
}

// note notes a name by its index in the decode's set, -1 where it is none of
// them: the export walk's facts ([scanExport]) and the decode's alike.
func (k *openCodeKeys) note(i int) {
	switch {
	case i < 0:
		k.other = true
		return
	case k.seen&(1<<i) != 0:
		k.twice = true
	}
	k.seen |= 1 << i
}

// has reports whether the decode met the name of index i.
func (k openCodeKeys) has(i int) bool { return k.seen&(1<<i) != 0 }

// within reports whether every name the decode met is one of mask's, and none
// twice.
func (k openCodeKeys) within(mask uint32) bool { return !k.other && !k.twice && k.seen&^mask == 0 }

// openCodeNames is names, each by its index: a decode's set ([openCodeKeys]).
func openCodeNames(names ...string) map[string]int {
	set := make(map[string]int, len(names))
	for i, name := range names {
		set[name] = i
	}
	return set
}

// The names an item of a step's content is decoded by ([openCodeItemWire]),
// each its index in [openCodeItemNames].
const (
	itemType = iota
	itemText
	itemName
	itemState
	itemTime
	itemID
	itemExecuted
	itemNative
	itemProviderState
	itemProviderResultState
)

var openCodeItemNames = openCodeNames("type", "text", "name", "state", "time", "id", "executed", "native",
	"providerState", "providerResultState")

// The names a message of an export is read by, each its index in
// [openCodeMessageNames]: those a person's transcript reads, the message's id,
// and every other name of an assistant step's, which a holdings reading holds a
// step to on the one export walk, whose message frame is this set
// ([exportFrameNames]).
const (
	msgID = iota
	msgType
	msgTime
	msgText
	msgContent
	msgStatus
	msgSummary
	msgAgent
	msgModel
	msgSnapshot
	msgFinish
	msgRawFinish
	msgProviderState
	msgNative
	msgCost
	msgTokens
	msgError
	msgRetry
	msgMetadata
)

var openCodeMessageNames = openCodeNames("id", "type", "time", "text", "content", "status", "summary",
	"agent", "model", "snapshot", "finish", "rawFinish", "providerState", "native", "cost", "tokens", "error",
	"retry", "metadata")

// openCodeFileText is what a file a tool answered with reads as in text: an
// image as a Claude Code tool's image reads ([claudeImagePlaceholder]), and
// any other file as a file. Its bytes, inline or behind a URI, are no text.
func openCodeFileText(mime string) string {
	if strings.HasPrefix(mime, "image/") {
		return claudeImagePlaceholder
	}
	return "[a file]"
}

// unixMillis is an export's instant, or zero where it carries none, or none an
// answer can carry ([carriedInstant]).
func unixMillis(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return carriedInstant(time.UnixMilli(ms).UTC())
}

// firstInstant is at, or otherwise where it is zero.
func firstInstant(at, otherwise time.Time) time.Time {
	if at.IsZero() {
		return otherwise
	}
	return at
}
