package sandbox

// Placement is WHERE one seat's code work runs.
//
// # A catalogue, not a mode
//
// This used to be a single `providers.sandbox.type`, which could express only
// ONE answer for a whole company: choosing the engine host for the seat that
// needs the operator's own subscription login chose it for the seat whose
// generated code must never touch that host. Those are different decisions
// about different work, so the provider block became a CATALOGUE and the
// choice moved to the seat, as role.sandbox.run_in.
//
// The manager therefore holds a provider PER PLACEMENT rather than one
// provider, and every lifecycle call that reaches a box — create, reconnect,
// kill — has to say which one. A run's placement is persisted on its row for
// exactly that reason: the completion turn may be a different process on a
// different node, and reconnecting to a remote box through the local backend
// would report a box that vanished rather than a box that is still running.
//
// # Why the sandbox package declares it and not config
//
// The same reason SetupStep is translated rather than shared: this package
// does not import config, so a backend can be exercised with a value a test
// wrote rather than a YAML document parsed into one. The two lists are kept
// the same by the engine's own build switch, which a test walks.
type Placement string

const (
	// Direct runs the coding agent as a process tree on the engine host,
	// rooted in a per-box directory with HOME and the XDG variables pointed
	// at it and an allowlisted environment.
	//
	// THIS ISOLATES STATE, NOT THE HOST. The agent runs as the engine user
	// with its own tools enabled, so it can read anything that user can read
	// and reach anything its credentials reach. The right cell for a
	// workstation or a dedicated VM, the wrong one for a shared host.
	Direct Placement = "direct"

	// Container runs a long-lived Docker or Podman container per box, with
	// the box directory bind-mounted at DefaultHome — the same home a remote
	// backend uses, so setup steps that provision system paths work exactly
	// as they do remotely. Real host isolation, at the cost of an image with
	// the coding CLI already installed.
	Container Placement = "container"

	// E2B runs the work in a remote box: a fresh machine per run that is
	// not the engine's, which is what makes it the right cell for anything
	// running on a shared host.
	E2B Placement = "e2b"
)

// Placements is the closed set.
var Placements = []Placement{Direct, Container, E2B}

// Valid reports whether p names a cell this engine can run.
//
// A VALUE OFF THE WIRE rather than a panic: a placement arrives on a pending
// run's row, written by a peer that may be a different build, and a row from
// the future must fail the one run rather than the node reading it.
func (p Placement) Valid() bool {
	switch p {
	case Direct, Container, E2B:
		return true
	}
	return false
}

// OnEngineHost reports whether this cell runs on the engine's own machine —
// what decides whether a run can reach the operator's subscription login, and
// what an operator has to think about before enabling it on a shared host.
func (p Placement) OnEngineHost() bool { return p == Direct || p == Container }
