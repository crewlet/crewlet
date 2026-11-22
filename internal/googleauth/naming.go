package googleauth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// Google holds a service account ID to 6 to 30 lowercase letters, digits and
// dashes, fixed at creation. Each of the engine's IDs is a prefix, a slug cut
// so the whole fits, a dash and 4 hex characters of a hash naming what the
// account is for, so two companies sharing an identity project, or two seats
// alike for the slug's length, never collide.
const (
	accountIDMax     = 30
	accountHashChars = 4
	growthPrefix     = "crewlet-g-"
	readerPrefix     = "crewlet-gr-"
)

// ServiceAccountID is the ID an account is created under: prefix, name
// slugified and cut to fit, and the hash of what. It labels the account and
// never identifies it: its owner keeps Google's unique ID. A name with no
// letter or digit is fallback.
func ServiceAccountID(prefix, name, fallback, what string) string {
	room := accountIDMax - len(prefix) - 1 - accountHashChars
	slug := org.Slugify(name)
	if len(slug) > room {
		slug = slug[:room]
	}
	slug = strings.TrimRight(slug, "-")
	if slug == "" {
		slug = fallback
	}
	sum := sha256.Sum256([]byte(what))
	return prefix + slug + "-" + hex.EncodeToString(sum[:])[:accountHashChars]
}

// AccountEmail is the email an account ID has in the identity project.
func AccountEmail(id, identity string) string {
	return id + "@" + identity + ".iam.gserviceaccount.com"
}

// GrowthAccountID is the ID of the growth identity of company's seat handle:
// the account Google's growth products know the seat as. Hashed over
// growth/<company>/<handle>, so it never collides with the seat's Google
// Cloud account, which hashes <company>/<handle>.
func GrowthAccountID(company, handle string) string {
	return ServiceAccountID(growthPrefix, handle, "agent",
		"growth/"+org.Slugify(company)+"/"+strings.TrimSpace(handle))
}

// ReaderAccountID is the ID of company's own read identity, which reads
// growth figures for the whole company. ONE PER COMPANY, never one fixed
// name: two companies may share an identity project, and a reader both
// companies' subjects could act as would let one read the other's figures.
func ReaderAccountID(company string) string {
	return ServiceAccountID(readerPrefix, company, "company", "reader/"+org.Slugify(company))
}
