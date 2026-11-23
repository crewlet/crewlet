package googleauth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seatname"
)

// Google holds a service account ID to 6 to 30 lowercase letters, digits and
// dashes, fixed at creation. Each of the engine's own IDs is a prefix, a slug
// cut so the whole fits, a dash and 4 hex characters of a hash naming what the
// account is for, so two companies sharing an identity project, or two seats
// alike for the slug's length, never collide. A seat's growth identity is
// named as every seat's identity at any vendor ([GrowthAccountID]).
const (
	accountIDMax     = 30
	accountHashChars = 4
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
// the account Google's growth products know the seat as, and the name a
// person adds there, so it is named as every seat's identity at any vendor
// ([seatname.For]): `crewlet-<handle>-<6 hex>`. Its 6-character hash never
// matches the 4 of the seat's Google Cloud account, so the two never collide.
func GrowthAccountID(company, handle string) string {
	return seatname.For(seatname.Google, company, handle)
}

// ReaderAccountID is the ID of company's own read identity, which reads
// growth figures for the whole company. ONE PER COMPANY, never one fixed
// name: two companies may share an identity project, and a reader both
// companies' subjects could act as would let one read the other's figures.
func ReaderAccountID(company string) string {
	return ServiceAccountID(readerPrefix, company, "company", "reader/"+org.Slugify(company))
}
