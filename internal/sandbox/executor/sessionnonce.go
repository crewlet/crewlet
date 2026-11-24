package executor

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// sessionNonce is the suffix a runner that names its MCP servers apart
// (Grok Build, Muse Code) adds to each server's name in session's runs: six hex
// digits of a digest of the session's id. A session id is a UUID the runner
// mints at its launch ([Harness.Session]), so no repository or image file can
// name a server of the session beforehand; and every run that continues the
// session names them alike, since the session's record hands its model each
// earlier call under the name it was made by, and a call to a name the run no
// longer has is refused (measured on Grok Build 1.0.92 and Muse Code 1.4.25).
// A run's suffix is never drawn anew: "" is refused by its runner.
func sessionNonce(session string) string {
	sum := sha256.Sum256([]byte("crewlet mcp server names\x00" + session))
	return hex.EncodeToString(sum[:sessionNonceLen/2])
}

// sessionNonceLen is how many hex digits [sessionNonce] makes.
const sessionNonceLen = 6

// freshSessionWiring is how a continuing run reaches its seat's tools where
// its script starts a session of its own instead, for a runner whose server
// names are the session's own ([sessionNonce]): the request's FreshWiring, or,
// where it hands none (no bridge token to tell the sessions apart), the
// request's own wiring, which that session still names under its own suffix.
// Nil for a run that continues nothing, or has no server to name.
func freshSessionWiring(req sandbox.RunRequest, session Session) *sandbox.Wiring {
	switch {
	case !session.Continues:
		return nil
	case req.FreshWiring != nil:
		return req.FreshWiring
	case len(req.MCPServers) == 0:
		return nil
	}
	return &sandbox.Wiring{MCPServers: req.MCPServers, Steer: req.Steer, SkillLoads: req.SkillLoads}
}
