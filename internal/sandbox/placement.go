package sandbox

// Placement is WHERE one seat's code work runs: a catalogue entry a seat's
// executor launches on (providers.llm.<key>.run_in), not one company-wide
// mode, since the seat whose harness needs the engine host's own reach and the
// seat whose code must never reach the engine user's files are different calls.
//
// The manager holds a provider PER PLACEMENT, so create, reconnect and kill
// each name one; a run's row persists it, as a box read through the other
// cell's backend reports a running box gone.
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
	// wrong for a shared host.
	Direct Placement = "direct"

	// Container runs a long-lived Docker or Podman container per box, its home
	// bind-mounted at DefaultHome. Real host isolation, at the cost of an
	// image with the harness installed.
	//
	// IT DOES NOT TAKE A SYSTEM PATH: a setup file is written from the host side
	// of the one mount, so one outside the home is refused as in Direct
	// ([hostbox.InHome]), and a setup command runs as the engine's uid on a
	// rootful runtime, which system directories usually refuse. Provision what
	// the image lacks in it.
	Container Placement = "container"
)

// Placements is the closed set.
var Placements = []Placement{Direct, Container}

// Valid reports whether p names a cell this engine can run. A placement
// arrives on a row a peer of another build wrote, so an unknown one fails
// that run rather than panicking the node reading it.
func (p Placement) Valid() bool {
	switch p {
	case Direct, Container:
		return true
	}
	return false
}

// OwnsMemory reports whether a box in this cell is held to a memory domain of
// its own (a container's cgroup), so a kill the kernel counts there for memory
// is the box's. A direct box shares the engine host's.
func (p Placement) OwnsMemory() bool { return p == Container }
