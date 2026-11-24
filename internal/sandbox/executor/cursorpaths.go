package executor

// CursorDir is Cursor Agent's own directory in the run's home, which
// CURSOR_CONFIG_DIR names ([CursorAgent.Pins]): its cli-config.json, its MCP
// servers, its chats and its projects' transcripts.
func (p Paths) CursorDir() string { return p.Home + "/.cursor" }

// CursorMCP is the home's own MCP servers, which a run lays its servers into
// beside the home's and takes them out of once its harness exits.
func (p Paths) CursorMCP() string { return p.CursorDir() + "/mcp.json" }

// CursorRun is the run's own directory: its helper and the files of one run.
func (p Paths) CursorRun() string { return p.WorkDir() + "/cursor" }

// CursorHelper is cursor_helper.mjs, which a run's script runs on the node
// Cursor's package ships ([CursorAgent.WriteConfig]).
func (p Paths) CursorHelper() string { return p.CursorRun() + "/helper.mjs" }

// CursorPID is the file the harness's process writes its pid to before it
// starts, which the turn cap's follower signals.
func (p Paths) CursorPID() string { return p.CursorRun() + "/pid" }

// CursorCapped is the mark the turn cap's follower leaves once it stopped
// the run at its cap.
func (p Paths) CursorCapped() string { return p.CursorRun() + "/capped" }

// CursorHistory is the document a continued run hands Cursor
// (--conversation-history-file), written from [Paths.CursorSession].
func (p Paths) CursorHistory() string { return p.CursorRun() + "/history.json" }

// CursorMCPAdded records the servers a run laid into [Paths.CursorMCP], read
// by the run's own removal and by the next run's, should this one be cut
// off. Apart from [Paths.CursorRun], which a clear removes whole.
func (p Paths) CursorMCPAdded() string { return p.WorkDir() + "/cursor-mcp-added.json" }

// CursorSessions is where a box keeps its own copy of each session's
// conversation, the messages a continued run is handed.
func (p Paths) CursorSessions() string { return p.WorkDir() + "/cursor-sessions" }

// CursorSession is session's conversation as the box keeps it: one message of
// Cursor's ConversationHistory a line, the archive's one member.
func (p Paths) CursorSession(session string) string {
	return p.CursorSessions() + "/" + session + ".jsonl"
}
