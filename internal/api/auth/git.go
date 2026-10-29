package auth

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// GitPrefix is where a person's git client reaches the code host, and
// CodePrefix the code host's REST reads.
//
// Both are GUARDED, as every route the exemption list ([Unguarded]) does not
// name is, and neither is on the public listener ([Public] names neither).
// Declared once here, so the classifier and every reader of where git starts
// cannot disagree about it. Each ends in a slash, which is what stops it
// covering a sibling: /github is not a git route, and /codes is not a code
// read.
const (
	GitPrefix  = "/git/"
	CodePrefix = "/code/"
)

// BoxGitPrefix is where a CODING BOX reaches the code host with its run's
// credential: HTTP Basic, the user name host.BoxUser (`run`), the run's token
// as the password.
//
// A PUBLIC prefix ([publicPrefixes]), unlike [GitPrefix]: a box holds no
// person's or deployment's credential and is reached from wherever its
// sandbox runs, so it is served on api.public when one is set — beside /mcp/
// and /otlp/, the box's other two edges — and EXEMPT from the guard, the run's
// token being the credential and the code host's box admission the check. It
// does not begin with [GitPrefix], so no reader of where a PERSON'S git starts
// takes a box's request for one: neither transport admits the other's
// credential. [GitAccess] classifies a request under either prefix alike — one
// classifier, every doubt a push.
const BoxGitPrefix = "/box/git/"

// GitRepoPath is the path a person's git reaches one repository at, below the
// listener's base: [GitPrefix], the name, then `.git`. THE ONE SPELLING, for
// the CLI's git configuration and the clone URL a code tool answers alike, so
// the address one hands out is the address the other's git is told to use.
func GitRepoPath(name string) string {
	return GitPrefix + url.PathEscape(name) + ".git"
}

// GitOp is what a git request asks of the code host. A closed set.
type GitOp string

const (
	// GitNone is a request under neither [GitPrefix] nor [BoxGitPrefix].
	GitNone GitOp = ""
	// GitFetch reads: the upload-pack advertisement (GET or HEAD info/refs
	// asking for git-upload-pack, or for no service at all) and the
	// upload-pack RPC (POST git-upload-pack).
	GitFetch GitOp = "fetch"
	// GitPush is the receive-pack advertisement and RPC, and EVERY OTHER
	// request under [GitPrefix] or [BoxGitPrefix].
	GitPush GitOp = "push"
)

// gitOps is the closed set, in declaration order.
var gitOps = []GitOp{GitNone, GitFetch, GitPush}

// Valid reports whether o is one of the three.
func (o GitOp) Valid() bool { return slices.Contains(gitOps, o) }

// git's names for its two services, as they appear in an advertisement's
// query and as the last segment of an RPC's path.
const (
	uploadPack  = "git-upload-pack"
	receivePack = "git-receive-pack"
)

// serviceKey is the query key an advertisement names its service by.
const serviceKey = "service"

