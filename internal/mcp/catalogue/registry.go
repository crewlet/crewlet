package catalogue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/httpx"
)

// SearchBudget bounds one search, the registry's answer included. A person is
// waiting on it in a search field, and a registry that has not answered a
// one-page search in ten seconds is not going to be useful in twenty.
const SearchBudget = 10 * time.Second

// MaxListings is how many listings one search offers, and the page size it
// asks for: a list a person reads card by card, where the twenty-first card
// says the search needs narrowing rather than scrolling. A registry that
// ignores the page size is cut to it.
const MaxListings = 20

// MaxSearchBytes bounds the registry's answer. A listing is a few kilobytes
// (the registry caps its description at 100 characters and names at 200), so
// a page of MaxListings is tens of kilobytes; a megabyte is room for listings
// fifty times that, and past it the answer is not a page this build asked for.
const MaxSearchBytes = 1 << 20

// MaxQuery bounds the search text, in characters: a server's name is what the
// registry matches it against, and the registry caps a name at 200.
const MaxQuery = 200

// The bounds on one listing's fields, past which the listing is left out
// rather than cut: the registry's own limits where it states one (a title and
// a description at 100 characters, a name at 200, a version at 255), and for
// what it leaves unbounded a size no real listing approaches.
const (
	maxTitle       = 100
	maxDescription = 100
	maxName        = 200
	maxVersion     = 255
	maxInputText   = 300
	maxAddress     = 2048
	maxInputs      = 32
	maxArguments   = 16
	maxArgument    = 256
)

// ErrRegistryAddress is a registry address this build will not search: not an
// absolute https address, or one carrying credentials, a query or a fragment.
// Every refusal is an [*AddressError] saying which.
var ErrRegistryAddress = errors.New("catalogue: not a registry address this build searches")

// AddressError is a registry address refused, with the reason a person
// reads.
type AddressError struct{ Reason string }

func (e *AddressError) Error() string {
	return "not a registry address this build searches: " + e.Reason
}

// Unwrap makes it [ErrRegistryAddress] to errors.Is.
func (e *AddressError) Unwrap() error { return ErrRegistryAddress }

// ErrQuery is search text this build will not send. Every refusal is a
// [*QueryError] saying why.
var ErrQuery = errors.New("catalogue: not a search this build sends")

// QueryError is search text refused, with the reason a person reads.
type QueryError struct{ Reason string }

func (e *QueryError) Error() string { return "not a search this build sends: " + e.Reason }

// Unwrap makes it [ErrQuery] to errors.Is.
func (e *QueryError) Unwrap() error { return ErrQuery }

// CheckQuery reports whether q is search text this build sends: some text, at
// most [MaxQuery] characters, with no control character in it.
func CheckQuery(q string) error {
	switch {
	case strings.TrimSpace(q) == "":
		return &QueryError{Reason: "give the text to search for"}
	case utf8.RuneCountInString(q) > MaxQuery:
		return &QueryError{Reason: fmt.Sprintf("it is over %d characters", MaxQuery)}
	case strings.ContainsFunc(q, unicode.IsControl):
		return &QueryError{Reason: "it holds a control character"}
	}
	return nil
}

// CheckRegistryAddress reports whether raw is an address a company may name as
// its registry: an absolute https address with a host, and nothing in it but
// the scheme, the host, an optional port and an optional path the API's own
// path is appended to.
//
// HTTPS ONLY, a private host included. What the registry answers is drafted
// into launch lines an admin may accept, so an answer a network peer could
// rewrite in transit is a command line that peer chose. A registry inside
// the company's network serves https like any other.
func CheckRegistryAddress(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return &AddressError{Reason: "it does not parse as an address"}
	case u.Scheme != "https":
		return &AddressError{Reason: "it must be an https address, so nobody between the engine and " +
			"the registry can rewrite the launch lines it answers"}
	case u.Hostname() == "":
		return &AddressError{Reason: "it names no host"}
	case u.User != nil:
		return &AddressError{Reason: "it carries credentials, which an address is never the place for"}
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return &AddressError{Reason: "it carries a query or a fragment; give the address the " +
			"registry's /v0.1/servers path is appended to"}
	}
	return nil
}

