package mcp

import (
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Identity is what a server said of itself in the handshake: its
// implementation's name, title and version, the protocol revision the session
// speaks, and the instructions it offers a client for using it.
//
// WORDS A THIRD PARTY WROTE, every field of it, kept as they arrived. Whoever
// carries them further bounds them (internal/coord's listing record) and
// whoever shows them to a person or a model says whose words they are.
type Identity struct {
	Name         string
	Title        string
	Version      string
	Protocol     string
	Instructions string
}

// identityOf reads the handshake's answer. A server that declines to identify
// itself leaves the implementation's fields empty, which is a valid session.
func identityOf(init *sdk.InitializeResult) Identity {
	id := Identity{Protocol: init.ProtocolVersion, Instructions: init.Instructions}
	if info := init.ServerInfo; info != nil {
		id.Name, id.Title, id.Version = info.Name, info.Title, info.Version
	}
	return id
}

// Identity is what one INSTANCE's server said of itself in its most recent
// handshake, and false for an instance the bridge does not supervise or one
// that never completed a handshake (a start the bridge is still retrying).
//
// THE LAST SESSION'S, live or dead, as its listing is: a server that is down
// keeps the tools its last session listed ([Bridge.ServerTools]), and the
// identity read beside them is the one that session gave.
func (b *Bridge) Identity(name string) (Identity, bool) {
	b.mu.RLock()
	s, ok := b.servers[name]
	b.mu.RUnlock()
	if !ok {
		return Identity{}, false
	}
	c := s.lastClient()
	if c == nil {
		return Identity{}, false
	}
	return c.identity, true
}
