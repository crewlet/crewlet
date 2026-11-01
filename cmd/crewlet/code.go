package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // a git pack's own trailer, not a security hash
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/textcut"
)

// `crewlet code` — the engine's own git host, from the command line.
//
// # Four kinds of subcommand
//
// status and verify ASK A RUNNING NODE, through the API's /code/ routes, as
// the person the token is bound to: the repositories are rows on the code
// log, which the data nodes keep, and a git node's cache is files that one
// process claims, so nothing here opens either. They are node clients in
// `objects status`'s idiom, and their views of an answer are this command's
// own (codeRepoView, codePushView, codeVerificationView) for its reason: a
// struct shared with the writer ties an older binary to a newer node's answer.
// The tests serve the writer — a real node — so the two cannot drift
// unnoticed.
//
// import and purge-unreachable ACT, through the act route
// (`POST /operator/act/{tool}`) as the person the token is bound to: the
// tools a person's assistant calls, code_import_repo and
// code_purge_unreachable, so a request from a shell is the same request with
// the same record. An import creates the repository, and its home git node
// fetches the source (codeimportnode.go).
//
// export and `import -local` are CLIENT-SIDE: the operator's own git, run
// with the operator's own environment — their proxy, their configuration,
// their credentials for another host — talks to the node's git routes exactly
// as a person's git does. export clones a repository and writes the bundle on
// this machine; `import -local` clones another host's repository here and
// pushes it into one the node serves, in bounded steps (codeimport.go). The
// engine dials nothing for either ([operatorGit]).
//
// credential is git's credential helper for the code host
// ([codeCredential]), which export configures for every git it runs against a
// node, and which a person configures for their own.

// codeUsage is every `crewlet code` synopsis.
const codeUsage = `usage: crewlet code <command>
  status [<config.yaml>] [-url URL] [-repo NAME] [-pushes N] [-json]
      the repositories and their history, as the node reads them
  verify [<config.yaml>] [-url URL] [-repo NAME]... [-repair] [-json]
      check THIS node's cache against the repositories' rows; every git
      node keeps its own cache, so verify each
  export <name> <file.bundle> [<config.yaml>] [-url URL] [-git PATH]
      write a repository's every ref and object to one git bundle
  import <source-url> <name> [<config.yaml>] [-url URL] -owner unit:KEY|seat:HANDLE
         [-credential VAR] [-mirror] [-op-id ID]
      create a project the node fills from another host's repository — or
      keeps in step with it, -mirror — fetching it with the token the node
      holds as VAR; a person's alone, through their own token
  import <source-url> <name> -local [<config.yaml>] [-url URL] [-git PATH] [-dry-run]
      push another host's repository from this machine into one that exists,
      in steps under code.limits.max_push_bytes; the same command again
      resumes it
  purge-unreachable <name> [<config.yaml>] [-url URL] [-op-id ID]
      ask the repository's maintainer to drop every object no branch or tag
      reaches, with no grace; a person's alone, through their own token
  credential [-token-env NAME] get|store|erase
      git's credential helper: credential.<url>.helper = !crewlet code credential
`

// runCode dispatches `crewlet code`. environ is the invocation's environment
// ([runIn]): the node commands take their credential from it, and so does the
// credential helper, which answers git with the same variable.
func runCode(environ config.Source, args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "status":
		return codeStatus(environ, rest, stdout, stderr)
	case "verify":
		return codeVerify(environ, rest, stdout, stderr)
	case "export":
		return codeExport(environ, rest, stdout, stderr)
	case "import":
		return codeImport(environ, rest, stdout, stderr)
	case "purge-unreachable":
		return codePurge(environ, rest, stdout, stderr)
	case "credential":
		return codeCredential(rest, os.Stdin, stdout, stderr, func(name string) string {
			value, _ := environ.Lookup(name)
			return value
		})
	case "hook":
		// THE HOOK NEVER REACHES run: main() dispatches `code hook` before
		// anything else, because its stdout is git's protocol. Asked here —
		// from a test — it is a hand-run, and says so in the host's own
		// sentence, with no relay to dial.
		host.RunHook(rest, strings.NewReader(""), stdout, stderr, func(string) string { return "" })
		return errSilent
	case "", "help":
		fmt.Fprint(stderr, codeUsage)
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown code command %q", sub)
	}
}

// ---- credential -----------------------------------------------------------

// maxCredentialInput bounds what the helper reads from git: git's attributes
// are a protocol, a host, a path, a user name and a few capability lines — a
// few hundred bytes. A megabyte of input is not a request, and 64 KiB is two
// orders of magnitude above any git sends.
const maxCredentialInput = 64 << 10

// codeCredentialUser is the user name the helper answers. FIXED: the API
// reads only the password of a Basic credential (ADR-0055), so a name would
// say something the node never reads.
const codeCredentialUser = "crewlet"

// codeCredential is git's credential helper for the code host: configured as
// credential.<base>.helper = !crewlet code credential, it answers `get` with
// the token in the environment as the Basic password, and keeps nothing — the
// token's home is the variable, never a file this helper writes, so `store`
// and `erase` do nothing.
//
// ONLY OVER HTTPS, OR PLAIN HTTP TO THIS MACHINE. A Basic credential over plain
// http to another host is the token in the clear on the network, so the helper
// says so and answers nothing, and git falls through to its next helper or
// prompt. Loopback is the exception because nothing crosses a network there —
// a node beside its operator, a test.
func codeCredential(args []string, stdin io.Reader, stdout, stderr io.Writer,
	getenv func(string) string,
) error {
	fs := flag.NewFlagSet("code credential", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tokenEnv := fs.String("token-env", apiTokenEnv,
		"the environment variable holding the API token git is given as the password")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// THE ACTION IS THE LAST ARGUMENT: git appends it to the configured
	// command, after any flag the configuration spelled.
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: crewlet code credential [-token-env NAME] get|store|erase")
		return fmt.Errorf("git's credential helper takes one action, get, store or erase; got %d", fs.NArg())
	}
	action := fs.Arg(0)
	switch action {
	case "get", "store", "erase":
	default:
		return fmt.Errorf("git's credential helper takes get, store or erase, not %q", action)
	}
	// EVERY ACTION READS ITS INPUT TO THE END: that is the protocol whatever
	// the action does with it, and a helper that stopped early would leave
	// git writing into a closed pipe.
	attrs, err := readCredentialAttributes(stdin)
	if err != nil {
		return err
	}
	if action != "get" {
		return nil
	}
	token := strings.TrimSpace(getenv(*tokenEnv))
	if token == "" {
		// Nothing to answer with: git asks its next helper, or the person.
		return nil
	}
	if strings.ContainsAny(token, "\n\x00") {
		// A line break would end the password and start an attribute of
		// the token's choosing.
		return fmt.Errorf("the token in %s holds a line break, which git's credential protocol "+
			"cannot carry: an API token is one line", *tokenEnv)
	}
	switch attrs["protocol"] {
	case "https":
	case "http":
		if !loopbackHost(attrs["host"]) {
			fmt.Fprintf(stderr, "crewlet code credential: not sending a token over plain http to %s; "+
				"serve the API over HTTPS\n", attrs["host"])
			return nil
		}
	default:
		// Not a node's API: no answer, and git asks elsewhere.
		return nil
	}
	_, err = fmt.Fprintf(stdout, "username=%s\npassword=%s\n", codeCredentialUser, token)
	return err
}

// readCredentialAttributes reads git's `key=value` lines up to a blank line,
// then the rest of the input to its end, bounded by [maxCredentialInput]. A
// line without `=` is skipped: git's protocol grows (capability[], wwwauth[])
// and a helper reads what it knows.
func readCredentialAttributes(r io.Reader) (map[string]string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxCredentialInput+1))
	if err != nil {
		return nil, fmt.Errorf("read git's credential request: %w", err)
	}
	if len(raw) > maxCredentialInput {
		return nil, fmt.Errorf("git sent more than %d bytes of credential attributes, which is not a "+
			"credential request", maxCredentialInput)
	}
	attrs := map[string]string{}
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			attrs[key] = value
		}
	}
	return attrs, nil
}

// loopbackHost reports whether git's `host` attribute — a name or an address,
// with the port where the URL named one — is this machine.
func loopbackHost(hostport string) bool {
	hostname := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		hostname = h
	}
	hostname = strings.TrimSuffix(strings.TrimPrefix(hostname, "["), "]")
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

// ---- the views of a node's answers -----------------------------------------

// codePosition is a statelog position as the code reads answer it.
type codePosition struct {
	Stream     string `json:"stream"`
	Generation uint32 `json:"generation"`
	Seq        uint64 `json:"seq"`
}

// String is the position in the tree's one token form, stream@generation:seq,
// so a position printed here is one `min_position` reads back.
func (p codePosition) String() string {
	return fmt.Sprintf("%s@%d:%d", p.Stream, p.Generation, p.Seq)
}

