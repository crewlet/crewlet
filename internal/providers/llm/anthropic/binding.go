package anthropic

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// What a thinking block is bound to, and what this backend does when that
// moved.
//
// Claude Fable 5.1, Opus 5.5 and Sonnet 5.5 bind each thinking block to the
// request that produced it: the top-level system prompt, the SET of tools —
// every definition whole, its name, description and input schema, compared by
// name and not by position — and every message before the block. A block
// replayed into a request where any of that differs is refused, and on an
// account the vendor enforces the refusal is a 400 the fallback chain does not
// retry. The messages are this engine's to keep append-only, and it does (see
// [formatMessages]); the system prompt is frozen for a phase. The TOOLS are
// not, and cannot be: `activate_tool` puts a new definition on the next
// round's request, and a resumed run renders its definitions again from a
// registry that may have moved while it was parked.
//
// So every assistant turn records the BINDING of the request that wrote it
// ([binding], on [llm.Message.Binding]), and a request to a model that runs
// the check ([claudemodel.Profile.PrefixBinding]) sheds the thinking of every
// turn up to and including the LAST one whose thinking was written under a
// binding other than its own ([shedThrough]). What survives is a run from the
// end of the conversation's sequence of thinking blocks, every one of them
// written under exactly this request's system prompt and tools — and dropping
// a run from the FRONT is the one removal the vendor's check accepts.
//
// THE CUT IS A PURE FUNCTION OF THE TURNS BEFORE IT AND THE CURRENT BINDING,
// and that is the whole proof that what it keeps is valid. A kept turn K was
// written under binding B, which is this request's; the request that wrote K
// had binding B too and the same turns before K (the conversation only grows),
// and it sent them as this function shapes them, so it shed exactly the turns
// this request sheds before K. Every message ahead of K — including a turn
// that shedding emptied and the merge of the user turns either side of it —
// and the thinking block ahead of K's own are therefore what they were when K
// was written. No history of tool sets is needed to reach that conclusion, so
// it holds for a set that grows (an activation), one that shrinks or is
// reworded (a resume), and one that comes back to what it was. The "last such
// turn", rather than every such turn, is what makes the kept blocks a single
// run: shedding a turn while keeping one before it would remove a block from
// the MIDDLE, which invalidates every block after it.
//
// "It sent them as this function shapes them" is a premise, and a model that
// runs no check breaks it: there nothing is shed, because every block is still
// valid to that model and shedding one only loses what it reasoned. A turn
// written by such a request while it was replaying reasoning the cut would
// have shed was written in a conversation no checked request will ever send
// again, so it records NO binding ([Provider.params]) and is shed in turn by
// the next checked request — rather than kept on the strength of a tool set
// that matched while the messages ahead of it did not. That is what a chain
// that falls back from Opus 5.5 to Opus 5 and back, or from Fable 5.1 to
// Mythos 5.1, costs after a tool change, and it is the price of a proof that
// rests on no claim about which model's blocks the vendor checks.
//
// The alternative that keeps the reasoning is the vendor's own append-only
// form of a tool change — every definition declared up front with
// `defer_loading` and each one announced by a `tool_addition` system message
// when it becomes available — and it is not used. It is a beta
// (`mid-conversation-tool-changes-2026-07-01`) that Microsoft Foundry and
// Sonnet 5 do not take and Bedrock takes only through InvokeModel, and it
// covers only a tool that ARRIVES: a definition a resume rendered differently
// needs `inline-tools-2026-09-15` as well, which only the Claude API serves.
// Nor is the vendor's `drop_block` setting, which drops the first mismatched
// block AND EVERY BLOCK AFTER IT: after one activation it would discard the
// reasoning of every later round too, where this keeps it.

// isThinking reports whether a content block of this type is reasoning — the
// blocks the vendor binds to the request that wrote them.
func isThinking(kind string) bool {
	return kind == "thinking" || kind == "redacted_thinking"
}

