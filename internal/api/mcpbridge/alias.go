package mcpbridge

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// aliasHashLen is the hex digits of a name's SHA-256 an alias ends in: 32
// bits, so two of a seat's tools alias alike only by a hash collision, which
// [aliased] still resolves, while leaving 40 characters of the name readable.
const aliasHashLen = 8

// aliasOf is the name the engine's own server lists a tool under whose own
// name it cannot list ([harnessprofile.BridgeToolName]): the name's runs of
// letters and digits joined by `_`, cut to fit, then `_` and [aliasHashLen]
// hex digits of the name's SHA-256 (the package doc, "A name `crewlet` cannot
// list"). round is 0 but where [aliased] met a collision.
func aliasOf(name string, round int) string {
	words := strings.FieldsFunc(name, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	})
	keep := max(0, harnessprofile.ToolNameMax()-2-aliasHashLen)
	base := strings.Join(words, "_")
	if len(base) > keep {
		base = strings.TrimRight(base[:keep], "_")
	}
	hashed := name
	if round > 0 {
		hashed = strconv.Itoa(round) + "\x00" + name
	}
	sum := sha256.Sum256([]byte(hashed))
	digest := hex.EncodeToString(sum[:])[:aliasHashLen]
	if base == "" {
		return digest
	}
	return base + "_" + digest
}

// aliased is offered as the engine's own server lists it, each tool whose
// name it cannot list renamed to its alias ([aliasOf]), and the real name of
// each alias. A name the server lists as it is keeps it, and an alias meeting
// a name already listed takes the next round, tried in the order of the real
// names, so the answer depends on the set alone.
func aliased(offered []toolsession.Tool) (out []toolsession.Tool, toolOf map[string]string) {
	taken := map[string]bool{}
	var renamed []string
	for _, tool := range offered {
		if harnessprofile.BridgeToolName(tool.Def.Name) {
			taken[tool.Def.Name] = true
		} else {
			renamed = append(renamed, tool.Def.Name)
		}
	}
	if len(renamed) == 0 {
		return offered, nil
	}
	slices.Sort(renamed)
	alias := make(map[string]string, len(renamed))
	toolOf = make(map[string]string, len(renamed))
	for _, name := range renamed {
		a := aliasOf(name, 0)
		for round := 1; taken[a]; round++ {
			a = aliasOf(name, round)
		}
		taken[a] = true
		alias[name], toolOf[a] = a, name
	}
	out = make([]toolsession.Tool, len(offered))
	for i, tool := range offered {
		if a, ok := alias[tool.Def.Name]; ok {
			tool.Def.Name = a
		}
		out[i] = tool
	}
	return out, toolOf
}
