package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
)

// A provider's `base_url` — an LLM entry's or the embeddings provider's — is
// held to TWO RULES, and which class each belongs to is the whole point of
// this file.
//
// THE RUNNABLE RULE ([CheckBaseURL]): an http:// or https:// URL with a host.
// No call on any other value can be sent at all, so a revision carrying one
// cannot run on any build, and an apply refuses it.
//
// THE ADMISSION RULE ([BaseURLUnsent]): nothing the backend never sends. Both
// SDKs resolve each request's path against the base URL — the Anthropic SDK's
// and openai-go's alike, measured against a server
// (anthropic.TestABaseURLsQueryIsNeverSent, openai.TestABaseURLsQueryIsNeverSent)
// — and resolving a relative reference replaces the base's query and drops its
// fragment (RFC 3986 §5.2.2), so an `?api-version=…` or a gateway key written
// as `?key=…` reaches the endpoint as nothing at all, at an endpoint whose
// configuration looks complete. And USERINFO is sent only where nothing else
// takes the `Authorization` header: Go's HTTP client turns a URL's userinfo
// into Basic credentials only on a request that carries no Authorization of
// its own, and openai-go sends any key it holds as `Authorization: Bearer` —
// so on an openai, openai-compatible or embeddings entry with a key the
// userinfo never leaves the process, while the Anthropic SDK sends its key as
// `x-api-key` and the userinfo always goes out. Every one of these RUNS — the
// entry builds and every call is sent, only without the part — and a later
// build is free to start sending one (both SDKs can add a query per request),
// so a write keeping one is refused and a stored revision carrying one is
// applied with a warning; refused at apply, a revision such a newer peer
// admitted would split the fleet's epoch.
//
// A value written with ${VAR} references is judged on what it says around
// them, because what a variable holds is unknown until it resolves; the engine
// holds the resolved value to both rules when it builds the provider
// ([LLMProvider.ResolveBaseURL]), refusing what cannot be sent and warning
// about what resolution revealed is dropped.
//
// No error here repeats the value or any part of it: a query and userinfo are
// exactly where a credential gets written into a URL, and a validation error
// is printed, logged and pasted.

// BaseURLPart is a part of a base_url its backend never sends.
type BaseURLPart string

// The parts [BaseURLUnsent] reports.
const (
	BaseURLQuery    BaseURLPart = "query"
	BaseURLFragment BaseURLPart = "fragment"
	BaseURLUserinfo BaseURLPart = "userinfo"
)

// Valid reports whether part is one this build names.
func (part BaseURLPart) Valid() bool {
	switch part {
	case BaseURLQuery, BaseURLFragment, BaseURLUserinfo:
		return true
	default:
		return false
	}
}

// reason is the sentence a refusal of part says: why it is never sent, and
// where what it carried belongs instead.
func (part BaseURLPart) reason() string {
	switch part {
	case BaseURLQuery:
		return "carries a query, which is never sent: the provider's SDK resolves " +
			"each request's path against base_url, and that replaces the query. " +
			"Remove it. A key the endpoint takes belongs in api_keys (or api_key), " +
			"which is sent as the provider's own key header; an endpoint that will " +
			"only read a query parameter needs a proxy in front of it that adds one"
	case BaseURLFragment:
		return "carries a fragment, which is never part of a request: remove it"
	case BaseURLUserinfo:
		return "carries userinfo, which is never sent while this provider has a key: " +
			"the key goes out as `Authorization: Bearer`, and Go's HTTP client sends " +
			"a URL's userinfo as Basic credentials only on a request with no " +
			"Authorization header of its own. Put the gateway's credential in " +
			"api_keys (or api_key), where it is sent as the Bearer token; a gateway " +
			"that reads Basic credentials only is reached from an entry that runs " +
			"on no key at all"
	default:
		return fmt.Sprintf("carries a %s, which is never sent", string(part))
	}
}

// CheckBaseURL is the RUNNABLE half of the base_url rule: a value no call can
// be sent on is refused, and nothing else. Unset is not this rule's to refuse
// (the backend's default endpoint), and a value written with ${VAR} references
// is judged once they resolve ([LLMProvider.ResolveBaseURL]). The value is
// judged TRIMMED, which is how the engine hands it to a backend.
func CheckBaseURL(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" || envref.Has(s) {
		return nil
	}
	return checkSendable(s)
}

// checkSendable refuses s unless it is an http:// or https:// URL with a host.
func checkSendable(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errBaseURLUnsendable
	}
	return nil
}

// errBaseURLUnsendable is [CheckBaseURL]'s refusal.
var errBaseURLUnsendable = errors.New("is not an http:// or https:// URL with a host, " +
	"so no call on this provider could be sent: write the endpoint in full, scheme " +
	"included (https://gateway.example.com)")

// BaseURLUnsent is the ADMISSION half of the base_url rule: one error per part
// of raw its backend never sends, nil when it sends the whole of it. bearerKeyed
// is whether the provider sends a key as `Authorization: Bearer`, which is what
// makes userinfo unsent. A value written with ${VAR} references is judged on the
// text around them.
func BaseURLUnsent(raw string, bearerKeyed bool) []error {
	var out []error
	for _, part := range unsentParts(writtenText(raw), bearerKeyed) {
		out = append(out, errors.New(part.reason()))
	}
	return out
}