// codeRepoView is a repository as `status` and `verify` read it: the listing's
// fields and, from GET /code/repos/{name}, the detail's.
type codeRepoView struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// ManagedBy is who changes it — `config` for one code.repos declares,
	// `tools` for one a tool made — and Kind what it is for: a `project`, or
	// a seat's own `scratch` repository, which the code-retention duty deletes
	// once it has sat idle since LastActivityAt.
	ManagedBy      string    `json:"managed_by"`
	Kind           string    `json:"kind"`
	LastActivityAt time.Time `json:"last_activity_at"`
	// Redirect is the repository's current name where it was asked for by an
	// OLD one a rename left redirecting to it, until RedirectUntil.
	Redirect      string    `json:"redirect"`
	RedirectUntil time.Time `json:"redirect_until"`
	Owner         struct {
		Kind string `json:"kind"`
		Ref  string `json:"ref"`
	} `json:"owner"`
	Visibility    string               `json:"visibility"`
	Search        string               `json:"search"`
	DefaultBranch string               `json:"default_branch"`
	Description   string               `json:"description"`
	Protections   []codeProtectionView `json:"protections"`
	DeletedAt     time.Time            `json:"deleted_at"`
	Refs          int64                `json:"refs"`
	LivePacks     int64                `json:"live_packs"`
	LiveBytes     int64                `json:"live_bytes"`
	LiveObjects   int64                `json:"live_objects"`
	Head          string               `json:"head"`
	Pushes        int64                `json:"pushes"`
	LastPushAt    time.Time            `json:"last_push_at"`
	Branches      int64                `json:"branches"`
	Tags          int64                `json:"tags"`
	Tombstones    int64                `json:"tombstones"`
	CreatedAt     time.Time            `json:"created_at"`
	Limits        *struct {
		MaxPushBytes int64 `json:"max_push_bytes"`
		MaxBlobBytes int64 `json:"max_blob_bytes"`
		RepoMaxBytes int64 `json:"repo_max_bytes"`
	} `json:"limits"`
	// Expunge is where the repository's expunges stand: a person's purge
	// requested and not yet served, an expunge open, and the last commit.
	Expunge struct {
		Open           string    `json:"open"`
		Request        string    `json:"request"`
		ExpungedAt     time.Time `json:"expunged_at"`
		DroppedObjects int64     `json:"dropped_objects"`
		DroppedBytes   int64     `json:"dropped_bytes"`
	} `json:"expunge"`
}

// codeProtectionView is one protection rule.
type codeProtectionView struct {
	Pattern     string `json:"pattern"`
	AllowForce  bool   `json:"allow_force"`
	AllowDelete bool   `json:"allow_delete"`
	DirectPush  string `json:"direct_push"`
}

// codePushView is one push of a repository's history.
type codePushView struct {
	At        time.Time `json:"at"`
	Actor     string    `json:"actor"`
	ActorKind string    `json:"actor_kind"`
	ActorSeat string    `json:"actor_seat"`
	// Run is a seat's push's: the turn that made it and the work item it was
	// charged to, and a coding box's run and its coding agent.
	Run struct {
		TurnID      string `json:"turn_id"`
		WorkBackend string `json:"work_backend"`
		WorkKey     string `json:"work_key"`
		RunID       string `json:"run_id"`
		CodingAgent string `json:"coding_agent"`
	} `json:"run"`
	Node     string `json:"node"`
	Commands []struct {
		Ref    string `json:"ref"`
		Old    string `json:"old"`
		New    string `json:"new"`
		Forced bool   `json:"forced"`
		Result string `json:"result"`
		Reason string `json:"reason"`
	} `json:"commands"`
}

// codeReadView is one answer of the code reads: a page of repositories, one
// repository, or a page of its pushes.
type codeReadView struct {
	Repos    []codeRepoView `json:"repos"`
	Repo     *codeRepoView  `json:"repo"`
	Pushes   []codePushView `json:"pushes"`
	Next     string         `json:"next"`
	Position codePosition   `json:"position"`
}

// What one answer of the code reads may carry, and the pages the CLI asks
// for.
//
// A code read's page ends at code.MaxPageBytes of encoded rows — closed on the
// row that crosses the budget, and keeping that row — so an answer carries up
// to MaxPageBytes, one row more and its envelope, whatever its row count says.
// The CLI reads them by that bound (codeAnswerBytes,
// [nodeClient.forCodeReads]), never by maxNodeResponseBytes, which a
// legitimate page carries past: every byte of a ref, a protection pattern and
// a default branch may be one JSON writes as six (`<`, `>` and `&` are legal
// in all three), and a history page of such pushes is 5.05 MB, a listing page
// of such repositories 4.20 MB.
//
// So a page asks for the MOST rows the reader serves of its kind
// (code.MaxReposPerPage, code.MaxPushesPerPage) — the fewest requests, the
// reader's byte budget bounding each — and a history asks for no more than
// the pushes still wanted.
const (
	// codeRowBytes is the most one row of a code read encodes to: two
	// mebibytes, above the largest TestTheCodePagesFitOneAnswer builds — a
	// push of code.MaxCommandsPerPush commands at the longest ref beside
	// code.MaxIntroduced ids and a seat's run at its bound, each byte of a ref
	// and of a run field but the coding run's UUID one JSON writes as six, at
	// 1 692 620 bytes (the code domain refuses a push whose record entry is
	// that wide, so no row reaches it), and a repository at its longest
	// description and default branch with every rule
	// code.MaxProtectionsPerRepo allows, all of them escaped, at 107 746.
	codeRowBytes = 2 << 20
	// codeEnvelopeBytes is the most an answer carries beside its rows: a comma
	// between rows — fewer than code.MaxReposPerPage, the most a page asks
	// for — the cursor, the position and the level, about 2 KB.
	codeEnvelopeBytes = 64 << 10
	// codeAnswerBytes is the most one answer of the code reads carries.
	codeAnswerBytes = code.MaxPageBytes + codeRowBytes + codeEnvelopeBytes
)

// forCodeReads returns a client that reads an answer of up to codeAnswerBytes.
//
// [maxNodeResponseBytes]'s exception, in [nodeClient.patiently]'s shape and
// for its reason one dimension over: every other route a node client reads
// answers a bounded document, so its cap is a diagnosis — something in front
// of the node — while a code read answers a PAGE of the code domain's rows,
// which carries what the reader's own page bound lets it, and a cap below
// that bound refuses legitimate answers. These commands read by that bound;
// the cap stays for everything else.
func (c *nodeClient) forCodeReads() *nodeClient {
	reading := *c
	reading.maxAnswer = codeAnswerBytes
	return &reading
}

// codeTime is a moment as these commands print it.
func codeTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// shortOID is an object id as git abbreviates it, or "-" for none.
func shortOID(oid string) string {
	if oid == "" {
		return "-"
	}
	return oid[:min(7, len(oid))]
}

// ---- status ---------------------------------------------------------------

// codeStatus is `crewlet code status`: the repositories, or with -repo one of
// them in detail with its recent pushes.
func codeStatus(environ config.Source, args []string, stdout, stderr io.Writer) error {
	var (
		repo   *string
		pushes *int
		asJSON *bool
	)
	client, err := nodeClientFor(environ, args, "code status", stderr, func(fs *flag.FlagSet) {
		repo = fs.String("repo", "", "one repository in detail, with its recent pushes")
		pushes = fs.Int("pushes", 10, "with -repo, how many of its newest pushes to print")
		asJSON = fs.Bool("json", false, "print the node's answers as they came, one JSON document a page")
	})
	if err != nil {
		return err
	}
	if *pushes < 0 {
		return fmt.Errorf("-pushes is how many pushes to print, zero or more; got %d", *pushes)
	}
	client = client.forCodeReads()
	ctx := context.Background()
	if *repo == "" {
		return codeListRepos(ctx, client, stdout, stderr, *asJSON)
	}
	return codeShowRepo(ctx, client, *repo, *pushes, stdout, *asJSON)
}