// GitAccess classifies r: THE ONE CLASSIFIER of a git request, asked by the
// guard's credential reader, the drain gate and the code host's admission.
//
// Which requests are fetches decides two things a mistake turns into a hole: a
// draining node finishes a fetch and refuses a push, and a token bound to
// nobody may fetch and never push. Read twice, the two readings would agree
// the day they were written and drift one commit at a time — and the day one
// of them took a push for a fetch, a drain would admit it. So nothing else
// CLASSIFIES a request (TestGitAccessIsTheOneClassifier). The one other reader
// of a request's service is the code host, which runs a different git process
// for each and so has to know which one was asked for — and it reads it only
// after asking GitAccess what the request is, which is why that gate allows a
// reading in internal/code/host and in no other place.
//
// A CODING BOX'S REQUEST IS CLASSIFIED AS A PERSON'S: a path under
// [BoxGitPrefix] answers exactly what the same path under [GitPrefix] does, so
// the drain gate and the code host read one answer for both transports.
//
// EVERY DOUBT IS A PUSH, the more restrictive answer — refused while draining,
// a person required — because a fetch misread as a push costs a retry and a
// push misread as a fetch costs the guarantee:
//
//   - any path under either prefix that is not one of the two fetch shapes,
//     which the mux then answers 404 (an unknown path, a dumb-protocol file, a
//     POST to info/refs, a path whose repository segment holds a slash);
//   - an advertisement asking for any service but git-upload-pack;
//   - an advertisement whose query does not parse, or that names the service
//     more than once. url.Values drops a pair it cannot read and answers the
//     FIRST of two, so a reader of the query other than this one could keep a
//     receive-pack this reading did not see — and a classifier must never
//     answer fetch for a request another reader may serve as a push.
//
// No service at all is a fetch: it is the dumb protocol's request for a
// read-only file, which the code host answers by refusing it.
func GitAccess(r *http.Request) GitOp {
	tail, under := gitTail(r)
	if !under {
		return GitNone
	}
	switch {
	case advertises(r, tail):
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return GitPush
		}
		switch service := query[serviceKey]; {
		case len(service) == 0:
			return GitFetch
		case len(service) == 1 && (service[0] == uploadPack || service[0] == ""):
			return GitFetch
		}
		return GitPush
	case tail == uploadPack && r.Method == http.MethodPost:
		return GitFetch
	}
	return GitPush
}

// GitAdvertisement reports whether r is a git ADVERTISEMENT: a GET or HEAD of
// a repository's info/refs, the request every git exchange OPENS with, a
// fetch's and a push's alike.
//
// The drain gate asks it beside [GitAccess], because it is what tells the two
// halves of a fetch apart. An advertisement BEGINS a fetch, and a draining node
// begins none: a fetch is new work — an upload-pack process tree, and the
// cache's sync from the object store before it — which the node's shutdown
// would then have to wait out. Every later request of the exchange is an RPC,
// a POST, that CONTINUES a fetch already begun — here or on a peer, since
// git's smart HTTP keeps no state between an exchange's requests — and a
// refusal there would break a fetch half done.
//
// A question about where a request falls in its exchange, never about the
// service it asks for: what a request is FOR stays [GitAccess]'s answer alone.
func GitAdvertisement(r *http.Request) bool {
	tail, under := gitTail(r)
	return under && advertises(r, tail)
}

// gitTail is what follows the repository segment of r's path — "" when
// nothing does — and whether that path is under [GitPrefix] or [BoxGitPrefix]
// at all. The repository segment ends at the first slash, so the tail of a
// path whose repository name held one is no git route's.
func gitTail(r *http.Request) (tail string, under bool) {
	rest, under := strings.CutPrefix(r.URL.Path, GitPrefix)
	if !under {
		rest, under = strings.CutPrefix(r.URL.Path, BoxGitPrefix)
	}
	if !under {
		return "", false
	}
	_, tail, _ = strings.Cut(rest, "/")
	return tail, true
}

// advertises reports whether a git request whose path ends in tail is an
// advertisement: git asks for info/refs with a GET, and a HEAD of it is the
// same question without the body. A POST to it is nothing git sends.
func advertises(r *http.Request, tail string) bool {
	return tail == "info/refs" && (r.Method == http.MethodGet || r.Method == http.MethodHead)
}

// GitText answers a git client in the one form it prints: text/plain, the
// sentence, and a newline.
//
// git's remote-curl shows an HTTP refusal's body only when it is text/plain
// (show_http_message), each line prefixed `remote: `; the API's JSON, which
// every other client reads, prints nothing at all to a person at a terminal.
// So every refusal the guard writes on a git route — its 401 challenge, a
// resolved credential's 403 ([Refusal]) and its identity 503 — the drain
// gate's 503 and every refusal the code host makes before git runs are
// written here, and in no other form.
//
// The response is never sniffed into anything else, and never cached in front
// of the node: a refusal is about this moment — a drain, a credential — and a
// proxy that kept one would go on refusing a request the node now serves.
//
// A SENTENCE IS ONE LINE, because git prints each line of the body as a
// `remote:` line of its own and a sentence spanning two reaches a person as a
// refusal broken in two. Every caller passes a sentence of its own making —
// a literal, or error text it has already folded onto one line — so one that
// still carries a newline inside it is the caller's bug: a test binary, where
// every caller's sentence is exercised, panics naming it (oneLine); a
// production one folds it onto one line rather than fail a refusal over its
// punctuation. Trailing newlines are trimmed either way, and one written.
func GitText(w http.ResponseWriter, status int, sentence string) {
	line := oneLine(sentence, testing.Testing())
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, line+"\n")
}

