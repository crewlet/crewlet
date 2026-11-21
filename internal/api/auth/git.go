package auth

import (
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// GitPrefix is where a person's git client reaches Gitlet, and where a person
// downloads a file or a commit's files beside it; CodePrefix is Gitlet's REST
// reads.
//
// Both are GUARDED, as every route the exemption list ([Unguarded]) does not
// name is, and neither is on the public listener ([Public] names neither).
// Declared once here, so the classifier and every reader of where git starts
// cannot disagree about it. Each ends in a slash, which is what stops it
// covering a sibling: /github is not a git route, and /codes is not a code
// read.
const (
	GitPrefix  = "/git/"
	CodePrefix = "/gitlet/"
)

// BoxGitPrefix is where a BOX reaches Gitlet's git with its own credential:
// HTTP Basic, host.BoxUser (`run`) with an executor's token (ADR-0093), or
// gitlet.CheckUser (`check`) with a check's.
//
// A PUBLIC prefix ([publicPrefixes]), unlike [GitPrefix]: a box holds no
// person's credential and is reached from wherever its sandbox runs, so it is
// served on api.public beside /mcp/ and /otlp/ and EXEMPT from the guard, its
// token being the credential and Gitlet's box admission the check. It does not
// begin with [GitPrefix], so neither transport admits the other's credential.
// [GitAccess] classifies both alike.
const BoxGitPrefix = "/box/git/"

// BoxGitletPrefix is where a CHECK'S BOX reads the one commit its check
// measures by object id — its tree, a file of it, its diff — through the API
// rather than a clone (a check whose checks file says `checkout: none`), with
// the credential it fetches with: HTTP Basic, gitlet.CheckUser and its check's
// token.
//
// PUBLIC AND UNGUARDED for [BoxGitPrefix]'s reasons, its admission Gitlet's
// (host.Server.AdmitCheck), so it is served where Gitlet is: on
// a node whose roles name git. It is no git route: [GitAccess] answers
// [GitNone] for it, and a drain serves its reads as it serves every read.
const BoxGitletPrefix = "/box/gitlet/"

// GitRepoPath is the path a person's git reaches one repository at, below the
// listener's base: [GitPrefix], the name, then `.git`. THE ONE SPELLING, for
// the CLI's git configuration and the clone URL a code tool answers alike, so
// the address one hands out is the address the other's git is told to use.
func GitRepoPath(name string) string {
	return GitPrefix + url.PathEscape(name) + ".git"
}

// GitOp is what a git request asks of Gitlet. A closed set.
type GitOp string

const (
	// GitNone is a request under neither [GitPrefix] nor [BoxGitPrefix].
	GitNone GitOp = ""
	// GitFetch reads: the upload-pack advertisement (GET or HEAD info/refs
	// asking for git-upload-pack, or for no service at all) and the
	// upload-pack RPC (POST git-upload-pack).
	GitFetch GitOp = "fetch"
	// GitPush is the receive-pack advertisement and RPC, and EVERY OTHER
	// request under [GitPrefix] or [BoxGitPrefix] but a download.
	GitPush GitOp = "push"
	// GitDownload reads a file's bytes or a commit's ZIP beside git: a GET or
	// HEAD of a download tail, made by a browser or a script, never git.
	GitDownload GitOp = "download"
)

// The two download tails, after a repository's segment under [GitPrefix]:
// the host's route patterns are spelled from them.
const (
	DownloadRawTail     = "-/raw"
	DownloadArchiveTail = "-/archive.zip"
)

// gitOps is the closed set, in declaration order.
var gitOps = []GitOp{GitNone, GitFetch, GitPush, GitDownload}

// Valid reports whether o is one of the four.
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
// guard's credential reader, the drain gate and Gitlet's admission.
//
// A draining node finishes a fetch and refuses a push, and a token bound to
// nobody may fetch and never push, so two readings drifting would let a drain
// admit a push. Nothing else CLASSIFIES a request
// (TestGitAccessIsTheOneClassifier); Gitlet reads the service only after asking
// GitAccess, to pick its git process. A box's request is classified as a
// person's: [BoxGitPrefix] answers what the same path under [GitPrefix] does.
func GitAccess(r *http.Request) GitOp {
	// EVERY DOUBT IS A PUSH: a fetch misread as a push costs a retry, a push
	// misread as a fetch costs the guarantee. That covers any path under
	// either prefix that is not a fetch shape, any service but
	// git-upload-pack, and a query that does not parse or names the service
	// twice (url.Values answers the FIRST, so another reader could serve a
	// receive-pack this one did not see).
	tail, under := gitTail(r)
	if !under {
		return GitNone
	}
	switch {
	case downloads(r, tail):
		// A GET or HEAD of exactly one download tail, its query never read.
		return GitDownload
	case advertises(r, tail):
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return GitPush
		}
		switch service := query[serviceKey]; {
		case len(service) == 0:
			// The dumb protocol, which Gitlet refuses.
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
// The drain gate asks it beside [GitAccess] to tell a fetch's halves apart. An
// advertisement BEGINS a fetch, which is new work (an upload-pack tree and a
// cache sync) a draining node must not start. Later RPCs, POSTs, CONTINUE a
// fetch begun here or on a peer (smart HTTP keeps no state between requests),
// and refusing one would break a fetch half done.
//
// It says where a request falls in its exchange, never what it is FOR, which
// stays [GitAccess]'s answer alone.
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

// downloads reports whether a request whose path ends in tail is a download:
// a GET or HEAD of exactly one of the two download tails, its path spelled
// with no escape, so an encoded slash cannot make a tail of a repository's name.
func downloads(r *http.Request, tail string) bool {
	return (tail == DownloadRawTail || tail == DownloadArchiveTail) && r.URL.RawPath == "" &&
		(r.Method == http.MethodGet || r.Method == http.MethodHead)
}

// GitText answers a git client in the one form it prints: text/plain, the
// sentence, and a newline. git's remote-curl shows an HTTP refusal's body only when it is text/plain
// (show_http_message), each line prefixed `remote: `; the API's JSON prints
// nothing to a person at a terminal. So every refusal on a git route (the
// guard's 401, 403 ([Refusal]) and identity 503, the drain gate's 503, and
// Gitlet's before git runs) is written here, in no other form.
//
// Never sniffed, and never cached in front of the node: a refusal is about this
// moment, and a proxy keeping one would go on refusing a request now served.
// A SENTENCE IS ONE LINE, as git prints each body line as its own `remote:`
// line; see [oneLine].
func GitText(w http.ResponseWriter, status int, sentence string) {
	line := oneLine(sentence)
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, line+"\n")
}

// oneLine is sentence as the one line [GitText] writes: its trailing newlines
// trimmed, and any newline left inside it folded into a space rather than
// failing a refusal over punctuation. A newline is not refused in a test
// binary, as telling one apart needs `testing`, which the binary must not link
// (cmd/crewlet's TestTheBinaryLinksNoTestScaffolding).
func oneLine(sentence string) string {
	return strings.ReplaceAll(strings.TrimRight(sentence, "\n"), "\n", " ")
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
// Gitlet's box admission answers each of its 401s through this with
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

// DownloadUnauthorized is a download's 401: no challenge, since a browser
// answers Basic with a password prompt and keeps what is typed into it.
func DownloadUnauthorized(w http.ResponseWriter) {
	GitText(w, http.StatusUnauthorized, downloadAuthSentence)
}

// downloadAuthSentence is what a download presenting no credential is told.
const downloadAuthSentence = "authentication required: sign in to Crewlet in this browser, or send a " +
	"Crewlet API token with the request, as a bearer or as a Basic password"

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
const gitAuthSentence = "authentication required: use a Crewlet API token as the password (crewlet gitlet credential)"