// codeListRepos prints every active and archived repository, a page at a time.
//
// At the reads' default level, stale — a listing an operator reads — and the
// position it was read at on stderr, so two runs can be compared.
//
// NOTHING IS HELD BUT WHAT IS PRINTED: with -json each page goes out as it
// came, and in the table each repository is its row of text, which the
// tabwriter keeps until it can size the columns — never the node's whole view
// of every repository, rules and description included, which at
// config.CodeMaxRepos runs to a couple of hundred megabytes for a table of a
// couple of megabytes.
func codeListRepos(ctx context.Context, client *nodeClient, stdout, stderr io.Writer, asJSON bool) error {
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tOWNER\tBRANCH\tHEAD\tREFS\tPACKS\tSIZE\tLAST PUSH")
	listed := 0
	from, to, err := codePages(ctx, client, "/code/repos", url.Values{}, 0, code.MaxReposPerPage,
		func(raw json.RawMessage, page codeReadView) bool {
			if asJSON {
				fmt.Fprintf(stdout, "%s\n", raw)
				return true
			}
			for _, r := range page.Repos {
				listed++
				fmt.Fprintf(w, "%s\t%s\t%s:%s\t%s\t%s\t%d\t%d\t%s\t%s\n", r.Name, r.Status, r.Owner.Kind,
					r.Owner.Ref, r.DefaultBranch, shortOID(r.Head), r.Refs, r.LivePacks, humanBytes(r.LiveBytes),
					codeTime(r.LastPushAt))
			}
			return true
		})
	if err != nil {
		return codeReadRefused(client, err)
	}
	if asJSON {
		return nil
	}
	if listed == 0 {
		// The header the tabwriter holds is never flushed.
		_, err := fmt.Fprintln(stdout, "no repositories: code.repos declares none")
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if from == to {
		fmt.Fprintf(stderr, "read at %s\n", from)
	} else {
		fmt.Fprintf(stderr, "read at %s through %s\n", from, to)
	}
	return nil
}

// codeShowRepo prints one repository in detail and up to n of its newest
// pushes.
//
// THE REPOSITORY AT THE LOG'S HEAD — a person asking about one repository
// wants it as it is now — and ITS HISTORY FLOORED THERE: every page of pushes
// is read at or past the position the detail answered at, so the history is
// never older than the repository printed above it and no later page skips
// pushes an earlier copy had not reached, with no barrier of its own
// (codePages). And as with the listing, only what is printed is kept: each
// push as its row of text, and with -json nothing.
func codeShowRepo(ctx context.Context, client *nodeClient, name string, n int, stdout io.Writer,
	asJSON bool,
) error {
	path := "/code/repos/" + url.PathEscape(name)
	var answer json.RawMessage
	if err := client.get(ctx, path+"?read_level=linearizable", &answer); err != nil {
		return codeReadRefused(client, err)
	}
	var detail codeReadView
	if err := json.Unmarshal(answer, &detail); err != nil {
		return fmt.Errorf("the node's answer for %s is not the shape this build reads: %w", name, err)
	}
	if detail.Repo == nil {
		return fmt.Errorf("the node's answer for %s carries no repository", name)
	}
	if asJSON {
		fmt.Fprintf(stdout, "%s\n", answer)
	}
	var history []codePushLine
	if n > 0 {
		_, _, err := codePages(ctx, client, path+"/pushes", floorAt(detail.Position), n, code.MaxPushesPerPage,
			func(raw json.RawMessage, page codeReadView) bool {
				if asJSON {
					fmt.Fprintf(stdout, "%s\n", raw)
					return true
				}
				for _, p := range page.Pushes {
					if len(history) < n {
						history = append(history, pushLine(p))
					}
				}
				return true
			})
		if err != nil {
			return codeReadRefused(client, err)
		}
	}
	if asJSON {
		return nil
	}
	return renderCodeRepo(stdout, *detail.Repo, history)
}

// floorAt is the query that reads at or past at with no barrier of its own:
// stale, floored at a position the caller already read at.
func floorAt(at codePosition) url.Values {
	q := url.Values{"read_level": {"stale"}}
	if at.Stream != "" {
		q.Set("min_position", at.String())
	}
	return q
}

// codePages reads a code listing page after page — until the node has no next
// page, visit says stop, or want rows (zero for every one) have been read —
// handing visit each page as it came and as this build reads it, and answers
// the positions the first and the last page were read at.
//
// ONE BARRIER A LISTING AT MOST. The first page is read at query's level, and
// every later one at stale floored at the first page's position (floorAt): at
// or past it, so the pages are one monotone history whichever data node
// answers each, and with no barrier of its own. A page read at linearizable
// appends a barrier to the code log, and the read index single-flights only
// the reads in flight together, so a listing paged one request after another
// at the head paid one a page — on the log whose census counts a day's
// barriers as the people's git reads.
//
// Each page asks for pageMax rows — the reader's most for the kind — or, with
// want, no more than are still wanted.
func codePages(ctx context.Context, client *nodeClient, path string, query url.Values, want, pageMax int,
	visit func(raw json.RawMessage, page codeReadView) bool,
) (first, last codePosition, err error) {
	// A COPY, so a caller's values are never changed under it: Set replaces
	// a key's values rather than appending to a slice the copy shares.
	query = maps.Clone(query)
	if query == nil {
		query = url.Values{}
	}
	read := 0
	for page := 0; ; page++ {
		limit := pageMax
		if want > 0 {
			limit = min(pageMax, want-read)
		}
		query.Set("limit", strconv.Itoa(limit))
		var raw json.RawMessage
		if err := client.get(ctx, path+"?"+query.Encode(), &raw); err != nil {
			return first, last, err
		}
		var answer codeReadView
		if err := json.Unmarshal(raw, &answer); err != nil {
			return first, last, fmt.Errorf("the node's answer to %s is not the shape this build reads: %w", path,
				err)
		}
		if page == 0 {
			first = answer.Position
			for key, values := range floorAt(first) {
				query[key] = values
			}
		}
		last = answer.Position
		read += len(answer.Repos) + len(answer.Pushes)
		if !visit(raw, answer) || answer.Next == "" || want > 0 && read >= want {
			return first, last, nil
		}
		query.Set("cursor", answer.Next)
	}
}

// codeReadRefused is a code read's refusal in an operator's words: a node
// serving no /code/ routes is told apart from a node's own answer, which is
// passed on as it came — its detail says what it is (a repository not
// declared names code.repos, a company hosting no code names code.native).
func codeReadRefused(client *nodeClient, err error) error {
	var refusal *nodeRefusal
	if errors.As(err, &refusal) && refusal.Code == "no_route" {
		return missingSurface(client.base, "code host", refusal.Code, "-url")
	}
	return err
}

// codePushLine is one push as status prints it: its row's cells, kept in
// place of the node's whole view of it.
type codePushLine struct {
	at, by, node string
	refs         int
	results      string
}

// pushLine is p as status prints it: who made it — a person's credential
// and, where bound, the seat they hold; a seat by its handle and the turn and
// work item behind its push, and its coding box's run and coding agent where
// the box pushed — the node that took it, and what became of every ref,
// `ok|noop|ng <ref> <old>..<new> [(reason)]` joined with `; `.
func pushLine(p codePushView) codePushLine {
	by := p.Actor
	switch {
	case p.ActorKind == string(code.PrincipalAgent):
		// A SEAT IS ITS OWN ACTOR, so its seat beside it would say nothing:
		// what its push says beside the handle is what made it.
		by = p.ActorSeat
		var made []string
		if p.Run.RunID != "" {
			made = append(made, strings.TrimSpace(p.Run.CodingAgent+" run "+p.Run.RunID))
		}
		if p.Run.TurnID != "" {
			made = append(made, "turn "+p.Run.TurnID)
		}
		if p.Run.WorkKey != "" {
			made = append(made, strings.TrimSpace(p.Run.WorkBackend+" "+p.Run.WorkKey))
		}
		if len(made) > 0 {
			by += " (" + strings.Join(made, ", ") + ")"
		}
	case p.ActorKind == string(code.PrincipalSystem):
		// THE ENGINE ON ITS OWN ACCOUNT names no actor: its one push is the
		// ref lifecycle's deletion of seats' idle branches.
		by = "engine (ref expiry)"
	case p.ActorSeat != "":
		by += " (" + p.ActorSeat + ")"
	}
	node := p.Node
	if node == "" {
		node = "-"
	}
	results := make([]string, len(p.Commands))
	for i, c := range p.Commands {
		// git's own spelling of a ref's move: two dots for a fast forward,
		// three for a forced one.
		move := shortOID(c.Old) + ".." + shortOID(c.New)
		if c.Forced {
			move = shortOID(c.Old) + "..." + shortOID(c.New)
		}
		results[i] = c.Result + " " + c.Ref + " " + move
		if c.Reason != "" {
			results[i] += " (" + c.Reason + ")"
		}
	}
	return codePushLine{at: codeTime(p.At), by: by, node: node, refs: len(p.Commands),
		results: strings.Join(results, "; ")}
}

// renderCodeRepo prints one repository and its pushes.
func renderCodeRepo(w io.Writer, r codeRepoView, pushes []codePushLine) error {
	var b strings.Builder
	switch {
	case r.Status == "deleted" && code.ManagedBy(r.ManagedBy) == code.ManagedTools:
		// A TOOL MADE IT, and a configuration never adopts one: declaring
		// its name would be refused, so the way back is the tools'.
		fmt.Fprintf(&b, "%s — deleted on %s; a restore through the code tools brings it back until "+
			"code.deleted_retention_days have passed\n", r.Name, codeTime(r.DeletedAt))
	case r.Status == "deleted":
		fmt.Fprintf(&b, "%s — deleted on %s; declaring it again in code.repos restores it until "+
			"code.deleted_retention_days have passed\n", r.Name, codeTime(r.DeletedAt))
	case r.Status == "purging":
		fmt.Fprintf(&b, "%s — being purged since its deletion on %s: its refs and packs are being "+
			"removed, and its name is free once they are\n", r.Name, codeTime(r.DeletedAt))
	default:
		fmt.Fprintf(&b, "%s — %s, owned by %s:%s, default branch %s\n", r.Name, r.Status, r.Owner.Kind,
			r.Owner.Ref, r.DefaultBranch)
	}
	if r.Redirect != "" {
		fmt.Fprintf(&b, "  renamed:      the name asked for is an old one, redirecting here until %s; "+
			"set a remote to /git/%s.git\n", codeTime(r.RedirectUntil), r.Redirect)
	}
	fmt.Fprintf(&b, "  managed by:   %s\n", managedLine(r))
	if r.Description != "" {
		fmt.Fprintf(&b, "  description:  %s\n", r.Description)
	}
	fmt.Fprintf(&b, "  visibility:   %s\n", visibilityLine(r.Visibility))
	fmt.Fprintf(&b, "  search:       %s\n", searchLine(r.Search))
	fmt.Fprintf(&b, "  protections:  %s\n", protectionsLine(r.Protections))
	refs := fmt.Sprintf("%d branches, %d tags", r.Branches, r.Tags)
	if other := r.Refs - r.Branches - r.Tags; other > 0 {
		refs += fmt.Sprintf(", %d other", other)
	}
	if r.Head == "" {
		refs += "; no " + r.DefaultBranch + " yet"
	} else {
		refs += "; " + r.DefaultBranch + " at " + r.Head
	}
	fmt.Fprintf(&b, "  refs:         %s\n", refs)
	fmt.Fprintf(&b, "  packs:        %d live, %s, %d objects\n", r.LivePacks, humanBytes(r.LiveBytes),
		r.LiveObjects)
	if r.Limits != nil {
		fmt.Fprintf(&b, "  limits:       push %s, file %s, repository %s\n", humanBytes(r.Limits.MaxPushBytes),
			humanBytes(r.Limits.MaxBlobBytes), humanBytes(r.Limits.RepoMaxBytes))
	}
	fmt.Fprintf(&b, "  pushes:       %d ever, the last %s\n", r.Pushes, codeTime(r.LastPushAt))
	if line := expungeLine(r); line != "" {
		fmt.Fprintf(&b, "  expunge:      %s\n", line)
	}
	fmt.Fprintf(&b, "  created:      %s\n", codeTime(r.CreatedAt))
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	if len(pushes) == 0 {
		return nil
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PUSHED AT\tBY\tNODE\tREFS\tRESULT")
	for _, p := range pushes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", p.at, p.by, p.node, p.refs, p.results)
	}
	return tw.Flush()
}

// expungeLine is where a repository's expunges stand, as `status` prints it:
// a purge requested and not yet served, an expunge open, and the last one
// committed — "" for a repository never expunged with none asked of it.
func expungeLine(r codeRepoView) string {
	var parts []string
	if e := r.Expunge; e.Request != "" {
		parts = append(parts, "purge "+e.Request+" requested, not yet served")
	}
	if e := r.Expunge; e.Open != "" {
		parts = append(parts, "expunge "+e.Open+" open")
	}
	if e := r.Expunge; !e.ExpungedAt.IsZero() {
		parts = append(parts, fmt.Sprintf("last committed %s, dropping %d objects (%s)", codeTime(e.ExpungedAt),
			e.DroppedObjects, humanBytes(e.DroppedBytes)))
	}
	return strings.Join(parts, "; ")
}

// managedLine is who changes a repository and what it is for, as `status`
// prints it: code.repos for a declared one, the code tools for one a tool
// made — a project, or a seat's scratch repository with the clock its idle
// expiry reads. A value this build does not know is printed as the node sent
// it.
func managedLine(r codeRepoView) string {
	if code.ManagedBy(r.ManagedBy) != code.ManagedTools {
		if code.ManagedBy(r.ManagedBy) == code.ManagedConfig {
			return "code.repos"
		}
		return r.ManagedBy
	}
	switch code.RepoKind(r.Kind) {
	case code.RepoProject:
		return "the code tools — a project"
	case code.RepoScratch:
		return "the code tools — " + r.Owner.Ref + "'s scratch repository, last active " +
			codeTime(r.LastActivityAt) + ", deleted once idle for code.scratch.idle_days unless a live turn holds it"
	}
	return "the code tools — " + r.Kind
}

// visibilityLine is a repository's visibility as `status` prints it: which
// seats read it, since a person reads every repository whatever it says. A
// value this build does not know is printed as the node sent it.
func visibilityLine(v string) string {
	switch code.Visibility(v) {
	case code.VisibilityCompany:
		return v + " — every seat reads it"
	case code.VisibilityUnit:
		return v + " — only the seats the chart relates to its owner read it"
	}
	return v
}

// searchLine is a repository's search mode as `status` prints it: what reads
// it. A value this build does not know is printed as the node sent it.
func searchLine(m string) string {
	switch code.SearchMode(m) {
	case code.SearchIndexed:
		return m + " — read from an index of its default branch, by a search across the company's " +
			"repositories as well as one naming it"
	case code.SearchGrep:
		return m + " — read with git grep, only by a search naming it"
	}
	return m
}

// protectionsLine is a repository's rules, each with what it forbids.
func protectionsLine(rules []codeProtectionView) string {
	if len(rules) == 0 {
		return "none"
	}
	parts := make([]string, len(rules))
	for i, p := range rules {
		var forbids []string
		switch {
		case p.DirectPush == "none":
			forbids = []string{"no direct push"}
		default:
			if !p.AllowForce {
				forbids = append(forbids, "no force")
			}
			if !p.AllowDelete {
				forbids = append(forbids, "no delete")
			}
		}
		if len(forbids) == 0 {
			forbids = []string{"force and delete allowed"}
		}
		parts[i] = p.Pattern + " (" + strings.Join(forbids, ", ") + ")"
	}
	return strings.Join(parts, ", ")
}

// ---- verify ---------------------------------------------------------------

// codeVerificationView is one verification as the node answers it.
type codeVerificationView struct {
	ID       string    `json:"id"`
	Repo     string    `json:"repo"`
	State    string    `json:"state"`
	Deadline time.Time `json:"deadline"`
	Report   *struct {
		Packs           int      `json:"packs"`
		Refs            int      `json:"refs"`
		RepairedBySync  []string `json:"repaired_by_sync"`
		Problems        []string `json:"problems"`
		RepairedInPlace bool     `json:"repaired_in_place"`
		Rebuilt         bool     `json:"rebuilt"`
		Found           []string `json:"found"`
	} `json:"report"`
	Error  string `json:"error"`
	Detail string `json:"detail"`
}

// The verify's polling. The request that begins a check waits host.VerifyHold
// for it, so a check still running when the node answers has run at least
// that long: it is read first codeVerifyPollFirst later — that same quarter
// of a second — and then at doubling waits up to codeVerifyPollMax. What a
// poll costs is a lookup on the node, so the bounds are the check's: a check
// a few times the hold is seen within one or two polls of its end, a large
// one's every five seconds, twelve lookups a minute, its end seen at most
// five seconds late.
const (
	codeVerifyPollFirst = host.VerifyHold
	codeVerifyPollMax   = 5 * time.Second
)

// codeVerifyLostContact is how long a verification's polls may go unanswered
// before the command stops waiting for it: half a minute, a blip of the
// network or a balancer. Longer recovers nothing — a node that restarts
// forgets every verification it kept (host.VerifyKept never crosses one) —
// and asking again hands back a check still running.
const codeVerifyLostContact = 30 * time.Second

// codeVerifyRefusedFor is how long a verify the node refuses for now —
// its verify slot held by another check, the memory the check needs held by
// the background work, the node stopping — is asked again, after each
// refusal's own Retry-After, before the command reports it and moves on:
// five minutes, twice the longest maintenance run measured (the
// multi-pack-index and its bitmap over 5 GB, 152 s: what holds the memory a
// check is refused for) and ten of a stopping node's Retry-After. A slot
// another check holds is held by that check's own bound, which can be
// hours, and a sweep waiting out every such check would finish none of the
// rest.
const codeVerifyRefusedFor = 5 * time.Minute

// codeVerifyWaits is how a verification is waited for: the polls' first and
// longest waits, how long they may go unanswered, how long past the deadline
// the node states the command still waits for it, and how long a refusal for
// now is asked again. A VALUE rather than constants read in place, so a case
// can wait at its own scale while the command waits at
// [productionVerifyWaits].
type codeVerifyWaits struct {
	pollFirst, pollMax, lostContact, grace, refusedFor time.Duration
}

// productionVerifyWaits is what `crewlet code verify` waits by. Its grace past
// the node's deadline is a poll and a request: the node ends a check at its
// deadline and records it failed, and that record is read at the next poll.
func productionVerifyWaits() codeVerifyWaits {
	return codeVerifyWaits{pollFirst: codeVerifyPollFirst, pollMax: codeVerifyPollMax,
		lostContact: codeVerifyLostContact, grace: codeVerifyPollMax + nodeRequestTimeout,
		refusedFor: codeVerifyRefusedFor}
}

// codeVerify is `crewlet code verify`: a check of THIS node's cache of each
// repository against its rows, and with -repair a repair of what it finds —
// in place where only the copy's own directory is out of place, by a rebuild
// otherwise. Every repository is verified before the command answers, and it
// exits non-zero when any is not clean — the hook a cron runs on each git
// node. A repository the node holds no copy of is passed over: nothing there
// can be damaged.
func codeVerify(environ config.Source, args []string, stdout, stderr io.Writer) error {
	var (
		named  []string
		repair *bool
		asJSON *bool
	)
	client, err := nodeClientFor(environ, args, "code verify", stderr, func(fs *flag.FlagSet) {
		fs.Func("repo", "a repository to verify; repeat it for more, or name none to verify every "+
			"active and archived one", func(v string) error {
			if strings.TrimSpace(v) == "" {
				return errors.New("-repo names a repository")
			}
			named = append(named, v)
			return nil
		})
		repair = fs.Bool("repair", false, "repair what the check finds — in place where only the copy's "+
			"directory is out of place, by a rebuild otherwise — then check the copy again")
		asJSON = fs.Bool("json", false, "print each verification as the node answered it")
	})
	if err != nil {
		return err
	}
	client = client.forCodeReads()
	ctx := context.Background()
	targets, floor, err := codeVerifyTargets(ctx, client, named)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		// Words, so with -json they go beside the answers, never among them.
		said := stdout
		if *asJSON {
			said = stderr
		}
		_, err := fmt.Fprintln(said, "no repositories: code.repos declares none")
		return err
	}
	failed := 0
	for _, name := range targets {
		clean, err := verifyOne(ctx, client, name, floor, *repair, *asJSON, productionVerifyWaits(), stdout,
			stderr)
		if err != nil {
			// The node serves no verify: the -url is wrong for every
			// repository alike.
			return err
		}
		if !clean {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d repositories did not verify clean on %s", failed, len(targets), client.base)
	}
	return nil
}

// codeVerifyTargets is what to verify and the position to verify it at: the
// named repositories — a name nobody declared is the verify's own refusal to
// say, naming code.repos — or, naming none, every active and archived
// repository, from one listing.
//
// ONE BARRIER A SWEEP: the listing's first page is read at the log's head, so
// a repository declared a moment ago is not missed for an old copy, and its
// position is the FLOOR every page after it and every verify is read at
// (min_position) — where a read or a verify at the head appends a barrier to
// the code log each. Naming repositories, the floor is one read at the head of
// the listing's first row, which is all it is read for.
func codeVerifyTargets(ctx context.Context, client *nodeClient, named []string) ([]string, codePosition, error) {
	query := url.Values{"read_level": {"linearizable"}, "status": {"active,archived"}}
	if len(named) > 0 {
		floor, _, err := codePages(ctx, client, "/code/repos", query, 1, 1,
			func(json.RawMessage, codeReadView) bool { return false })
		if err != nil {
			return nil, codePosition{}, codeReadRefused(client, err)
		}
		return named, floor, nil
	}
	var targets []string
	floor, _, err := codePages(ctx, client, "/code/repos", query, 0, code.MaxReposPerPage,
		func(_ json.RawMessage, page codeReadView) bool {
			for _, r := range page.Repos {
				targets = append(targets, r.Name)
			}
			return true
		})
	if err != nil {
		return nil, codePosition{}, codeReadRefused(client, err)
	}
	return targets, floor, nil
}

// verifyOne verifies one repository on the node at floor and prints what it
// found, answering whether the node's copy is clean — or not held, which has
// nothing to be unclean — and an error only where the node serves no verify
// at all, which is the -url's and every repository's alike.
//
// THE CHECK IS THE NODE'S OPERATION: the POST begins it and waits a moment
// for it (host.VerifyHold), answering it ended — a small copy's — or running,
// and the command then reads it until it ends, because a check reads every
// byte of the repository and can outlive any proxy's read timeout. A refusal
// the node says clears by waiting — its verify slot or the check's memory
// held, a node stopping — is asked again after the node's own Retry-After,
// for up to waits.refusedFor, since the node refused it having read nothing
// and the retry costs what the refusal did.
func verifyOne(ctx context.Context, client *nodeClient, name string, floor codePosition, repair, asJSON bool,
	waits codeVerifyWaits, stdout, stderr io.Writer,
) (bool, error) {
	query := url.Values{}
	if repair {
		query.Set("repair", "true")
	}
	if floor.Stream != "" {
		query.Set("min_position", floor.String())
	}
	path := "/code/repos/" + url.PathEscape(name) + "/verify"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var (
		raw    json.RawMessage
		header http.Header
	)
	refusedUntil := time.Now().Add(waits.refusedFor)
	for {
		var err error
		header, err = client.exchange(ctx, http.MethodPost, path, nil, nil, &raw)
		if err == nil {
			break
		}
		var refusal *nodeRefusal
		switch {
		case errors.As(err, &refusal) && refusal.Code == "not_found" && refusal.Reason == "not_held":
			// No verification to print as JSON: in that mode stdout is the
			// node's answers alone, and this is said beside them.
			said := stdout
			if asJSON {
				said = stderr
			}
			fmt.Fprintf(said, "%s: not held on this node — nothing here to check (verify the git nodes "+
				"that serve it)\n", name)
			return true, nil
		case errors.As(err, &refusal) && refusal.Code == "no_route":
			return false, fmt.Errorf("%s serves no verify: only a node whose roles include both ingress and "+
				"git keeps a cache to check — point -url at one", client.base)
		case errors.As(err, &refusal) && refusal.Status == http.StatusServiceUnavailable &&
			refusal.RetryAfter > 0 && time.Now().Add(refusal.RetryAfter).Before(refusedUntil):
			fmt.Fprintf(stderr, "%s: %v\n  asking again in %s\n", name, err, refusal.RetryAfter)
			if !sleepFor(ctx, refusal.RetryAfter) {
				fmt.Fprintf(stderr, "%s: %v\n", name, ctx.Err())
				return false, nil
			}
			continue
		default:
			fmt.Fprintf(stderr, "%s: %v\n", name, err)
			return false, nil
		}
	}
	var v codeVerificationView
	if err := json.Unmarshal(raw, &v); err != nil {
		fmt.Fprintf(stderr, "%s: the node's verification is not the shape this build reads: %v\n", name, err)
		return false, nil
	}
	raw, ok := awaitVerification(ctx, client, name, &v, raw, header, waits, stderr)
	if !ok {
		return false, nil
	}
	if asJSON {
		fmt.Fprintf(stdout, "%s\n", raw)
	}
	return renderVerification(stdout, stderr, name, v, repair, asJSON), nil
}

// sleepFor waits d, or less where ctx ends first — answering whether the
// whole wait passed.
func sleepFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// checkEndsBy is when, on this machine's clock, a verification answered with
// header says its node ends the check it is running, waits.grace past it —
// the deadline the node states, measured against the answer's own Date (the
// node's clock) so the two clocks need not agree; zero while it states none
// (bringing the copy to its rows, or a repair — a read in place or a
// rebuild — whose reads of the object store are bounded each on its own).
func checkEndsBy(v codeVerificationView, header http.Header, received time.Time, grace time.Duration) time.Time {
	if v.Deadline.IsZero() {
		return time.Time{}
	}
	at, err := http.ParseTime(header.Get("Date"))
	if err != nil {
		// No Date to measure by — something in front of the node took it
		// off: the deadline as it reads on this machine's clock.
		return v.Deadline.Add(grace)
	}
	return received.Add(v.Deadline.Sub(at)).Add(grace)
}

// awaitVerification reads v — raw as the node wrote it, answered with header
// — until it is no longer running, answering its last answer as the node
// wrote it, or false — said on stderr — where it could not be waited for: the
// node still running a check past the deadline it stated for it, silent past
// waits.lostContact, or no longer keeping the verification.
func awaitVerification(ctx context.Context, client *nodeClient, name string, v *codeVerificationView,
	raw json.RawMessage, header http.Header, waits codeVerifyWaits, stderr io.Writer,
) (json.RawMessage, bool) {
	at := "/code/repos/" + url.PathEscape(name) + "/verify/" + url.PathEscape(v.ID)
	endsBy := checkEndsBy(*v, header, time.Now(), waits.grace)
	var lostSince time.Time
	for attempt := 1; v.State == "running"; attempt++ {
		if !endsBy.IsZero() && time.Now().After(endsBy) {
			fmt.Fprintf(stderr, "%s: %s said it would end its check (verification %s) by %s and still says it "+
				"is running; read GET %s there for its outcome\n", name, client.base, v.ID,
				v.Deadline.UTC().Format(time.RFC3339), at)
			return nil, false
		}
		wait := backoff.Doubling(attempt, waits.pollFirst, waits.pollMax)
		if !endsBy.IsZero() {
			wait = min(wait, max(time.Until(endsBy), 0))
		}
		if !sleepFor(ctx, wait) {
			fmt.Fprintf(stderr, "%s: %v; the check (verification %s) goes on on the node — read GET %s "+
				"there for its outcome\n", name, ctx.Err(), v.ID, at)
			return nil, false
		}
		var next json.RawMessage
		header, err := client.exchange(ctx, http.MethodGet, at, nil, nil, &next)
		var lost noAnswer
		switch {
		case err == nil:
			lostSince = time.Time{}
		case errors.As(err, &lost):
			if lostSince.IsZero() {
				lostSince = time.Now()
			}
			if time.Since(lostSince) < waits.lostContact {
				continue
			}
			fmt.Fprintf(stderr, "%s: lost the node while it checked (verification %s): %v\n  read GET %s "+
				"there once it answers, or run this again — a check still running is handed back\n",
				name, v.ID, err, at)
			return nil, false
		default:
			var refusal *nodeRefusal
			if errors.As(err, &refusal) && refusal.Code == "not_found" {
				fmt.Fprintf(stderr, "%s: the node no longer keeps verification %s — it restarted, or "+
					"another verify of %s began — run this again\n", name, v.ID, name)
				return nil, false
			}
			fmt.Fprintf(stderr, "%s: reading verification %s: %v\n", name, v.ID, err)
			return nil, false
		}
		// A FRESH VALUE: a field the new answer leaves out — a deadline once
		// the check has ended — is not the last answer's.
		var read codeVerificationView
		if err := json.Unmarshal(next, &read); err != nil {
			fmt.Fprintf(stderr, "%s: the node's verification is not the shape this build reads: %v\n", name, err)
			return nil, false
		}
		*v, raw = read, next
		endsBy = checkEndsBy(*v, header, time.Now(), waits.grace)
	}
	return raw, true
}

// renderVerification prints what a verification that ended found, answering
// whether the copy is clean: no problem left — a repair that left the copy
// whole included, with what it had found and how it repaired it: in place,
// the copy's own directory put right with every pack and index it holds
// kept, or by a rebuild, or in place and then rebuilt for what that left.
// What the sync before the check repaired in place is told first and leaves
// the copy clean: the copy checked no longer holds it, though the disk that
// damaged it may hold more.
func renderVerification(stdout, stderr io.Writer, name string, v codeVerificationView, repair,
	quiet bool,
) bool {
	var (
		packs, refs               int
		repaired, problems, found []string
		inPlace, rebuilt          bool
	)
	if r := v.Report; r != nil {
		packs, refs, repaired, problems, found = r.Packs, r.Refs, r.RepairedBySync, r.Problems, r.Found
		inPlace, rebuilt = r.RepairedInPlace, r.Rebuilt
	}
	out := stdout
	if quiet {
		// The JSON is the answer; what it says in words goes beside it.
		out = stderr
	}
	counts := fmt.Sprintf("%d packs, %d refs", packs, refs)
	switch v.State {
	case "done":
	case "failed":
		fmt.Fprintf(stderr, "%s: the check failed (%s): %s\n", name, v.Error, v.Detail)
		for _, p := range repaired {
			fmt.Fprintf(stderr, "  repaired in place by the sync before it: %s\n", p)
		}
		for _, p := range problems {
			fmt.Fprintf(stderr, "  found before it failed: %s\n", p)
		}
		return false
	default:
		fmt.Fprintf(stderr, "%s: the node answered a verification state this build does not know (%q)\n",
			name, v.State)
		return false
	}
	if len(repaired) > 0 {
		fmt.Fprintf(out, "%s: the sync before the check found damage on disk and repaired it in place:\n", name)
		for _, p := range repaired {
			fmt.Fprintf(out, "  %s\n", p)
		}
	}
	switch {
	case inPlace && rebuilt:
		fmt.Fprintf(out, "%s: out of place, repaired in place and then rebuilt for what that left (%s); "+
			"the check before the repair found:\n", name, counts)
	case inPlace:
		fmt.Fprintf(out, "%s: out of place, and repaired in place (%s); the check before the repair found:\n",
			name, counts)
	case rebuilt:
		fmt.Fprintf(out, "%s: damaged, and rebuilt (%s); the check before the rebuild found:\n", name, counts)
	}
	if inPlace || rebuilt {
		for _, p := range found {
			fmt.Fprintf(out, "  %s\n", p)
		}
	}
	if len(problems) == 0 {
		switch {
		case rebuilt:
			fmt.Fprintf(out, "%s: the rebuilt copy is whole\n", name)
		case inPlace:
			fmt.Fprintf(out, "%s: the repaired copy is whole\n", name)
		default:
			fmt.Fprintf(out, "%s: ok (%s)\n", name, counts)
		}
		return true
	}
	switch {
	case rebuilt:
		fmt.Fprintf(out, "%s: the rebuilt copy still has %d problem(s) — what a rebuild leaves is in the "+
			"object store or the rows, and each says which:\n", name, len(problems))
	case inPlace:
		fmt.Fprintf(out, "%s: the copy repaired in place still has %d problem(s):\n", name, len(problems))
	default:
		fmt.Fprintf(out, "%s: %d problem(s) (%s):\n", name, len(problems), counts)
	}
	for _, p := range problems {
		fmt.Fprintf(out, "  %s\n", p)
	}
	if !repair {
		fmt.Fprintf(out, "  run it again with -repair to repair this node's copy\n")
	}
	return false
}

// ---- export ---------------------------------------------------------------

// codeExport is `crewlet code export`: one repository's every ref and object,
// written to a git bundle on this machine, through the operator's own git
// (operatorGit) — a clone of the node's repository, bundled.
//
// THE FILE IS WRITTEN AS <file>.partial AND RENAMED ONCE CHECKED, so an
// export that was interrupted, or wrote a bundle a clone would refuse, leaves
// nothing that looks finished; and the mirror it bundles from is kept BESIDE
// the file rather than in the system's temporary directory, because a mirror
// is the repository's whole size and the volume the operator chose for the
// bundle is the one sized for it, where a temporary directory is often a
// small, memory-backed one.
func codeExport(environ config.Source, args []string, stdout, stderr io.Writer) error {
	// THE TWO SUBJECTS LEAD, before any flag (`work purge`'s idiom): the
	// optional config document a node client takes after them is a third
	// positional, and only their order tells the three apart.
	name, rest := splitSubject(args)
	file, rest := splitSubject(rest)
	if name == "" || file == "" {
		fmt.Fprintln(stderr, "usage: crewlet code export <name> <file.bundle> [<config.yaml>] [-url URL] "+
			"[-git PATH]")
		return errors.New("name the repository to export and the bundle file to write, before any flag")
	}
	var gitPath *string
	client, err := nodeClientFor(environ, rest, "code export", stderr, func(fs *flag.FlagSet) {
		gitPath = fs.String("git", "", "the git binary to run; empty is the first on PATH")
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// THE REPOSITORY FIRST, at the log's head: a name that is not declared
	// is said in the node's words, naming code.repos, rather than as git's
	// failed clone.
	var answer codeReadView
	if err = client.forCodeReads().get(ctx,
		"/code/repos/"+url.PathEscape(name)+"?read_level=linearizable", &answer); err != nil {
		return codeReadRefused(client, err)
	}
	if answer.Repo == nil {
		return fmt.Errorf("the node's answer for %s carries no repository", name)
	}
	switch s := answer.Repo.Status; s {
	case "active", "archived":
	case "deleted":
		return fmt.Errorf("%s is deleted, and the node serves it to nobody: declare it again in code.repos "+
			"to restore it, then export it", name)
	default:
		// purging — which nothing undoes — or a status a newer node added.
		return fmt.Errorf("%s is %s, and the node serves it to nobody", name, s)
	}

	g, err := newOperatorGit(*gitPath, client, os.Environ())
	if err != nil {
		return err
	}
	if err = g.atClientFloor(ctx); err != nil {
		return err
	}
	target, err := filepath.Abs(file)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", file, err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(target), ".crewlet-export-")
	if err != nil {
		return fmt.Errorf("make the export's working directory beside %s: %w", target, err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	mirror := filepath.Join(tmp, "mirror.git")
	if _, err = g.run(ctx, g.targetEnv(), "", "clone", "--mirror", g.repoURL(name), mirror); err != nil {
		return err
	}
	listed, err := g.run(ctx, g.environ, mirror, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return err
	}
	refs := bytes.Count(listed, []byte("\n"))
	if refs == 0 {
		return fmt.Errorf("%s holds no refs yet, so there is nothing to export", name)
	}
	partial := target + ".partial"
	defer func() { _ = os.Remove(partial) }()
	if _, err = g.run(ctx, g.environ, mirror, "bundle", "create", partial, "--all"); err != nil {
		return err
	}
	if err = checkBundle(ctx, partial); err != nil {
		return fmt.Errorf("the bundle git wrote for %s is not whole, so nothing was put at %s: %w", name, file, err)
	}
	if err = os.Rename(partial, target); err != nil {
		return fmt.Errorf("put the bundle in place: %w", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "exported %s: %d refs, %s to %s\n", name, refs, humanBytes(info.Size()), file)
	return err
}

// maxBundleHeader bounds a bundle's header — its signature, capabilities and
// one line a ref — as checkBundle reads it: 64 MiB, maxGitOutput's figure,
// above the ref lines of code.MaxRefsPerRepo refs (a hundred thousand of at
// most 256 bytes each with their ids, some 30 MiB), so a file whose header
// does not end inside it is not a bundle of a repository this host serves.
const maxBundleHeader = maxGitOutput

// checkBundle holds the git bundle at path to what a clone of it holds it to,
// reading it once from start to end: its HEADER a v2 or v3 bundle's — no
// prerequisite, since a bundle of every ref of a mirror has none, and a ref
// line for at least one ref — and its PACK whole: git's `PACK` signature, a
// version git writes, an object count, and the trailer, the hash of every
// byte before it (SHA-1, or SHA-256 where the bundle names that object
// format). A pack cut short or with a byte changed is caught by that one
// hash, at hashing speed — git's hashfile rule, held by its one
// implementation (code.HashTrailer, which a git node holds its own files to),
// so the bytes are judged here exactly as the node that serves them judges
// its packs.
//
// STOPPED WITH ITS CALLER: the pack is the repository's whole size, and the
// export catches an interrupt to cancel ctx rather than die of it, so a check
// that took no context would read on to the end of a repository-sized file
// after the operator asked it to stop. ctx is checked before every read.
//
// NOT `git bundle verify`, which reads the header and the prerequisites and
// never the pack: a bundle with eight bytes of its pack overwritten, or cut
// short by a hundred, is "okay" to it and fails the clone with `inflate: data
// stream error … index-pack died`. And not `git index-pack`, which would
// inflate and hash every object — measured at 0.11–0.14 s of CPU a mebibyte
// (gitproc's PackWorkPerMiB) — when a pack whose trailer is its bytes' hash is
// the pack `bundle create` wrote; the clone that restores it indexes it.
func checkBundle(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // read-only: what was read is the answer
	info, err := f.Stat()
	if err != nil {
		return err
	}
	sum, header, err := bundleHeader(bufio.NewReaderSize(f, 64<<10))
	if err != nil {
		return err
	}
	pack := io.NewSectionReader(f, header, info.Size()-header)
	var head [12]byte
	if n, headErr := pack.ReadAt(head[:], 0); n < len(head) {
		return fmt.Errorf("its pack has no header: %w", headErr)
	}
	if string(head[:4]) != "PACK" {
		return fmt.Errorf("what follows its header is no pack (it begins %q)", head[:4])
	}
	if version := binary.BigEndian.Uint32(head[4:8]); version != 2 && version != 3 {
		return fmt.Errorf("its pack is version %d, which git does not write", version)
	}
	if binary.BigEndian.Uint32(head[8:12]) == 0 {
		return errors.New("its pack holds no object, though its header names a ref")
	}
	if pack.Size() < int64(len(head)+sum.Size()) {
		return errors.New("its pack ends before its trailer")
	}
	t, err := code.HashTrailer(ctx, pack, pack.Size(), sum, code.Pieces{})
	switch {
	case err != nil:
		// io.ErrUnexpectedEOF among them: the file shortened as it was read.
		return fmt.Errorf("reading its pack: %w", err)
	case !t.Sound():
		return fmt.Errorf("its pack's bytes hash to %x, and its trailer says %x: it was cut short or changed",
			t.Sum, t.Held)
	}
	return nil
}

// bundleHeader reads a bundle's header from r, through the blank line that
// ends it, answering the hash its pack's trailer is made with — fresh — and
// how many bytes the header takes: where its pack begins in the file.
func bundleHeader(r *bufio.Reader) (hash.Hash, int64, error) {
	read := 0
	line := func() (string, error) {
		s, err := r.ReadString('\n')
		read += len(s)
		if read > maxBundleHeader {
			return "", fmt.Errorf("its header runs past %s", humanBytes(maxBundleHeader))
		}
		if err != nil {
			return "", fmt.Errorf("its header ends before the blank line that ends it: %w", err)
		}
		return strings.TrimSuffix(s, "\n"), nil
	}
	signature, err := line()
	if err != nil {
		return nil, 0, err
	}
	if signature != "# v2 git bundle" && signature != "# v3 git bundle" {
		return nil, 0, fmt.Errorf("it is not a git bundle (its first line is %q)", signature)
	}
	sum, refs := sha1.New(), 0
	for {
		l, err := line()
		switch {
		case err != nil:
			return nil, 0, err
		case l == "":
			if refs == 0 {
				return nil, 0, errors.New("its header names no ref")
			}
			return sum, int64(read), nil
		case strings.HasPrefix(l, "@"):
			// A v3 capability: the object format is the one that changes
			// what the trailer is.
			if format, ok := strings.CutPrefix(l, "@object-format="); ok && format == "sha256" {
				sum = sha256.New()
			}
		case strings.HasPrefix(l, "-"):
			return nil, 0, fmt.Errorf("it needs a commit it does not hold (%s), which no bundle of every ref of "+
				"a mirror does", strings.TrimPrefix(l, "-"))
		default:
			oid, ref, ok := strings.Cut(l, " ")
			if !ok || ref == "" || (len(oid) != 2*sha1.Size && len(oid) != 2*sha256.Size) {
				return nil, 0, fmt.Errorf("its header holds a line that is no ref: %q", l)
			}
			refs++
		}
	}
}

// ---- the operator's git -----------------------------------------------------

// operatorGit is the OPERATOR's git, run with the operator's own environment —
// their proxy, their configuration, their network — because export (and
// import) are client-side: the engine dials nothing. Outside internal/ and so
// outside gitproc's exec gate on purpose: gitproc is the ENGINE's git, built
// with no environment of anyone's, and this one is the person's.
type operatorGit struct {
	path string // -git, else the first git on PATH: an absolute path either way
	base string // the node's base URL
	// token is the node's token: handed to git ONLY through the helper below,
	// in the environment, never on a command line, where ps and a log see it.
	token string
	self  string // this binary, links resolved: the helper git runs
	// environ is the operator's environment (os.Environ), and configCount
	// the GIT_CONFIG_COUNT it already carries — settings this command adds
	// after, never over.
	environ     []string
	configCount int
	// terminal reports a controlling terminal, where a git that talks to
	// another host with the operator's own credentials may ask its person
	// something ([gitCall].asks).
	terminal bool
}

// newOperatorGit is the git export runs: the one -git names or the first on
// PATH, talking to client's node with client's token, under environ.
//
// THE PATH IS MADE ABSOLUTE HERE: os/exec resolves a relative Cmd.Path
// against Cmd.Dir, so `-git ./bin/git` ran from the working directory for the
// version check and the clone and was "no such file" for every command run in
// the mirror; and a bare name is looked up on PATH, as a shell would.
func newOperatorGit(gitPath string, client *nodeClient, environ []string) (operatorGit, error) {
	path := gitPath
	switch {
	case path == "":
		found, err := exec.LookPath("git")
		if err != nil {
			return operatorGit{}, fmt.Errorf("no git on PATH (%w): install git %s or newer (gitproc.ClientMinVersion, "+
				"the client floor), or name one with -git", err, gitproc.ClientMinVersion)
		}
		path = found
	case !strings.ContainsRune(path, filepath.Separator):
		found, err := exec.LookPath(path)
		if err != nil {
			return operatorGit{}, fmt.Errorf("-git %s: %w", path, err)
		}
		path = found
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return operatorGit{}, fmt.Errorf("-git %s: %w", gitPath, err)
	}
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return operatorGit{}, fmt.Errorf("find this binary, which git runs as its credential helper: %w", err)
	}
	count := 0
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			// git refuses a count it cannot read, so this does too, rather
			// than numbering its own settings over the operator's.
			count, err = strconv.Atoi(strings.TrimSpace(v))
			if err != nil || count < 0 {
				return operatorGit{}, fmt.Errorf("GIT_CONFIG_COUNT=%q in this environment is not a count git "+
					"reads", v)
			}
		}
	}
	return operatorGit{path: path, base: client.base, token: client.token, self: self,
		environ: environ, configCount: count, terminal: controllingTerminal()}, nil
}

// controllingTerminal reports whether this process has a controlling
// terminal — the one git and ssh ask a person on, /dev/tty, which opens only
// where there is one — whatever its standard streams are.
func controllingTerminal() bool {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = tty.Close()
	return true
}

// credentialHelper is the helper a git that talks to the node runs: this
// binary's `code credential`, spelled for the shell git runs it through.
func credentialHelper(self string) string {
	return "!" + shellQuote(self) + " code credential"
}

// shellQuote is s as one POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// repoURL is the git URL of a repository on the node: the API's base, then
// the one path a repository's git is served at ([auth.GitRepoPath]).
func (g operatorGit) repoURL(name string) string {
	return g.base + auth.GitRepoPath(name)
}

// targetEnv is the environment of a git that talks to the NODE: the
// operator's, plus GIT_TERMINAL_PROMPT=0 (a missing credential fails rather
// than prompts), the token in CREWLET_API_TOKEN, and — numbered after every
// setting the operator's own GIT_CONFIG_COUNT carries — two settings:
// credential.helper EMPTY, which clears every helper the operator's
// configuration lists for this one invocation, so a stored and stale password
// for the same host is never sent instead; then this binary's helper SCOPED
// TO THE NODE'S URL (credential.<base>.helper), so the token is offered to
// the node and to nothing else — not to a host a redirect from the node's
// address, or from a proxy in front of it, sends git to, which git asks for a
// credential again (http.followRedirects=initial, its default) and an
// unscoped helper would answer. A git that talks to anything else gets none
// of this.
func (g operatorGit) targetEnv() []string {
	n := g.configCount
	// Appended LAST: where a key repeats, exec gives the process the last.
	return append(slices.Clone(g.environ),
		"GIT_TERMINAL_PROMPT=0",
		apiTokenEnv+"="+g.token,
		fmt.Sprintf("GIT_CONFIG_KEY_%d=credential.helper", n),
		fmt.Sprintf("GIT_CONFIG_VALUE_%d=", n),
		fmt.Sprintf("GIT_CONFIG_KEY_%d=credential.%s.helper", n+1, g.base),
		fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n+1, credentialHelper(g.self)),
		fmt.Sprintf("GIT_CONFIG_COUNT=%d", n+2),
	)
}

// atClientFloor refuses a git below the client floor (gitproc.ClientMinVersion)
// — the operator's git TALKS to a git node and runs none of its own, so the
// node floor is not its business — as gitproc's own *ErrTooOld, naming the
// git, its version and the floor, with the one remedy only this command has.
func (g operatorGit) atClientFloor(ctx context.Context) error {
	out, err := g.run(ctx, g.environ, "", "version")
	if err != nil {
		return err
	}
	v, err := gitproc.ParseVersion(string(out))
	if err != nil {
		return fmt.Errorf("git at %s: %w", g.path, err)
	}
	if !v.AtLeast(gitproc.ClientMinVersion) {
		return fmt.Errorf("%w; or name one with -git", &gitproc.ErrTooOld{Path: g.path, Found: v, Floor: gitproc.FloorClient})
	}
	return nil
}

// maxGitOutput bounds what one git command's stdout is captured to: an
// ls-remote of code.MaxRefsPerRepo refs is about 6 MiB, and a
// cat-file --batch-check over a large history tens of MiB. Past it the
// command fails rather than holding an unbounded answer.
const maxGitOutput = 64 << 20

// gitStderrTail is how much of git's stderr an error quotes: the last 64 KiB,
// which holds every line a failure ends with.
const gitStderrTail = 64 << 10

// gitKillGrace is how long a git asked to stop is given before it is killed,
// for gitproc.KillGrace's reason: git removes its lock files on SIGTERM.
const gitKillGrace = gitproc.KillGrace

// run runs the operator's git under env — on the repository at gitDir where
// one is named — answering its stdout; a failure quotes the end of its stderr.
func (g operatorGit) run(ctx context.Context, env []string, gitDir string, args ...string) ([]byte, error) {
	return g.call(ctx, gitCall{env: env, gitDir: gitDir}, args...)
}

// gitCall is how one invocation of the operator's git is run: under env, on
// the repository at gitDir where one is named, with config's `key=value`
// settings on its command line where any are given, with stdin where one is
// given, its stdout into stdout where one is given (captured, bounded by
// maxGitOutput, and answered where not), and its stderr copied to live as it
// is written where a person is watching. asks marks a git that talks to
// ANOTHER HOST with the operator's own credentials, which may ask its person
// something on the terminal ([operatorGit.call]).
type gitCall struct {
	env    []string
	gitDir string
	config []string
	stdin  []byte
	stdout io.Writer
	live   io.Writer
	asks   bool
}

// call runs the operator's git as c says, answering what it wrote to stdout
// where nothing else read it — on a failure too, since a push's porcelain
// lines are its answer whatever its exit — and an error quoting the end of
// its stderr.
//
// ITS PROCESS GROUP IS ITS OWN, so an interrupt — the context's — ends git
// and everything it started, the remote helper among them, with one signal.
// EXCEPT FOR A GIT THAT MAY ASK ITS PERSON SOMETHING (c.asks) where this
// process has a controlling terminal: git — or the ssh it starts — asks for a
// user name and password no credential helper holds, a host key on a first
// connection, a key's passphrase with no agent, on /dev/tty; and a process
// group of its own is a BACKGROUND group to that terminal, which the kernel
// stops (SIGTTIN) at its first read, so the import waited on a stopped git
// until somebody pressed Ctrl-C. That git runs in this command's own group
// instead, exactly as a git the person typed would: in the foreground, its
// prompt answerable, and a Ctrl-C reaching git and ssh from the terminal as
// it reaches this command. The context then ends git alone (SIGTERM, then
// WaitDelay's kill), which is what its remote helper and ssh end with. Where
// there is no terminal — a CI job, a cron — nothing can be asked, git's
// prompt fails on its own and says so, and the call keeps its own group.
// The node-facing calls never prompt (targetEnv: GIT_TERMINAL_PROMPT=0, and
// this binary as the credential helper), and keep it always.
//
// THE READER CAN STOP IT: a stdout writer that fails ends git at its next
// write, which meets a closed pipe, and that writer's error is the answer —
// what stopped git, rather than the broken pipe git died of.
//
// THE REPOSITORY IS NAMED, never found: `--git-dir`, not a working directory
// git discovers it from. The operator's configuration is deliberately
// inherited, and safe.bareRepository=explicit — a hardening git has offered
// since 2.38 — refuses every bare repository it would discover, the mirror
// among them ("cannot use bare repository").
func (g operatorGit) call(ctx context.Context, c gitCall, args ...string) ([]byte, error) {
	verb := args[0]
	lead := make([]string, 0, 1+2*len(c.config))
	if c.gitDir != "" {
		lead = append(lead, "--git-dir="+c.gitDir)
	}
	for _, kv := range c.config {
		// -c rather than the environment: the command line's settings are
		// read after GIT_CONFIG_COUNT's and GIT_CONFIG_PARAMETERS', so they
		// outrank every setting the operator's environment carries.
		lead = append(lead, "-c", kv)
	}
	args = append(lead, args...)
	cmd := exec.CommandContext(ctx, g.path, args...)
	cmd.Env = c.env
	if c.asks && g.terminal {
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	} else {
		procgroup.Set(cmd)
		cmd.Cancel = func() error { return procgroup.Terminate(cmd.Process.Pid) }
	}
	cmd.WaitDelay = gitKillGrace
	if c.stdin != nil {
		cmd.Stdin = bytes.NewReader(c.stdin)
	}
	captured := &boundedBuffer{max: maxGitOutput, verb: verb}
	var out io.Writer = captured
	if c.stdout != nil {
		out = c.stdout
	}
	reader := &stoppingWriter{w: out}
	stderr := &tailBuffer{max: gitStderrTail}
	cmd.Stdout, cmd.Stderr = reader, io.Writer(stderr)
	if c.live != nil {
		cmd.Stderr = io.MultiWriter(stderr, c.live)
	}
	err := cmd.Run()
	if reader.err != nil {
		return captured.Bytes(), reader.err
	}
	if err != nil {
		if ctx.Err() != nil {
			return captured.Bytes(), fmt.Errorf("git %s: interrupted", verb)
		}
		return captured.Bytes(), fmt.Errorf("git %s: %w\n%s", verb, err, strings.TrimRight(stderr.String(), "\n"))
	}
	return captured.Bytes(), nil
}

// stoppingWriter is git's stdout as [operatorGit.call] reads it: the reader's
// first refusal, kept, so it is the answer rather than the broken pipe git
// dies of after it.
type stoppingWriter struct {
	w   io.Writer
	err error
}

func (s *stoppingWriter) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.w.Write(p)
	if err != nil {
		s.err = err
	}
	return n, err
}

// boundedBuffer is a buffer that refuses what would take it past max — the
// write fails, so git's next write meets a closed pipe and it stops.
type boundedBuffer struct {
	bytes.Buffer
	max  int
	verb string
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		return 0, fmt.Errorf("git %s wrote more than %s, which this command does not hold", b.verb,
			humanBytes(int64(b.max)))
	}
	return b.Buffer.Write(p)
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return textcut.Tail(string(t.buf), t.max) }