// oneLine is sentence as the one line [GitText] writes: its trailing newlines
// trimmed, and any newline left inside it refused by panicking when strict —
// a test binary — or folded into a space otherwise.
func oneLine(sentence string, strict bool) string {
	line := strings.TrimRight(sentence, "\n")
	if !strings.Contains(line, "\n") {
		return line
	}
	if strict {
		panic(fmt.Sprintf("auth.GitText: the sentence %q spans more than one line, and git would "+
			"print each as a remote: line of its own; write it as one", sentence))
	}
	return strings.ReplaceAll(line, "\n", " ")
}

// GitChallenge is the 401 a PERSON'S git route answers: a Basic challenge,
// then [GitText] with the sentence that names the credential a person
// presents — [GitChallengeSaying] with it.
//
// git asks its credential helper — or the person at the terminal — for a
// credential only after a challenge, so a git route's first request carries
// none by design, and a 401 without one ends the clone with nothing asked.
func GitChallenge(w http.ResponseWriter) { GitChallengeSaying(w, gitAuthSentence) }

// GitChallengeSaying is a git route's 401 in the words of its own credential:
// the same Basic challenge as [GitChallenge], then [GitText] of sentence.
//
// A SENTENCE OF ITS OWN because the person's names an API token — the one
// credential a coding box must never hold — so a box answered in a person's
// words, its helper having answered nothing, would be sent looking for one.
// The code host's box admission answers each of its 401s through this with
// what a box presents instead: its run's token.
func GitChallengeSaying(w http.ResponseWriter, sentence string) {
	w.Header().Set("WWW-Authenticate", gitRealm)
	GitText(w, http.StatusUnauthorized, sentence)
}

// GitUnavailable is a git route's 503: [GitText] with a Retry-After of
// retryAfterSeconds, and ZERO MEANS NOT RETRYABLE and writes no header —
// httpjson.Unavailable's rule, in git's form, so a git route's "come back" and
// "do not come back" are told apart the same way the API's are.
//
// ONE WRITER for the pair, for that function's reason: a 503 whose header was
// set by hand at each call site is how a dozen of the API's came to say "come
// back" without saying when. THE SENTENCE SAYS THE WAIT TOO, because git reads
// no Retry-After: it prints the body and stops, so a person at a terminal
// learns that a retry will do only from the words, and the header is for the
// proxy or the CI wrapper that does read one.
func GitUnavailable(w http.ResponseWriter, retryAfterSeconds int, sentence string) {
	if retryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	}
	GitText(w, http.StatusServiceUnavailable, sentence)
}

// gitIdentityUnavailable is the guard's 503 on a git route: this node could not
// tell who the credential is, which waiting clears — the same answer
// `503 identity_unavailable` is everywhere else, with the same
// [RetryIdentitySeconds] in its Retry-After.
func gitIdentityUnavailable(w http.ResponseWriter) {
	GitUnavailable(w, RetryIdentitySeconds, gitIdentitySentence)
}

// gitIdentitySentence is what git prints beside [gitIdentityUnavailable]'s 503.
var gitIdentitySentence = "this node cannot tell who this credential is right now: retry in " +
	strconv.Itoa(RetryIdentitySeconds) + " s, or against another node"

// gitRealm is the challenge: Basic, the realm git shows when it prompts, and
// UTF-8 as the encoding a credential is sent in (RFC 7617).
const gitRealm = `Basic realm="crewlet", charset="UTF-8"`

// gitAuthSentence is what git prints beside a refused credential: what to put
// in the password, which is the only half of a Basic credential the guard
// reads, and the helper that answers git's prompt with one.
const gitAuthSentence = "authentication required: use a Crewlet API token as the password (crewlet code credential)"
