package artifacts

// FileSource is how a format reads a version's files, wherever they are: the
// files a publish brings and carries, or a stored version's objects. A format
// package (slideformat, docindex) reads through it alone, so one parser decides
// a format on publish and on read.
type FileSource interface {
	// Paths are every file of the version.
	Paths() []string

	// Essence is the media type essence of the file at path, false where
	// the version holds none.
	Essence(path string) (string, bool)

	// Read is the bytes of the file at path.
	Read(path string) ([]byte, error)
}