// Registry searches a company's community registry over the MCP Registry's
// v0.1 API. Safe for concurrent use; it holds nothing between searches.
type Registry struct {
	client *http.Client
}

// RegistryOptions wire a registry client.
type RegistryOptions struct {
	// Client overrides the shared transport's client, for a test serving
	// its own registry.
	Client *http.Client
}

// NewRegistry builds a registry client.
//
// ON THE PLAIN CLIENT, a private address allowed: the registry's address is
// one an operator wrote into the company's configuration, not one a remote
// party named. And NO REDIRECT IS FOLLOWED, since a redirect is the remote
// naming an address: a registry that moved answers its 3xx, which the
// search reports with the address it named, for the operator to write.
func NewRegistry(opts RegistryOptions) *Registry {
	client := opts.Client
	if client == nil {
		client = httpx.Client(SearchBudget)
	}
	own := *client
	own.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Registry{client: &own}
}

// Found is one search's answer.
type Found struct {
	// Entries are the listings this build can offer, in the registry's
	// order, each marked Community. Never nil.
	Entries []Entry
	// LeftOut counts the listings the registry answered and this build
	// left out: a shape it does not read, a field past its bounds, or no
	// launch it can spell.
	LeftOut int
}

// Search asks the registry at base for the newest version of each server
// whose name holds q, and offers what it can.
func (r *Registry) Search(ctx context.Context, base, q string) (Found, error) {
	if err := CheckRegistryAddress(base); err != nil {
		return Found{}, err
	}
	q = strings.TrimSpace(q)
	if err := CheckQuery(q); err != nil {
		return Found{}, err
	}
	endpoint, err := url.JoinPath(base, "v0.1", "servers")
	if err != nil {
		return Found{}, &AddressError{Reason: err.Error()}
	}
	endpoint += "?" + url.Values{
		"search":  {q},
		"version": {"latest"},
		"limit":   {strconv.Itoa(MaxListings)},
	}.Encode()

	ctx, cancel := context.WithTimeout(ctx, SearchBudget)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Found{}, &AddressError{Reason: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return Found{}, fmt.Errorf("the registry at %s could not be reached: %w", base, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return Found{}, fmt.Errorf("the registry at %s answered %s and named %q; this build follows "+
			"no redirect, so write the address it moved to as mcp_registry.url", base, resp.Status,
			resp.Header.Get("Location"))
	case resp.StatusCode != http.StatusOK:
		return Found{}, fmt.Errorf("the registry at %s answered %s: %s", base, resp.Status,
			httpx.ReadRefusal(resp))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxSearchBytes+1))
	if err != nil {
		return Found{}, fmt.Errorf("the registry at %s could not be read: %w", base, err)
	}
	if len(body) > MaxSearchBytes {
		return Found{}, fmt.Errorf("the registry at %s answered over %d KiB for one page, so it was "+
			"not read", base, MaxSearchBytes>>10)
	}
	return offerPage(body)
}

// registryPage is one page of the v0.1 server list, in the fields read. Each
// server is decoded on its own, so one listing this build cannot read leaves
// that listing out rather than the page.
type registryPage struct {
	Servers []struct {
		Server json.RawMessage `json:"server"`
		Meta   struct {
			Official *struct {
				Status    string    `json:"status"`
				UpdatedAt time.Time `json:"updatedAt"`
			} `json:"io.modelcontextprotocol.registry/official"`
		} `json:"_meta"`
	} `json:"servers"`
}

// serverJSON is a listing's `server.json`, in the fields read.
type serverJSON struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Version     string `json:"version"`
	WebsiteURL  string `json:"websiteUrl"`
	Repository  *struct {
		URL string `json:"url"`
	} `json:"repository"`
	Packages []packageJSON `json:"packages"`
	Remotes  []remoteJSON  `json:"remotes"`
}

