// Package appstoreconnect is the App Store Connect integration: every
// operation of Apple's App Store Connect API, reached by the engine for the
// agents through the company's team keys, one per Apple role (ADR-0170).
//
// # Keys and tokens
//
// A team key is pasted once per role and sealed; the engine signs a token per
// request ([Client]), ES256, living ten minutes from a minute back, scoped to
// that one request, so a token that reaches a log repeats only the call it was
// made for. Each call goes through the least-privileged connected key whose
// role may make it, and a refused key is never retried with another.
//
// # Rate budget
//
// Apple limits each key per rolling hour and says what is left on every
// answer. Agents' reads stop at a tenth of a key's limit, which is kept for
// approved acts and the pass. A read waits out a 429 within httpx's cap; a
// write is never sent twice.
package appstoreconnect
