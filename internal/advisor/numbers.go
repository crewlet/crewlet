package advisor

import (
	"strconv"
	"strings"
)

// A MODEL'S NUMBERS READ AS THE ENGINE'S (ADR-0166). The engine says a token
// count as 2.8M and any other count with its thousands grouped (698,252); a
// model writes bare digits ("rose from 698252 to 2798109"). Once its words
// are checked, every number of five digits or more in them is said the
// engine's way, a token count where the finding states it as one.

// SayNumbers is f with every long number in its words said as the engine
// says a fact.
func SayNumbers(f Finding) Finding {
	tokens := TokenValues(f.Evidence)
	f.Headline, f.Title = NumbersSaid(f.Headline, tokens), NumbersSaid(f.Title, tokens)
	f.Why, f.Item = NumbersSaid(f.Why, tokens), NumbersSaid(f.Item, tokens)
	return f
}

// TokenValues is every number facts state as a token count, now or before.
func TokenValues(facts []Fact) map[int64]bool {
	out := map[int64]bool{}
	for _, f := range facts {
		if f.Counts() != UnitTokens && !strings.Contains(strings.ToLower(f.Label+" "+f.Key), "token") {
			continue
		}
		out[f.Value] = true
		if f.Was != nil {
			out[*f.Was] = true
		}
	}
	return out
}

// NumbersSaid is s with every bare number of five digits or more grouped by
// thousands, or said as a token count where tokens holds it. A number inside
// a key, an id, a decimal or an already grouped number is left as written.
func NumbersSaid(s string, tokens map[int64]bool) string {
	digit := func(b byte) bool { return b >= '0' && b <= '9' }
	word := func(b byte) bool {
		return digit(b) || b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if !digit(s[i]) {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && digit(s[j]) {
			j++
		}
		run := s[i:j]
		bare := len(run) >= 5
		if i > 0 {
			left := s[i-1]
			joined := (left == '.' || left == ',') && i > 1 && digit(s[i-2])
			bare = bare && !word(left) && !joined && !strings.ContainsRune("-/:$#", rune(left))
		}
		if j < len(s) {
			right := s[j]
			joined := (right == '.' || right == ',') && j+1 < len(s) && digit(s[j+1])
			bare = bare && !word(right) && !joined
		}
		if n, err := strconv.ParseInt(run, 10, 64); bare && err == nil {
			if tokens[n] {
				run = tokensSaid(n)
			} else {
				run = groupedInt(n)
			}
		}
		b.WriteString(run)
		i = j
	}
	return b.String()
}
