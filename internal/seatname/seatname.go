// Package seatname is the one name Crewlet suggests or creates for an agent
// seat's identity at any vendor: a key, a user, a system user, a robot, a
// service account, a token. Every vendor package names a seat's identity with
// [For], so a person finds every agent's identity under one pattern at every
// vendor, and no vendor package invents a second.
//
// # The name
//
// `crewlet-<handle>`: lowercase letters and digits joined by hyphens, or by
// underscores where the vendor refuses hyphens ([Rule.Underscores]); no
// spaces, capitals, dots or other characters; cut to the vendor's length.
// Where the vendor's names are shared beyond one company (a Google identity
// project two companies may share, GitHub's app names), a short hash of the
// company and the handle keeps it unique ([Rule.Hashed]). The company is never
// spelled into the name: a name labels the identity for a person at the
// vendor, where the company is already the account they are in, and the
// vendor's own ID identifies it.
//
// # Each vendor's rule
//
// Each [Rule] is the vendor's published rule for that name, or Crewlet's own
// cap where the vendor publishes none ([Crewlet]). [For] never answers a name
// its rule refuses: [Rule.Check] holds every name it builds, and a handle with
// no letter or digit is named by the fallback `agent`.
package seatname

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// Prefix is what every seat's name begins with.
const Prefix = "crewlet"

// fallback names a seat whose handle holds no letter or digit.
const fallback = "agent"

// Rule is one vendor's rule for one name.
type Rule struct {
	// What is what the name names there ("Team key name").
	What string

	// Min and Max bound the name's length in bytes.
	Min, Max int

	// Underscores joins words with underscores, for a vendor that refuses
	// hyphens; hyphens otherwise.
	Underscores bool

	// Hashed adds HashChars hex characters of a hash of the company and the
	// handle, for a vendor whose names are shared beyond one company.
	Hashed    bool
	HashChars int

	// LetterFirst is a vendor whose names must begin with a letter, which
	// `crewlet` always does; kept so [Rule.Check] says so of any name.
	LetterFirst bool

	// Words is the rule as the sheet says it under the name.
	Words string
}

// Crewlet is the cap where a vendor publishes no rule: lowercase letters,
// digits and hyphens, 3 to 30. Thirty fits every vendor's field Crewlet has
// met and keeps a name readable in a vendor's list.
var Crewlet = Rule{What: "Name", Min: 3, Max: 30, LetterFirst: true,
	Words: "Lowercase letters, digits and hyphens, at most 30."}

// The vendors' rules, each the vendor's published one where it has one.
var (
	// AppStoreConnect is a team key's name: Apple publishes no rule.
	AppStoreConnect = with(Crewlet, "Team key name")

	// MetaAds is a system user's name: Meta publishes no rule.
	MetaAds = with(Crewlet, "System user name")

	// Expo is a robot's name: Expo publishes no rule.
	Expo = with(Crewlet, "Robot name")

	// DigitalOcean is a token's name: DigitalOcean publishes no rule.
	DigitalOcean = with(Crewlet, "Token name")

	// GitLab is a personal access token's name: GitLab publishes no rule.
	GitLab = with(Crewlet, "Token name")

	// Cloudflare is an account API token's name: at most 120
	// (developers.cloudflare.com/api/resources/accounts/subresources/tokens/methods/create/).
	Cloudflare = Rule{What: "Token name", Min: 1, Max: 120, LetterFirst: true,
		Words: "Cloudflare's rule: at most 120 characters."}

	// Scaleway is an IAM application's name, which its policy shares: at most
	// 64 (scaleway-sdk-go api/iam/v1alpha1, CreateApplicationRequest.Name).
	Scaleway = Rule{What: "Application name", Min: 1, Max: 64, LetterFirst: true,
		Words: "Scaleway's rule: at most 64 characters."}

	// Mattermost is a personal access token's description, by which the engine
	// knows the tokens it minted: at most 255 bytes (the server's
	// UserAccessToken.IsValid, mattermost/mattermost
	// server/public/model/user_access_token.go; the API docs state none).
	Mattermost = Rule{What: "Token description", Min: 1, Max: 255, LetterFirst: true,
		Words: "Mattermost's rule: at most 255 characters."}

	// Datadog is the local part of a service account's address: at most 64,
	// the longest local part mail allows (RFC 5321, section 4.5.3.1.1).
	Datadog = Rule{What: "Service account address (local part)", Min: 1, Max: 64, LetterFirst: true,
		Words: "Mail's rule: at most 64 characters before the @."}

	// Google is a service account's ID: 6 to 30 lowercase letters, digits and
	// hyphens, a letter first and no hyphen last, unique in an identity
	// project that companies may share, so hashed
	// (cloud.google.com/iam/docs/reference/rest/v1/projects.serviceAccounts/create).
	Google = Rule{What: "Google identity (service account ID)", Min: 6, Max: 30, LetterFirst: true,
		Hashed: true, HashChars: 6,
		Words: "Google's rule: lowercase letters, digits and hyphens, 6 to 30."}
)

