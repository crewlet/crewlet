package executor

// CursorAgentArchiveFormat is the layout a Cursor run packs its session in.
const CursorAgentArchiveFormat = cursorArchiveFormat

// CursorAgentSupported is the one Cursor Agent build the runner drives, as
// `cursor-agent --version` prints it.
const CursorAgentSupported = cursorSupported

// CursorAgentFixture is a file of testdata/cursor at the measured release.
func CursorAgentFixture(name string) string {
	return "testdata/cursor/" + cursorSupported + "/" + name
}