// replayed reports whether m is an assistant turn this backend wrote, which it
// replays from its own blocks rather than rebuilding from the neutral view.
func replayed(m llm.Message) bool {
	return m.Role == llm.RoleAssistant && m.Origin.Provider == providerName && len(m.Raw) > 0
}

// binding is the digest of what a request binds its thinking to beside its
// messages: the system prompt as sent, and the tool definitions as sent, as a
// SET.
//
// Each definition is read back from the bytes the SDK encodes for the wire, so
// it is exactly what the vendor compares — a rendering detail of the SDK (a
// schema's `type` pinned to object, a malformed `required` dropped) cannot make
// two different-looking definitions that send the same thing count as a
// change. Each is then put in one canonical form — keys in one order, no space
// between tokens, every number in the digits it was written in — with its
// cache breakpoint removed, because the vendor ignores both key order and
// `cache_control`, and the breakpoint sits on whichever tool is LAST: two
// requests offering the same tools in another order would otherwise differ on
// which definition carries it. The set is sorted, so order is not part of it.
//
// A difference this reads that the vendor would not (a schema re-spelling a
// number) costs reasoning that was still valid; a difference the vendor reads
// that this did not would be a 400. Every simplification is therefore in the
// first direction.
func binding(system string, tools []sdk.ToolUnionParam) (string, error) {
	defs := make([][]byte, 0, len(tools))
	for i, tool := range tools {
		raw, err := json.Marshal(tool)
		if err != nil {
			return "", fmt.Errorf("anthropic: tool %d (%s) cannot be encoded: %w", i, toolName(tool), err)
		}
		def, err := canonical(raw)
		if err != nil {
			return "", fmt.Errorf("anthropic: tool %d (%s): %w", i, toolName(tool), err)
		}
		defs = append(defs, def)
	}
	slices.SortFunc(defs, bytes.Compare)

	// Every part length-prefixed, so no two different requests can be one
	// byte string: the system prompt first, then each definition.
	h := sha256.New()
	part := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	part([]byte(system))
	for _, def := range defs {
		part(def)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// canonical is one JSON object in a single spelling: decoded with every number
// kept as its literal and re-encoded with its keys sorted, minus the
// `cache_control` key at its top level.
func canonical(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var object map[string]any
	if err := dec.Decode(&object); err != nil {
		return nil, fmt.Errorf("encoded definition is not a JSON object: %w", err)
	}
	delete(object, "cache_control")
	return json.Marshal(object)
}

func toolName(tool sdk.ToolUnionParam) string {
	if tool.OfTool != nil {
		return tool.OfTool.Name
	}
	return "unnamed"
}

// shedThrough is how many of messages' leading entries have their thinking
// shed: every one up to and including the last replayed turn that carries
// thinking written under a binding other than current, and zero when there is
// none. A turn written before bindings were recorded has none, and is shed —
// nothing can show that its blocks match.
//
// A turn that carries no thinking never sets the cut, whatever its binding:
// it has nothing the vendor could refuse, so shedding everything before it
// would lose reasoning for nothing. That includes every turn another backend
// wrote, which is rebuilt without thinking, so a chain that fell back to
// another vendor for a round and came back keeps the reasoning around it.
func shedThrough(messages []llm.Message, current string) (int, error) {
	shed := 0
	for i, m := range messages {
		if !replayed(m) || m.Binding == current {
			continue
		}
		for j, block := range m.Raw {
			head, err := headOf(block)
			if err != nil {
				return 0, fmt.Errorf("anthropic: message %d: replayed content block %d: %w", i, j, err)
			}
			if isThinking(head.Type) {
				shed = i + 1
				break
			}
		}
	}
	return shed, nil
}

// blockHead is the part of a content block this backend decides on.
type blockHead struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func headOf(block json.RawMessage) (blockHead, error) {
	var head blockHead
	if err := json.Unmarshal(block, &head); err != nil {
		return blockHead{}, fmt.Errorf("not a JSON object: %w", err)
	}
	return head, nil
}