type packageJSON struct {
	RegistryType    string `json:"registryType"`
	RegistryBaseURL string `json:"registryBaseUrl"`
	Identifier      string `json:"identifier"`
	Version         string `json:"version"`
	Transport       struct {
		Type string `json:"type"`
	} `json:"transport"`
	RuntimeArguments     []json.RawMessage `json:"runtimeArguments"`
	PackageArguments     []argumentJSON    `json:"packageArguments"`
	EnvironmentVariables []inputJSON       `json:"environmentVariables"`
}

type argumentJSON struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type remoteJSON struct {
	Type    string      `json:"type"`
	URL     string      `json:"url"`
	Headers []inputJSON `json:"headers"`
}

type inputJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	IsRequired  bool   `json:"isRequired"`
	IsSecret    bool   `json:"isSecret"`
	Value       string `json:"value"`
}

// offerPage reads one page of listings into what this build can offer.
func offerPage(body []byte) (Found, error) {
	var page registryPage
	if err := json.Unmarshal(body, &page); err != nil {
		return Found{}, fmt.Errorf("the registry's answer is not the MCP Registry v0.1 server list: %w", err)
	}
	found := Found{Entries: []Entry{}}
	seen := map[string]bool{}
	for _, listing := range page.Servers {
		if len(found.Entries) == MaxListings {
			// A registry that ignored the page size: the rest were not
			// asked for, rather than left out.
			break
		}
		var updated *time.Time
		if official := listing.Meta.Official; official != nil {
			if official.Status != "" && official.Status != "active" {
				// Deprecated or deleted at the registry: not offered.
				found.LeftOut++
				continue
			}
			if !official.UpdatedAt.IsZero() {
				at := official.UpdatedAt.UTC()
				updated = &at
			}
		}
		var s serverJSON
		if err := json.Unmarshal(listing.Server, &s); err != nil {
			found.LeftOut++
			continue
		}
		entry, ok := offer(s, updated)
		if !ok || seen[entry.ID] {
			found.LeftOut++
			continue
		}
		seen[entry.ID] = true
		found.Entries = append(found.Entries, entry)
	}
	return found, nil
}

// listingName is the registry's own pattern for a server's name: a namespace
// the registry verified for the publisher, a slash, and the server's name.
var listingName = regexp.MustCompile(`^[a-zA-Z0-9.-]+/[a-zA-Z0-9._-]+$`)

// offer drafts one listing as an entry, or false where it is not one this
// build can offer whole.
func offer(s serverJSON, updated *time.Time) (Entry, bool) {
	if len(s.Name) < 3 || len(s.Name) > maxName || !listingName.MatchString(s.Name) {
		return Entry{}, false
	}
	description, ok := line(s.Description, maxDescription)
	if !ok || description == "" {
		return Entry{}, false
	}
	title, ok := line(s.Title, maxTitle)
	if !ok {
		return Entry{}, false
	}
	version, ok := line(s.Version, maxVersion)
	if !ok || version == "" {
		return Entry{}, false
	}
	publisher, server, _ := strings.Cut(s.Name, "/")
	if title == "" {
		title = server
	}
	e := Entry{
		ID: s.Name, Name: suggestedName(server), Title: title, Description: description,
		Args: []string{}, Headers: []Input{}, Env: []Input{}, ToolAnnotations: map[string]Hints{},
		Community: true, Publisher: publisher, Version: version, UpdatedAt: updated,
	}
	if e.Docs = httpsAddress(s.WebsiteURL); e.Docs == "" && s.Repository != nil {
		e.Docs = httpsAddress(s.Repository.URL)
	}

	// A REMOTE FIRST: nothing runs on the engine host for it.
	for _, remote := range s.Remotes {
		if offerRemote(&e, remote) {
			return e, true
		}
	}
	for _, pkg := range s.Packages {
		if offerPackage(&e, pkg) {
			return e, true
		}
	}
	return Entry{}, false
}

