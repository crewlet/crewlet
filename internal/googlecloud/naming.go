package googlecloud

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/org"
)

// Google holds an account ID to 6 to 30 lowercase letters, digits and dashes,
// fixed at creation; a display name to 100 bytes and a description to 256.
// `crewlet-` and a 17-character handle slug, a dash and 4 hex characters of the
// company and handle are at most 30, so two companies sharing an identity
// project, or two handles alike for 17 characters, never collide.
const (
	accountPrefix    = "crewlet-"
	handleSlugMax    = 17
	accountHashChars = 4
	displayNameMax   = 100
	descriptionMax   = 256
)

// AccountID is the ID one seat's service account is created under. It labels
// the account and never identifies it: the ledger keeps its unique ID.
func AccountID(company, handle string) string {
	slug := strings.TrimRight(cut(org.Slugify(handle), handleSlugMax), "-")
	if slug == "" {
		slug = "agent"
	}
	sum := sha256.Sum256([]byte(org.Slugify(company) + "/" + strings.TrimSpace(handle)))
	return accountPrefix + slug + "-" + hex.EncodeToString(sum[:])[:accountHashChars]
}

// AccountEmail is the email an account ID has in the identity project.
func AccountEmail(id, identity string) string {
	return id + "@" + identity + ".iam.gserviceaccount.com"
}

// displayName is what the console lists the account as.
func displayName(company, handle string) string {
	return cut("Crewlet "+strings.TrimSpace(company)+" "+strings.TrimSpace(handle), displayNameMax)
}

// description is what the account says of itself to a person in the console.
func description(company, handle string) string {
	return cut("Managed by Crewlet for agent "+strings.TrimSpace(handle)+" of "+strings.TrimSpace(company)+
		". Do not edit.", descriptionMax)
}

// cut is s within limit bytes, never splitting a character.
func cut(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
