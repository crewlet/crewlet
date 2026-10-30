package config

import (
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// Scope keys, spelled as the vendor matches them.
//
// A seat's project or space list, the company's reach and what the vendor
// holds are compared under these rules and no other, so a key typed in lower
// case is the same key everywhere it is compared: the document's validation,
// the access routes, the sync's reach check and the plan's drift check.

// JiraProjectKey is a Jira project key as Jira matches it: trimmed and
// upper-cased, which is how Jira stores every project key.
func JiraProjectKey(key string) string { return org.NormalizeScope(key) }

// ConfluenceSpaceKey is a space key as Confluence matches it: a global space's
// key trimmed and upper-cased, as Confluence stores it, and a personal space's
// (`~username`, `~<account id>`) trimmed and otherwise left exactly as
// written, because Confluence matches those case-sensitively and an
// upper-cased one names no space at all.
func ConfluenceSpaceKey(key string) string {
	key = strings.TrimSpace(key)
	if strings.HasPrefix(key, "~") {
		return key
	}
	return org.NormalizeScope(key)
}