// offerRemote drafts a Streamable HTTP remote at a fixed https address, or
// false. A remote whose address is a template is not offered: its variables
// are the listing's to name and a person's to fill, and an address typed by
// hand goes through the same add flow.
func offerRemote(e *Entry, remote remoteJSON) bool {
	if remote.Type != "streamable-http" {
		// The engine's http transport is Streamable HTTP; a listing's
		// legacy SSE remote is one it cannot dial.
		return false
	}
	address := httpsAddress(remote.URL)
	if address == "" || strings.ContainsAny(address, "{}") {
		return false
	}
	headers, ok := inputs(remote.Headers, httpToken.MatchString)
	if !ok {
		return false
	}
	e.Transport, e.URL, e.Headers = TransportHTTP, address, headers
	// THE LISTING'S WORDS SAY HOW A VALUE IS SPELLED, and nothing more: a
	// value a person types is what is sent.
	for i, h := range remote.Headers {
		if format, ok := line(h.Value, maxInputText); ok {
			e.Headers[i].Format = format
		}
		if strings.EqualFold(h.Name, "Authorization") {
			e.Headers[i].Secret = true
		}
	}
	e.Auth = []AuthMethod{}
	if len(headers) > 0 {
		e.Auth = []AuthMethod{AuthHeaders}
	}
	// A VETTED MARK ONLY FOR THE VENDOR'S OWN ADDRESS: a remote at a host
	// the curated table recognises is that vendor's server, whoever listed
	// it. A package's name proves nothing about who built it, so a local
	// listing is never drawn with a vendor's mark.
	if known, ok := Recognise(Launch{Transport: TransportHTTP, URL: address}); ok {
		e.Vendor = known.Vendor
	}
	return true
}

// The runners a listing's package is launched through, by registry type, with
// the public registry each fetches from by default.
var runners = map[string]struct {
	base    string
	command string
	args    func(identifier, version string) []string
	name    *regexp.Regexp
}{
	"npm": {"https://registry.npmjs.org", "npx",
		func(id, v string) []string { return []string{"-y", id + "@" + v} },
		regexp.MustCompile(`^(@[a-z0-9][a-z0-9._~-]*/)?[a-z0-9][a-z0-9._~-]{0,213}$`)},
	"pypi": {"https://pypi.org", "uvx",
		func(id, v string) []string { return []string{id + "==" + v} },
		regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,98}[A-Za-z0-9])?$`)},
}

// exactVersion is a release a runner fetches as written: it starts with a
// letter or digit, so it can never be read as a flag, and holds no range or
// wildcard.
var exactVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+!_-]{0,63}$`)

// offerPackage drafts a stdio package from the public npm or PyPI registry at
// its exact release, or false.
//
// NEVER A LISTING'S RUNNER ARGUMENTS, and never a package from another
// registry: either one would change what the runner fetches (`--registry`,
// `--index-url`, `-p other`) in words a person reading the launch line would
// not recognise as a different package. A listing that needs one is left out.
func offerPackage(e *Entry, pkg packageJSON) bool {
	run, known := runners[pkg.RegistryType]
	switch {
	case !known, pkg.Transport.Type != "stdio", len(pkg.RuntimeArguments) > 0:
		return false
	case pkg.RegistryBaseURL != "" && strings.TrimRight(pkg.RegistryBaseURL, "/") != run.base:
		return false
	case !run.name.MatchString(pkg.Identifier):
		return false
	case !exactVersion.MatchString(pkg.Version) || strings.EqualFold(pkg.Version, "latest"):
		return false
	}
	args := run.args(pkg.Identifier, pkg.Version)
	if len(pkg.PackageArguments) > maxArguments {
		return false
	}
	for _, a := range pkg.PackageArguments {
		word, ok := argument(a)
		if !ok {
			return false
		}
		if word != "" {
			args = append(args, word)
		}
	}
	env, ok := inputs(pkg.EnvironmentVariables, envName.MatchString)
	if !ok {
		return false
	}
	e.Transport, e.Command, e.Args, e.Env = TransportStdio, run.command, args, env
	e.Package, e.Release = pkg.Identifier, pkg.Version
	e.Auth = []AuthMethod{AuthNone}
	return true
}

