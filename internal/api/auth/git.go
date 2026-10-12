package auth

import (
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// GitPrefix is where a person's git client reaches the code host, and
// CodePrefix the code host's REST reads.
//
// Both are ALWAYS GUARDED, and [GuardedPrefixes] reads these constants rather
// than spelling its own, so the guard and the classifier cannot disagree about
// where git starts. Each ends in a slash, which is what stops it covering a
// sibling: /github is not a git route, and /codes is not a code read.
const (
	GitPrefix  = "/git/"
	CodePrefix = "/code/"
)

// GitOp is what a git request asks of the code host. A closed set.
type GitOp string

const (
	// GitNone is a request that is not under [GitPrefix].
	GitNone GitOp = ""
	// GitFetch reads: the upload-pack advertisement (GET or HEAD info/refs
	// asking for git-upload-pack, or for no service at all) and the
	// upload-pack RPC (POST git-upload-pack).
	GitFetch GitOp = "fetch"
	// GitPush is the receive-pack advertisement and RPC, and EVERY OTHER
	// request under [GitPrefix].
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
// EVERY DOUBT IS A PUSH, the more restrictive answer — refused while draining,
// a person required — because a fetch misread as a push costs a retry and a
// push misread as a fetch costs the guarantee:
//
//   - any path under [GitPrefix] that is not one of the two fetch shapes,
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
// nothing does — and whether that path is under [GitPrefix] at all. The
// repository segment ends at the first slash, so the tail of a path whose
// repository name held one is no git route's.
func gitTail(r *http.Request) (tail string, under bool) {
	rest, under := strings.CutPrefix(r.URL.Path, GitPrefix)
	if !under {
		return "", false
	}
	_, tail, _ = strings.Cut(rest, "/")
	return tail, true
}

// advertises reports whether a request under [GitPrefix] whose path ends in
// tail is an advertisement: git asks for info/refs with a GET, and a HEAD of it
// is the same question without the body. A POST to it is nothing git sends.
func advertises(r *http.Request, tail string) bool {
	return tail == "info/refs" && (r.Method == http.MethodGet || r.Method == http.MethodHead)
}

// GitText answers a git client in the one form it prints: text/plain, the
// sentence, and a newline.
//
// git's remote-curl shows an HTTP refusal's body only when it is text/plain
// (show_http_message), each line prefixed `remote: `; the API's JSON, which
// every other client reads, prints nothing at all to a person at a terminal.
// So the guard's 401, the drain gate's 503 and every refusal the code host
// makes before git runs are written here, and in no other form.
//
// The response is never sniffed into anything else, and never cached in front
// of the node: a refusal is about this moment — a drain, a credential — and a
// proxy that kept one would go on refusing a request the node now serves.
func GitText(w http.ResponseWriter, status int, sentence string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, strings.TrimRight(sentence, "\n")+"\n")
}

// GitChallenge is the 401 a git route answers: a Basic challenge, then
// [GitText].
//
// git asks its credential helper — or the person at the terminal — for a
// credential only after a challenge, so a git route's first request carries
// none by design, and a 401 without one ends the clone with nothing asked.
func GitChallenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", gitRealm)
	GitText(w, http.StatusUnauthorized, gitAuthSentence)
}

// gitRealm is the challenge: Basic, the realm git shows when it prompts, and
// UTF-8 as the encoding a credential is sent in (RFC 7617).
const gitRealm = `Basic realm="crewlet", charset="UTF-8"`

// gitAuthSentence is what git prints beside a refused credential: what to put
// in the password, which is the only half of a Basic credential the guard
// reads.
const gitAuthSentence = "authentication required: use a Crewlet API token as the password"
