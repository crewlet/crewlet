package cards

import "strings"

// spelled are the words of a path, a field or a tool's name a person writes
// otherwise: initialisms in capitals, and the one abbreviation the API's paths
// and fields use for a word.
var spelled = map[string]string{
	"api": "API", "csv": "CSV", "dm": "DM", "iam": "IAM", "id": "ID", "json": "JSON",
	"llm": "LLM", "mcp": "MCP", "oauth": "OAuth", "sso": "SSO", "url": "URL",
	"xlsx": "XLSX", "yaml": "YAML", "config": "configuration",
}

// asWords is one path segment or field key as words: "mcp-servers" is "MCP
// servers", "agent_id" is "agent ID", "export.csv" is "export CSV".
func asWords(key string) string {
	parts := strings.FieldsFunc(key, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	for i, w := range parts {
		if s, ok := spelled[strings.ToLower(w)]; ok {
			parts[i] = s
		}
	}
	if len(parts) == 0 {
		return key
	}
	return strings.Join(parts, " ")
}

// Humanise is one path segment or field key as a person reads it, its first
// letter capitalised: "mcp-servers" is "MCP servers", "my-work" is "My
// work". ONE RULE for every name drawn from the API's own spelling: a generic
// request's title (internal/assistant/catalog's RequestTitle), a call on the
// act route by the tool it names ("Open DM") and an inferred card's labels.
func Humanise(key string) string { return capitalise(asWords(key)) }

// capitalise is s with its first letter in capitals.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Singular is the last word of a collection's name for one of it: "Roles" is
// "Role", "Policies" is "Policy", "Statuses" is "Status", "People" is "Person".
// A word that is not a plural ("Status", "Analysis", "Data") is returned as it
// is. ONE RULE for the two places a collection is named for one of its
// members: the title of a read of one item (internal/assistant/catalog's
// RequestTitle) and the noun an inferred list is counted with.
func Singular(noun string) string {
	head, word := "", noun
	if i := strings.LastIndexByte(noun, ' '); i >= 0 {
		head, word = noun[:i+1], noun[i+1:]
	}
	lower := strings.ToLower(word)
	switch {
	case lower == "people":
		word = word[:1] + "erson"
	case strings.HasSuffix(lower, "ies") && len(lower) > 3:
		word = word[:len(word)-3] + "y"
	case strings.HasSuffix(lower, "sses"), strings.HasSuffix(lower, "uses"),
		strings.HasSuffix(lower, "xes"), strings.HasSuffix(lower, "ches"),
		strings.HasSuffix(lower, "shes"):
		word = word[:len(word)-2]
	case strings.HasSuffix(lower, "s") && !strings.HasSuffix(lower, "ss") &&
		!strings.HasSuffix(lower, "us") && !strings.HasSuffix(lower, "is"):
		word = word[:len(word)-1]
	}
	return head + word
}
