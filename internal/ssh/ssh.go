// Package ssh lets agent seats reach the company's servers over SSH, each
// session authenticated by a short-lived OpenSSH user certificate the engine
// signs with a certificate authority it holds itself.
//
// # One authority, minted and never shown (ADR-0105)
//
// The authority is an Ed25519 key the connect form mints on Save and seals as
// SSH_CERTIFICATE_AUTHORITY (setup.ShapeSSHCertificateAuthority). Only its
// public line is ever rendered; servers trust that line once
// (TrustedUserCAKeys), so adding or removing an agent touches no server. A
// rotation stages a second authority beside it, which the engine never signs
// with until it is promoted.
//
// # Hosts are pinned, never learned
//
// Each server is registered in the company document with its host keys, read
// by the engine's scan and compared by a person against the server console, so
// the engine's client accepts no other key and never trusts a key on first use.
//
// # Two levels, one principal per host and level
//
// A seat holds one level across the hosts it names. Read reaches a host's read
// user, whose every command the server forces through an allowlist. Operate
// adds commands a person approves one at a time, which the engine runs itself
// with a certificate bound to that command. A certificate's principal is
// crewlet.<host id>.<level>, so one issued for one server is refused by
// another.
//
// # No account to make
//
// The integration creates nothing at a vendor: a seat's access is the document
// alone, so the access sync's steps are empty and its observation is the
// document's own setting ([accessVendor]).
package ssh

// Backend is the integration's name: the config block's key, the integration
// kind and the seat access lane.
const Backend = "ssh"
