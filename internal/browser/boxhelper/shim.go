package boxhelper

import "path"

// ShimArgs is how a harness starts the box's browser as its stdio MCP server,
// the shim, for the job launch in the box whose home is home: node running
// the helper's mcp, naming the job and its workspace, root, which the server
// takes as its working folder: a relative file name the model gives lands
// there, an absolute one is taken as given, and either is admitted only there
// or in the server's output directory. It carries no token, so a fresh
// session's configuration holds the same entry.
func ShimArgs(home, launch, root string) (command string, args []string) {
	return "node", []string{path.Join(Folder(home), "helper.mjs"), "mcp", "--job", launch, "--root", root}
}
