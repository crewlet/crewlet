package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
)

// The DEPLOYMENT BASES — `integrations.public_base_url`, where vendors reach
// this deployment, and `integrations.dashboard_base_url`, where people do —
// are held to ONE ORIGIN JUDGE ([CheckOrigin]) at every place a value enters:
// the text a document writes (validation), the value a `${VAR}` resolves to
// (where the base is read, [Integrations.WebhookBase]), and the `-public-url`
// a provisioning command is handed. A judge applied only to the written text
// never reached the documented form, a whole `${VAR}`, whose value it never
// saw: a variable holding `https://crewlet.example.com?tenant=a` registered
// `https://crewlet.example.com?tenant=a/webhooks/gitlab` — a request for `/`
// — at every vendor, each reporting a healthy hook that delivered nowhere.

// Where each base is used, said beside every refusal of it.
const (
	publicBaseUse = "It is the base every webhook a vendor delivers to and every " +
		"app address is built on, so a value no address can be built on yields " +
		"hooks the vendor accepts and never reaches"
	dashboardBaseUse = "It is the base every link an agent composes for a person " +
		"is built on, so a value no address can be built on yields links nobody " +
		"can follow"
)

// CheckOrigin judges a deployment base as the text it is: an http:// or
// https:// URL with a host, carrying no query, no fragment and no userinfo.
// Every consumer appends its own rooted path to the base as TEXT, so a query
// swallows that path (`https://x.example.com?k=v/webhooks/gitlab` is a request
// for `/`), a fragment is never sent at all, and userinfo would be pasted into
// every manifest a vendor holds and every link a person is handed. A PATH is
// allowed: it sits in front of every path the engine appends, which is how a
// deployment behind a path-routing proxy is reached.
//
// One refusal, the first that applies, and it repeats none of the value: a
// query and userinfo are where a credential is written into a URL. Empty is
// not this judge's to refuse — an unset base means "no address".
func CheckOrigin(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	if u, err := url.Parse(s); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errOriginScheme
	}
	return originLeftover(s, true)
}

// errOriginScheme is [CheckOrigin]'s refusal of a value with no http(s)
// scheme or no host.
var errOriginScheme = errors.New("is not an http:// or https:// URL with a host: " +
	"write the origin in full, scheme included (https://crewlet.example.com)")

// originLeftover refuses the parts of s no address built on it can carry: a
// query or a fragment, read from the TEXT (a `?` or a `#` means one thing
// anywhere in a URL, so even a value too broken to parse is judged), and
// userinfo, read from the parse when parse says there is one to read.
func originLeftover(s string, userinfo bool) error {
	part := ""
	switch {
	case strings.Contains(s, "?"):
		part = "a query"
	case strings.Contains(s, "#"):
		part = "a fragment"
	case userinfo:
		if u, err := url.Parse(s); err == nil && u.User != nil {
			part = "userinfo"
		}
	}
	if part == "" {
		return nil
	}
	return fmt.Errorf("carries %s, and every address is built by appending a path "+
		"to this base, so it would land in every one: write the bare origin, "+
		"scheme://host[:port], with a path in front of the engine's own if a "+
		"proxy needs one", part)
}

// checkWrittenOrigin is [CheckOrigin] over the text a DOCUMENT writes, where a
// `${VAR}` stands for a value only its resolution knows. A whole reference has
// nothing written around it to judge. Text around references is judged for
// what the text itself says — every reference standing in as a neutral token,
// so `${HOST}?tenant=a` carries a query whatever HOST holds and
// `https://${USER}:${PASS}@host` carries userinfo — and for its scheme and host
// only where it opens with a literal, since `${SCHEME}://host` has a scheme
// nothing written yet decides.
func checkWrittenOrigin(raw string) error {
	s := strings.TrimSpace(raw)
	if !envref.Has(s) {
		return CheckOrigin(s)
	}
	if _, whole := envref.Whole(s); whole {
		return nil
	}
	written, _ := envref.Expand(s, func(string) (string, bool) { return "x", true })
	if strings.HasPrefix(s, "${") {
		return originLeftover(written, true)
	}
	return CheckOrigin(written)
}

// ErrBaseUnresolved is what a [BaseError] is when the base names a variable
// this process resolved nothing for.
var ErrBaseUnresolved = errors.New("the deployment base names a variable that resolved to nothing")

// BaseError is why a deployment base that IS configured gives this process no
// address: a reference that resolved to nothing, or a resolved value
// [CheckOrigin] refuses. It names the field and the variables, never the
// value, and says what to change.
type BaseError struct {
	// Field is the base's path in the company document.
	Field string
	// Variables names the references the value is written with, empty for
	// a literal.
	Variables []string
	// Unresolved names the references that resolved to nothing; empty when
	// every one resolved.
	Unresolved []string
	// Err is [CheckOrigin]'s refusal of the resolved value, nil when the
	// base is unresolved.
	Err error
}

