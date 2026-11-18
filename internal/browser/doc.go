// Package browser holds what the engine decides about Chromium that more than
// one package reads: the oldest major the engine accepts and the suite
// certifies ([MinChromiumMajor]).
//
// # The floor
//
// [MinChromiumMajor] is the oldest Chromium major the engine accepts — the
// artifact render's DevTools client refuses an older browser, naming both
// versions — and the oldest the suite certifies: scripts/chromium-pin.env pins
// its newest patch beside Chrome for Testing's Stable, and every case that
// drives Chromium runs on both (chromiumtest.Each), unless its doc says why
// the floor adds nothing to its claim.
//
// It is Chrome for Testing's first major published for all four platforms the
// engine is released for: 153, its first build for linux-arm64 (Chrome for
// Testing's per-milestone listing, read 2026-11-08 and again 2026-11-10). A
// floor a supported development platform cannot fetch is a gate that platform
// skips, so no older major can be the floor, however little a consumer needs.
// The render's client needs no newer protocol: every command, event and field
// it declares was described by major 142 (its package doc says how that was
// read), so the platform bound sets the floor.
//
// Raising it is argued here with what was measured, and moves the floor pin
// to the newest patch of the new major in the same change:
// TestThePinsMeetTheFloor fails while the two disagree.
package browser
