package auth

// A PRESENTED BEARER IS COMPARED AS IT ARRIVES, AND ITS VALUE IS WHAT PROTECTS
// IT.
//
// Every guarded route's match-or-401 is a guessing oracle at line rate, so
// the value protects the bearer, never a curve in front of the comparison:
//
//   - a machine token carries `credential.TokenBytes` of crypto/rand;
//   - a session cookie is an HMAC-SHA256 under the fleet's keyring;
//   - a Tier A value is at least `secrets.MinSharedTokenChars` characters.

// A curve could only be keyed on the SOURCE (a bearer names nobody until it is
// compared), and anybody sharing the address (NAT, VPN egress, or everyone when
// the proxy is not in `api.trusted_proxies`) could hold it shut for the rest:
// one refused bearer from a stranger kept every valid token there at 429, and
// counting an in-flight comparison as a refusal 429'd valid clients too. It
// also cannot sit on `POST /auth/token` alone, as every route answers a guess.

// A guess costs the guesser visibility: each refusal is a failed attempt in the
// audit trail's per-minute count (audit.go).