func (e *BaseError) Error() string {
	switch {
	case len(e.Unresolved) > 0:
		return fmt.Sprintf("%s names %s, and this process resolved nothing for it: set "+
			"it in the environment or the secret store", e.Field, refList(e.Unresolved))
	case len(e.Variables) > 0:
		return fmt.Sprintf("%s, resolved from %s, %v",
			e.Field, strings.Join(e.Variables, ", "), e.Err)
	default:
		return fmt.Sprintf("%s %v", e.Field, e.Err)
	}
}

// Unwrap is the origin judge's refusal.
func (e *BaseError) Unwrap() error { return e.Err }

// Is matches [ErrBaseUnresolved] for a base whose reference resolved to
// nothing.
func (e *BaseError) Is(target error) bool {
	return target == ErrBaseUnresolved && len(e.Unresolved) > 0
}

// refList renders variable names as the references a document writes.
func refList(names []string) string {
	refs := make([]string, len(names))
	for i, name := range names {
		refs[i] = "${" + name + "}"
	}
	return strings.Join(refs, ", ")
}

// WebhookBase is the base every inbound path is built on, without a trailing
// slash: "" with a nil error when this deployment names none, and a
// [*BaseError] when it names one THIS PROCESS CANNOT USE.
//
// Trimmed here rather than at each caller, because five of them would each
// have to remember: a base ending in "/" yields "…//webhooks/jira", which
// some third-party apps normalise, some reject, and some accept while signing
// the unnormalised form.
//
// # It takes a resolver, and that is the whole point of the signature
//
// `public_base_url` is a Tier B field, so a `${VAR}` is a legal way to write
// it and the document stores it VERBATIM like every other pointer. Read raw,
// that value is not an address: it is the characters `${VAR}`, and every
// caller here is building something a third-party app will HOLD — a
// registered webhook, a manifest an operator pastes, an app's baked-in
// redirect. Slack refuses such a manifest and names nothing; a webhook
// registered at `${VAR}/webhooks/gitlab` is accepted, reported healthy, and
// delivers nowhere. So there is no raw accessor to reach for by accident, and
// a caller that cannot resolve passes nil and is told the base is unresolved.
//
// # Three answers, because the callers act on three
//
// Unset is a working configuration — a laptop has no inbound address — and a
// pass runs read-only on it. Unresolved and unusable are a configuration
// somebody has to correct, and a caller that reported either as "unset" sent
// an operator to a field they could see was filled in. A resolved value is
// held to the origin judge the written one was ([CheckOrigin]), because a
// whole `${VAR}` — the documented form — is otherwise never judged at all.
func (i *Integrations) WebhookBase(resolve func(string) (string, bool)) (string, error) {
	return resolveBase("integrations.public_base_url", i.PublicBaseURL, resolve)
}

// DashboardBase is the base every link a person follows is built on, by
// [Integrations.WebhookBase]'s rules and for its reasons: a link built on the
// characters of a reference, or on a value with a query, is a link nobody can
// follow.
func (i *Integrations) DashboardBase(resolve func(string) (string, bool)) (string, error) {
	return resolveBase("integrations.dashboard_base_url", i.DashboardBaseURL, resolve)
}

// resolveBase is one deployment base read: every reference resolved, the
// result judged by [CheckOrigin], and the trailing slash dropped.
func resolveBase(field, raw string, resolve func(string) (string, bool)) (string, error) {
	base := strings.TrimSpace(raw)
	if base == "" {
		return "", nil
	}
	names := envref.Names(base)
	if len(names) > 0 {
		if resolve == nil {
			return "", &BaseError{Field: field, Variables: names, Unresolved: names}
		}
		expanded, missing := envref.Expand(base, resolve)
		if len(missing) > 0 {
			return "", &BaseError{Field: field, Variables: names, Unresolved: missing}
		}
		base = strings.TrimSpace(expanded)
		if base == "" {
			return "", &BaseError{Field: field, Variables: names, Unresolved: names}
		}
	}
	if err := CheckOrigin(base); err != nil {
		return "", &BaseError{Field: field, Variables: names, Err: err}
	}
	return strings.TrimRight(base, "/"), nil
}

// NoWebhookBase is the clause a report opens with when a pass has no address
// to register a hook at: why, from the error [Integrations.WebhookBase]
// answered — "integrations.public_base_url is unset" for none at all, and the
// [BaseError]'s own sentence, naming the variable to correct, otherwise.
func NoWebhookBase(err error) string {
	if err != nil {
		return err.Error()
	}
	return "integrations.public_base_url is unset"
}
