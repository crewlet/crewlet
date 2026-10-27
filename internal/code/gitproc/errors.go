package gitproc

import "errors"

// The refusals and ends a [Pool.Run] answers with, beside the four kinds a git
// failure is classified into (classify.go). Every message built on one of
// these names what to change — a field of [Cmd], the allowlist, a file under
// HOME — because the person reading it is the one who has to change it.
var (
	// ErrBusy: no slot in the run's lane was free within [SlotWait] (or, for
	// maintenance, at all). The host answers it 503 with a Retry-After.
	ErrBusy = errors.New("gitproc: no slot was free")

	// ErrNoDeadline: a Cmd with no Deadline. No git runs unbounded.
	ErrNoDeadline = errors.New("gitproc: a git process must carry a deadline")

	// ErrEnv: a Cmd.Env name outside [EnvHookSocket] and [EnvHookToken].
	ErrEnv = errors.New("gitproc: a call site named an environment variable outside the pool's two")

	// ErrConfig: a Cmd.Config key outside the allowlist, or one of the keys
	// the pool sets for every process.
	ErrConfig = errors.New("gitproc: a configuration key outside the pool's allowlist")

	// ErrInvalid: a Cmd that cannot be a git command the engine runs — an
	// unknown class, a global option where the subcommand goes, a relative
	// directory.
	ErrInvalid = errors.New("gitproc: a malformed git command")

	// ErrRemote: a remote no git process may dial as given — a URL that is
	// not https, names a user, a query or a fragment, or a host in any form
	// but the one [ResolveRemote] reads, a header that is not one line, a
	// proxy git cannot be handed, or an SSL_CERT_DIR naming more directories
	// of authorities than libcurl reads. Never an address the public-address
	// rule refused, which is httpx.ErrNotPublic, nor a lookup that failed,
	// which is the resolver's own error, because the three ask different
	// people for different things: a malformed remote is corrected where it
	// was written, a refused address is the operator's
	// (git.import_allow_private), and a failed lookup is worth asking again.
	ErrRemote = errors.New("gitproc: not a remote a git process may dial")

	// ErrRepoShape: a Cmd.Dir (or the alternate the call names, at its real
	// path) that is definitely not a repository the engine wrote by hand —
	// always a *ShapeError, which names the rule and the repository. A file
	// that could not be READ is never this: that is the read's own error,
	// and says nothing about what the file holds.
	ErrRepoShape = errors.New("gitproc: not a repository the engine made")

	// ErrHome: the pool's HOME is not an empty directory.
	ErrHome = errors.New("gitproc: git's HOME is not an empty directory")

	// ErrNoTrace: the node's git wrote no trace2 event stream on the
	// descriptor the pool gave it, so no failure it reports could be
	// classified (trace.go) — refused at New rather than run with every
	// damaged cache read as git's own failure.
	ErrNoTrace = errors.New("gitproc: git wrote no trace2 event stream")

	// ErrDeadline: the run outlived its Cmd.Deadline and was killed.
	ErrDeadline = errors.New("gitproc: git ran past its deadline")

	// ErrStalled: the run read and wrote nothing for its Cmd.Stall and was
	// killed.
	ErrStalled = errors.New("gitproc: git stopped moving")

	// ErrInput: the caller's own Cmd.Stdin failed.
	ErrInput = errors.New("gitproc: the caller's Stdin failed")

	// ErrOutput: the caller's own Cmd.Stdout failed, or the capture outgrew
	// its bound ([ErrCaptureFull] matches too).
	ErrOutput = errors.New("gitproc: the caller's Stdout failed")

	// ErrCaptureFull: git wrote more than the capture holds. A capture is
	// never returned cut short as if it were whole.
	ErrCaptureFull = errors.New("gitproc: git's output outgrew its capture")
)
