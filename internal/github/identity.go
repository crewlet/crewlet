package github

import "strings"

// How a seat is addressed on GitHub, and why it takes two spellings.
//
// AN AGENT ACTS AS ITS OWN APP, and GitHub gives one app two names. A mention
// carries the app's SLUG (`@acme-sre-lead`); every payload reporting what the
// app did carries the account's login, the slug with `[bot]` appended. No
// lookup relates them, so both are registered: the slug in the transport's
// own namespace, where mentions resolve, and the bot login in the one
// [notify.BotNamespace] names, where a payload's sender does.
//
// Neither costs a request: the login is the slug the engine wrote down when
// it created the app, where a personal access token needs a `GET /user`.

// botSuffix is what GitHub appends to an app's slug to make its account.
const botSuffix = "[bot]"

// NormalizeLogin folds a GitHub login to the one spelling this engine
// compares.
//
// GITHUB LOGINS ARE CASE-INSENSITIVE and payloads carry the canonical
// casing, while [Mentions] lowercases what a person typed. Compared exactly,
// a seat whose account is `SreLead` is woken by `@SreLead` and not by
// `@srelead`, which is the same person writing the same name and is the
// spelling most people type. So every login is folded once, at the two edges
// where one enters this package: a routing target and a registration.
func NormalizeLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

// BotLogin is the account a seat's own GitHub App acts as.
//
// Empty for an empty slug, which is a seat whose app has not been created
// yet: registering a bare `[bot]` would map every such seat onto one id and
// the second would be refused as a duplicate.
func BotLogin(slug string) string {
	slug = NormalizeLogin(slug)
	switch {
	case slug == "":
		return ""
	case strings.HasSuffix(slug, botSuffix):
		return slug
	default:
		return slug + botSuffix
	}
}
