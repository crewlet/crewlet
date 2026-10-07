package sandbox

// NewContainerBoxAt is a container box whose directory is root, for the file
// contract suite: a container box's reads and writes happen on the HOST side
// of its bind mount, so its file contract can be certified without a container
// runtime. Nothing it reaches runs a command. It answers the box and the host
// directory its home is — the one the mount would carry, which is not root
// itself, since root also holds the engine's records about the box.
func NewContainerBoxAt(root string) (Sandbox, string) {
	layout := boxLayout{id: "contract", root: root}
	return &containerBox{layout: layout, runtime: "docker", container: "contract"}, layout.home()
}
