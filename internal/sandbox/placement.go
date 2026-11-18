package sandbox

// Placement is WHERE one seat's code work runs: a catalogue entry a seat's
// executor launches on (providers.llm.<key>.run_in), not one company-wide
// mode, since the seat needing the operator's subscription login and the seat
// whose generated code must never touch the engine host are different calls.
//
// The manager holds a provider PER PLACEMENT, so create, reconnect and kill
// each name one; a run's row persists it, as reconnecting to a remote box
// through the local backend, from another node, reports a running box gone.
//
// Declared here rather than in config, as SetupStep is, so a test can hand a
// backend a value; the engine's build switch, which a test walks, keeps the
// two lists equal.
type Placement string

const (
	// Direct runs the harness as a process tree on the engine host, rooted in a
	// per-box directory with HOME and the XDG variables pointed at it and an
	// allowlisted environment.
	//
	// THIS ISOLATES STATE, NOT THE HOST: the agent runs as the engine user and
	// reaches whatever that user can. Right for a workstation or a dedicated VM,
	// wrong for a shared host. A CONFINED backend (confine.go) is the exception,
	// for the Dashboard Assistant's deep work alone (ADR-0054).
	Direct Placement = "direct"

	// Container runs a long-lived Docker or Podman container per box, its home
	// bind-mounted at DefaultHome (a remote box's home, so paths read the same).
	// Real host isolation, at the cost of an image with the harness installed.
	//
	// IT DOES NOT TAKE A SYSTEM PATH: a setup file is written from the host side
	// of the one mount, so one outside the home is refused as in Direct
	// ([hostbox.InHome]), and a setup command runs as the engine's uid on a
	// rootful runtime, which system directories usually refuse. Provision what
	// the image lacks in it.
	Container Placement = "container"

	// E2B runs the work in a remote box: a fresh machine per run that is
	// not the engine's, which is what makes it the right cell for anything
	// running on a shared host.
	E2B Placement = "e2b"
)

// Placements is the closed set.
var Placements = []Placement{Direct, Container, E2B}

// Valid reports whether p names a cell this engine can run. A placement
// arrives on a row a peer of another build wrote, so an unknown one fails
// that run rather than panicking the node reading it.
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

// OwnsMemory reports whether a box in this cell is held to a memory domain of
// its own (a container's cgroup, a remote machine), so a kill the kernel counts
// there for memory is the box's. A direct box shares the engine host's.
func (p Placement) OwnsMemory() bool { return p == Container || p == E2B }

// OwnsKernel reports whether a box in this cell runs on a kernel of its own (a
// remote machine made for the run), so the kernel's per-user budgets, inotify's
// watches and instances among them, are the box's alone.
//
// NO CELL ON THE ENGINE HOST HAS ONE, whatever user its boxes run as: direct
// and rootful container boxes run as the engine user's uid (or `run_args`'s,
// on the host kernel), and a rootless container's user namespace is charged to
// the user that created it, at every level up to the host's. What a box may
// take of such a budget is decided by this, never by the box ([CodingSteps]).
func (p Placement) OwnsKernel() bool { return p.Valid() && !p.OnEngineHost() }