// flagName is a named argument's flag, dashes included.
var flagName = regexp.MustCompile(`^--?[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// argument is one package argument as one word of the launch, "" for one whose
// value a person gives (left out of the draft for them to add), or false for
// one this build will not pass.
func argument(a argumentJSON) (string, bool) {
	value, ok := line(a.Value, maxArgument)
	if !ok || value != strings.TrimSpace(a.Value) {
		return "", false
	}
	if value == "" {
		// A value the listing leaves to the person running it, flag or
		// positional alike: a flag written without its value would read
		// the next word as one.
		return "", a.Type == "positional" || (a.Type == "named" && flagName.MatchString(a.Name))
	}
	switch a.Type {
	case "positional":
		// A positional that starts with a dash is a flag spelled as a
		// value, which is the named argument's shape and nobody's mistake.
		if strings.HasPrefix(value, "-") {
			return "", false
		}
		return value, true
	case "named":
		if !flagName.MatchString(a.Name) {
			return "", false
		}
		return a.Name + "=" + value, true
	}
	return "", false
}

// httpToken is an http header name: RFC 9110's token.
var httpToken = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,128}$")

// envName is an environment variable's name.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// inputs reads a listing's headers or variables, names held to valid, or false
// when one is not a key a server can read or there are more than a server
// plausibly takes. The listing's value is never carried: a person types it.
func inputs(raw []inputJSON, valid func(string) bool) ([]Input, bool) {
	if len(raw) > maxInputs {
		return nil, false
	}
	out := make([]Input, 0, len(raw))
	names := map[string]bool{}
	for _, in := range raw {
		key := strings.ToLower(in.Name)
		if !valid(in.Name) || names[key] {
			return nil, false
		}
		names[key] = true
		description, ok := line(in.Description, maxInputText)
		if !ok {
			return nil, false
		}
		out = append(out, Input{Name: in.Name, Description: description, Required: in.IsRequired,
			Secret: in.IsSecret})
	}
	return out, true
}

// httpsAddress is raw when it is an absolute https address with a host, no
// credentials and no whitespace, bounded, else "".
func httpsAddress(raw string) string {
	if raw == "" || len(raw) > maxAddress || strings.ContainsFunc(raw, unicode.IsSpace) {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return ""
	}
	return raw
}

// line is s as one line a person reads: whitespace runs collapsed to one
// space, trimmed, and at most max characters; false for text that is not
// UTF-8, is longer, or holds a control character or a direction override,
// which would make what a person reads differ from what the bytes say.
func line(s string, max int) (string, bool) {
	if !utf8.ValidString(s) {
		return "", false
	}
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > max {
		return "", false
	}
	for _, r := range s {
		if unicode.IsControl(r) || directional(r) {
			return "", false
		}
	}
	return s, true
}

// directional is a character that reorders the text around it.
func directional(r rune) bool {
	switch {
	case r == '؜', r == '‎', r == '‏':
		return true
	case r >= '‪' && r <= '‮':
		return true
	case r >= '⁦' && r <= '⁩':
		return true
	}
	return false
}

// suggestedName is a server name drawn from a listing's: lowercase letters,
// digits and single hyphens, at most 32 characters, the shape a server name
// takes. The add flow judges it like any name a person types.
func suggestedName(server string) string {
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(server) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			hyphen = false
		case !hyphen && b.Len() > 0:
			b.WriteByte('-')
			hyphen = true
		}
	}
	name := strings.TrimRight(b.String(), "-")
	if len(name) > 32 {
		name = strings.TrimRight(name[:32], "-")
	}
	if name == "" {
		return "server"
	}
	return name
}
