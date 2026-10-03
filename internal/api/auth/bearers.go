package auth

// A PRESENTED BEARER IS COMPARED AS IT ARRIVES, AND ITS VALUE IS WHAT PROTECTS
// IT.
//
// Every guarded route compares the bearer it is handed against the Tier A
// entries and the identity directory's machine tokens, and whether the
// comparison matched is the whole of what the caller learns: 401, or the
// route's own answer. That is an oracle at line rate, and it is one on EVERY
// guarded route — a read of /agents answers a guessed value exactly as
// `POST /auth/token` does. What makes guessing through it hopeless is the
// value, never a curve in front of it:
//
//   - a machine token carries `credential.TokenBytes` of crypto/rand;
//   - a session cookie is an HMAC-SHA256 under the fleet's keyring;
//   - a Tier A value is at least `secrets.MinSharedTokenChars` characters,
//     refused shorter by config, and `crewlet secrets keygen` mints one.
//
// # Why no curve stands in front of the comparison
//
// A curve here can only be keyed on the SOURCE — a bearer names nobody until
// it is compared — and a source-keyed refusal is one anybody sharing the
// address holds shut for everybody else at it: an office behind one NAT, a
// VPN's egress, and the whole internet on a deployment whose proxy is not in
// `api.trusted_proxies`. One was tried: ten refused bearers free, then a
// doubling wait to thirty seconds, admitted BEFORE the comparison — which is
// the only place a curve can stand, since one consulted afterwards lets a
// correct guess through whatever it says. So one refused bearer from a stranger
// every twenty-five seconds kept every pipeline, every machine token and the
// break-glass Tier A token at that address answering 429 on every guarded
// route of every node the load balancer reached; and because an attempt still
// being compared counted as a refusal until it resolved, a client with a
// dozen VALID requests in flight met the same 429 with nobody guessing at all.
// The curve bought nothing against a value of that length and cost the way
// back in.
//
// And not on `POST /auth/token` alone, which is where the identity design
// first put it: every other guarded route answers the same guess the same way,
// so a curve on the exchange alone slows nobody who is guessing and still lets
// a stranger at the address close the one route a break-glass holder uses to
// reach the dashboard.
//
// WHAT A GUESS STILL COSTS THE GUESSER is visibility: every refused bearer on a
// guarded route is a failed attempt in the audit trail's per-minute count
// (audit.go), so a spray is a row an operator reads, naming the source and how
// many attempts it failed.
