package engine

import (
	"errors"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// BoxURL is one engine address a box dials back to the engine, as this node
// reads it from its environment.
type BoxURL struct {
	// Variable is the environment variable that holds it.
	Variable string
	// URL is the value as [sandbox.ParseBoxURL] reads it: "" when the
	// variable is unset, or set and refused.
	URL string
	// Refused is the grammar's refusal of a value that is set and unusable,
	// naming the variable and never the value. Nil otherwise.
	Refused error
}

// BoxURLs are the engine addresses a box dials back to the engine.
type BoxURLs struct {
	// Receiver is the in-box telemetry receiver's base
	// ([sandbox.OtelReceiverURLVar]).
	Receiver BoxURL
	// Bridge is the tool bridge's base, through which a box's executor reaches
	// its seat's tools ([mcpbridge.BaseURLVar]).
	Bridge BoxURL
	// Git is the code host's base a coding box clones and pushes through
	// ([sandbox.BoxGitURLVar]): a git node's public listener and /box/git,
	// or a balancer over every git node's — any git node admits a box, by
	// its run's row (ADR-0086). Unset, no run is given a repository.
	Git BoxURL
}

// ReadBoxURLs reads every engine address a box dials back to the engine from
// getenv, each through [sandbox.ParseBoxURL].
//
// ONE READING FOR TWO READERS: [New], which refuses its boot on any refusal
// here ([BoxURLs.Refusal]) before anything is opened, and `crewlet llm
// doctor`, which a deploy script gates on before a node takes seats and which
// reports every one of these the boot would refuse — whatever mode the
// company's providers run in, since a node that refuses to boot runs no seat
// at all. Listed once here, a URL a later change gives boxes is one neither
// reader can leave out.
func ReadBoxURLs(getenv func(string) string) BoxURLs {
	read := func(variable string) BoxURL {
		url, err := sandbox.ParseBoxURL(variable, getenv(variable))
		return BoxURL{Variable: variable, URL: url, Refused: err}
	}
	return BoxURLs{
		Receiver: read(sandbox.OtelReceiverURLVar),
		Bridge:   read(mcpbridge.BaseURLVar),
		Git:      read(sandbox.BoxGitURLVar),
	}
}

// All is every box URL, in the order [New] reads them.
func (u BoxURLs) All() []BoxURL { return []BoxURL{u.Receiver, u.Bridge, u.Git} }

// Refusal joins every refusal among them, in [BoxURLs.All]'s order; nil when
// the boot takes every one.
func (u BoxURLs) Refusal() error {
	var refused []error
	for _, url := range u.All() {
		if url.Refused != nil {
			refused = append(refused, url.Refused)
		}
	}
	return errors.Join(refused...)
}
