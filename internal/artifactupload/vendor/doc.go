// Package vendor copies the allowlisted CDN files an HTML artifact references
// into that artifact's own version at publish, integrity-checked, behind a
// per-node cache.
//
// Nothing is fetched when somebody views a page, so a view works without the
// CDN and the CDN never learns a viewer's address. Each copy is an ordinary
// object of the one artifact that publishes it: sharing one stored copy across
// artifacts would bring back the race between a writer re-using an object and
// the collector deleting it, for a few kilobytes of saving.
package vendor
