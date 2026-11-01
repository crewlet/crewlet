// Package vendor fetches the allowlisted CDN files an HTML artifact references,
// so the publish pipeline can copy each into that artifact's own version,
// behind a per-node cache keyed by URL.
//
// Nothing is fetched when somebody views a page, so a view works without the
// CDN and the CDN never learns a viewer's address. Each copy becomes an
// ordinary object of the one artifact that publishes it: sharing one stored
// copy across artifacts would bring back the race between a writer re-using an
// object and the collector deleting it, for a few kilobytes of saving. What is
// shared is this node's cache of the BYTES, which is never an object anybody
// names.
//
// THE ONE REQUEST THE ENGINE MAKES TO A URL A PAGE WROTE, so it is held tight:
// https only, a host on the linter's allowlist, a redirect only to the same
// host, a body of at most [MaxFileBytes] within [FetchTimeout], a type the page
// can load the reference as, the page's subresource integrity value checked
// against the bytes, and a dialer that refuses every address that is not
// public ([httpx.Guarded], the one guard for a URL a remote party names,
// ADR-0072). A file whose URL names one release is reused until the
// cache evicts it; one that names none (a web font's stylesheet) is fetched
// again past [MutableTerm].
package vendor