// with is r naming what.
func with(r Rule, what string) Rule {
	r.What = what
	return r
}

// sep is the rule's separator.
func (r Rule) sep() string {
	if r.Underscores {
		return "_"
	}
	return "-"
}

// pattern is the whole-name shape the rule allows.
func (r Rule) pattern() *regexp.Regexp {
	s := regexp.QuoteMeta(r.sep())
	return regexp.MustCompile(`^[a-z0-9]+(?:` + s + `[a-z0-9]+)*$`)
}

// Check is what the rule refuses in name, nil where it allows it.
func (r Rule) Check(name string) error {
	var problems []string
	if len(name) < r.Min {
		problems = append(problems, fmt.Sprintf("shorter than %d", r.Min))
	}
	if len(name) > r.Max {
		problems = append(problems, fmt.Sprintf("longer than %d", r.Max))
	}
	if !r.pattern().MatchString(name) {
		problems = append(problems, "holds a character "+r.What+" refuses, or a separator at either end")
	}
	if r.LetterFirst && (name == "" || name[0] < 'a' || name[0] > 'z') {
		problems = append(problems, "does not begin with a letter")
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("seatname: %q: %s", name, strings.Join(problems, ", "))
}

// Slug is s as lowercase letters and digits joined by sep: every other run of
// characters is one separator, and none is left at either end.
func Slug(s, sep string) string {
	var b strings.Builder
	pending := false
	for _, c := range strings.ToLower(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			if pending && b.Len() > 0 {
				b.WriteString(sep)
			}
			pending = false
			b.WriteRune(c)
			continue
		}
		pending = true
	}
	return b.String()
}

// For is the name of company's seat handle under rule r: `crewlet-<handle>`,
// cut to fit, with the hash where r is hashed. It panics where the name breaks
// r, which only a rule too narrow for `crewlet` itself can cause: a programming
// error in a [Rule], never an input's.
func For(r Rule, company, handle string) string { return ForWith(r, company, handle) }

// ForWith is [For] with words after the handle, for a vendor where one seat
// holds several identities a person tells apart by name (a token per level):
// `crewlet-<handle>-<word>...`. The handle is cut first, so the words survive
// the vendor's length; only words too long to sit beside `crewlet` are cut.
func ForWith(r Rule, company, handle string, words ...string) string {
	sep := r.sep()
	slug := Slug(handle, sep)
	if slug == "" {
		slug = fallback
	}
	var tail string
	for _, w := range words {
		if w = Slug(w, sep); w != "" {
			tail += sep + w
		}
	}
	var hash string
	if r.Hashed {
		sum := sha256.Sum256([]byte(strings.TrimSpace(company) + "/" + strings.TrimSpace(handle)))
		hash = sep + hex.EncodeToString(sum[:])[:r.HashChars]
	}
	head := Prefix + sep + slug
	if room := r.Max - len(tail) - len(hash); len(head) > room {
		if room < len(Prefix) {
			head, room = head+tail, r.Max-len(hash)
			tail = ""
		}
		head = strings.TrimRight(head[:min(room, len(head))], sep)
	}
	name := head + tail + hash
	if err := r.Check(name); err != nil {
		panic(err)
	}
	return name
}