// writtenText is a WRITTEN value with every reference standing in as a neutral
// token, so a value's own text is what is judged: `${GW}?key=…` carries a
// query whatever GW holds, and `${GW}` alone carries nothing until it
// resolves. Trimmed, as every value here is judged.
func writtenText(raw string) string {
	s := strings.TrimSpace(raw)
	if envref.Has(s) {
		s, _ = envref.Expand(s, func(string) (string, bool) { return "x", true })
	}
	return s
}

// unsentParts is every part of the URL text s its backend never sends, in the
// order [BaseURLPart] declares them.
func unsentParts(s string, bearerKeyed bool) []BaseURLPart {
	if s == "" {
		return nil
	}
	var out []BaseURLPart
	// BY THE TEXT for the two delimiters, which mean one thing anywhere in
	// a URL (a literal one inside userinfo or a path must be escaped), so a
	// value too broken to parse is still judged — and an EMPTY query or
	// fragment counts, since `…/v1?` resolves to a request that drops it.
	query, fragment := strings.IndexByte(s, '?'), strings.IndexByte(s, '#')
	if query >= 0 && (fragment < 0 || query < fragment) {
		out = append(out, BaseURLQuery)
	}
	if fragment >= 0 {
		out = append(out, BaseURLFragment)
	}
	if bearerKeyed {
		if u, err := url.Parse(s); err == nil && u.User != nil {
			out = append(out, BaseURLUserinfo)
		}
	}
	return out
}

// ResolvedBaseURL is a provider's base_url as its backend is handed it.
type ResolvedBaseURL struct {
	// URL is the value with every reference resolved and the whole TRIMMED
	// once: the exact string both rules judged, and the exact string the
	// backend is handed — a value judged trimmed and sent untrimmed passed
	// every check and failed every call, with the whole URL, userinfo
	// included, in the SDK's parse error. "" for a field left unset or
	// resolving to nothing, which is the backend's default endpoint.
	URL string

	// From names the variables the value was written with.
	From []string

	// Missing names the references that resolved to nothing.
	Missing []string

	// Revealed is every part of URL its backend never sends that the WRITTEN
	// value's admission rule could not see — a part a variable carried, or
	// userinfo that is unsent only because of the conventional key the
	// entry resolved. The written value's own are reported as an admission
	// warning when the revision is applied, so these are what is left for
	// the build to say.
	Revealed []BaseURLPart
}

// ResolveBaseURL resolves this entry's base_url through r and judges the
// result: an error, naming the variables and never the value, when no call on
// it can be sent ([CheckBaseURL]); otherwise the value and what its backend
// drops of it ([ResolvedBaseURL.Revealed]), judged against the key the entry
// resolved — the conventional OPENAI_API_KEY an entry naming no api_keys reads
// included, which no write can see.
func (l *LLMProvider) ResolveBaseURL(r *Resolver) (ResolvedBaseURL, error) {
	resolvedKeyed := l.bearer() && len(l.ResolvedKeys(r)) > 0
	return resolveBaseURL(l.BaseURL, r, l.bearerKeyed(), resolvedKeyed)
}

// ResolveBaseURL is [LLMProvider.ResolveBaseURL] for the embeddings provider,
// whose backend is openai-go's on both types and so sends its key as Bearer.
func (e *EmbeddingProvider) ResolveBaseURL(r *Resolver) (ResolvedBaseURL, error) {
	return resolveBaseURL(e.BaseURL, r, strings.TrimSpace(e.APIKey) != "", e.ResolvedKey(r) != "")
}

// resolveBaseURL is the one resolution of a base_url: expand, trim once, judge
// that string, and report what resolution revealed beyond what the written
// text already said.
func resolveBaseURL(written string, r *Resolver, writtenKeyed, resolvedKeyed bool) (ResolvedBaseURL, error) {
	expanded, missing := r.Expand(written)
	out := ResolvedBaseURL{
		URL: strings.TrimSpace(expanded), From: envref.Names(written), Missing: missing,
	}
	if out.URL == "" {
		return out, nil
	}
	if err := checkSendable(out.URL); err != nil {
		if len(out.From) > 0 {
			return ResolvedBaseURL{}, fmt.Errorf("base_url, resolved from %s, %w",
				strings.Join(out.From, ", "), err)
		}
		return ResolvedBaseURL{}, fmt.Errorf("base_url %w", err)
	}
	said := unsentParts(writtenText(written), writtenKeyed)
	for _, part := range unsentParts(out.URL, resolvedKeyed) {
		if !containsPart(said, part) {
			out.Revealed = append(out.Revealed, part)
		}
	}
	return out, nil
}

// containsPart reports whether parts holds part.
func containsPart(parts []BaseURLPart, part BaseURLPart) bool {
	for _, p := range parts {
		if p == part {
			return true
		}
	}
	return false
}

// validateDials is [LLMProvider.validateDials] for the embeddings provider:
// the admission half of its base_url rule.
func (e *EmbeddingProvider) validateDials(path Path) error {
	var p problems
	for _, err := range BaseURLUnsent(e.BaseURL, strings.TrimSpace(e.APIKey) != "") {
		p.add(at(path, "base_url"), ErrConflict, "%v", err)
	}
	return p.err()
}
