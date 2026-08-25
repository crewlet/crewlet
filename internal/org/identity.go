// Package org is the organization model: the hierarchy, the seats that hold
// it, and the identity derivation that lets any node name any seat.
//
// The org chart is the execution graph. Knowledge, permissions,
// communication and downward delegation are all scoped by a seat's position
// in the tree, so this package is what the routing, prompt and placement
// layers all read.
//
// # Seat identity is DERIVED, never looked up
//
// An agent seat's runtime id is a UUIDv5 over (org name, handle), so every
// node computes the same id for the same seat with no database and no
// running instance. That is what lets a node route an event to a seat it is
// not itself running — which matters because each engine event topic has one
// fleet-wide consumer group, so the node that wins a delivery is rarely the
// node running the recipient. A pool miss means "not on this node", never
// "does not exist".
package org

import (
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// agentIDNamespace is a fixed UUIDv4, chosen once and frozen.
//
// Changing it re-derives every agent id and orphans every per-seat row
// written before the change — diary, episodes, onboarding markers,
// counterparty profiles. The seat keeps working; it has simply lost its
// memory. Treat as load-bearing.
//
// The value is carried over from the Python engine. The rewrite is a clean
// break with no data migration, so nothing requires this specific
// namespace — but keeping it costs nothing and leaves the door open for
// anyone who does want to carry a company's memory across.
var agentIDNamespace = uuid.MustParse("c1ea9c6e-3f5d-4c9c-9e9f-1d2c3b4a5e6f")

var nonSlugRunes = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify converts a role name to a URL- and email-safe handle.
//
//	"Sarah Chen"    → "sarah-chen"
//	"QA/Test Lead"  → "qa-test-lead"
//
// This is the SINGLE canonical handle derivation. Every layer that needs a
// handle — the engine spawning a seat, a webhook route, the dashboard —
// calls this one function, because a divergent reimplementation produces a
// different derived agent id and silently orphans that seat's memory.
func Slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = nonSlugRunes.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// DeriveAgentID returns the deterministic id for a seat.
//
// The org name is part of the input so handles stay namespaced: two
// companies sharing a store can both have a "ceo" without colliding.
//
// Reports false when either input is empty rather than returning a hash of
// nothing — an unnameable seat must not silently acquire an id that another
// unnameable seat would also derive.
func DeriveAgentID(orgName, handle string) (uuid.UUID, bool) {
	if orgName == "" || handle == "" {
		return uuid.Nil, false
	}
	return uuid.NewSHA1(agentIDNamespace, []byte(orgName+":"+handle)), true
}
