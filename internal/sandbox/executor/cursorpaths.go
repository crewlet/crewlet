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

// CursorNoteHelper is the note helper a run writes into a box its machine has
// a build for, which hands the run its checkouts ([CursorAgent.checkoutHome]).
func (p Paths) CursorNoteHelper() string { return p.CursorRun() + "/notehelper" }

// CursorCheckouts is the run's checkout state: what checkout-launch and the
// postToolUse hook's firings handed, under its lock.
func (p Paths) CursorCheckouts() string { return p.CursorRun() + "/checkouts" }

// CursorCheckoutLaunch is the helper's checkout-launch config.
func (p Paths) CursorCheckoutLaunch() string { return p.CursorRun() + "/checkout-launch.json" }

// CursorCheckoutSections is the launch checkouts' sections, which the script
// writes into the run's rule of them ([cursorCheckoutRule]).
func (p Paths) CursorCheckoutSections() string { return p.CursorRun() + "/checkout-sections.md" }

// CursorCheckoutSkills is where a firing copies a clone's skills, each listed
// by its copy's path, and CursorCheckoutCopied the list of copies.
func (p Paths) CursorCheckoutSkills() string { return p.CursorRun() + "/checkout-skills" }

// CursorCheckoutCopied lists, a name a line, the skills a firing copied into
// [Paths.CursorCheckoutSkills].
func (p Paths) CursorCheckoutCopied() string { return p.CursorRun() + "/checkout-copied" }

// CursorHooks is the home's own hooks, which a run lays its postToolUse hook
// into beside the home's entries ([cursorHooked]).
func (p Paths) CursorHooks() string { return p.CursorDir() + "/hooks.json" }

// CursorHeldHooks is the home's own hooks.json held whole while a run's stands
// in its place ([holdHome]): apart from [Paths.CursorRun], which a clear
// removes whole, so a cut-off run's is put back whatever order the clear takes.
func (p Paths) CursorHeldHooks() string { return p.WorkDir() + "/cursor-hooks.held" }

// CursorHooksOff is why a run lays no postToolUse hook, a line its script adds
// to the skip log once.
func (p Paths) CursorHooksOff() string { return p.CursorRun() + "/hooks-off" }
