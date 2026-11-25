package googlecloud

import (
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/org"
)

// Google holds a display name to 100 bytes and a description to 256.
const (
	accountPrefix  = "crewlet-"
	handleSlugMax  = 17
	displayNameMax = 100
	descriptionMax = 256
)

// crewletsOwn reports whether email is an account Crewlet keeps in the identity
// project: the provisioner, or one named with accountPrefix. The Cloud Shell
// block's preflight applies the same rule ([preflightLines]).
func crewletsOwn(email, provisioner string) bool {
	return email == provisioner || strings.HasPrefix(email, accountPrefix)
}

// AccountID is the ID one seat's Google Cloud service account is created
// under: `crewlet-`, a 17-character handle slug, a dash and 4 hex characters
// of the company and handle ([googleauth.ServiceAccountID]).
func AccountID(company, handle string) string {
	return googleauth.ServiceAccountID(accountPrefix, cut(org.Slugify(handle), handleSlugMax), "agent",
		org.Slugify(company)+"/"+strings.TrimSpace(handle))
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
