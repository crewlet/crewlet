package sandbox

// NewContainerBoxAt is a container box over a host directory, for the file
// contract suite: a container box's reads and writes happen on the HOST side
// of its bind mount, so its file contract can be certified without a container
// runtime. Nothing it reaches runs a command.
func NewContainerBoxAt(root string) Sandbox {
	return &containerBox{
		layout: boxLayout{id: "contract", root: root}, runtime: "docker", container: "contract",
	}
}
