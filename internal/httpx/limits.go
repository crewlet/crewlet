package httpx

// MaxResponseBody bounds a decoded third-party app response.
//
// Every third-party app client caps the body it DRAINS (1 MiB) and left the
// SUCCESS decode unbounded, an asymmetry inside one function, where the
// unbounded arm is the one that runs on every successful call.
//
// The error body's cap is [RefusalBytes] and is now shared too. This sentence
// used to claim it already was — "every third-party app client caps the ERROR
// body it reads (2 KiB)" — and it was false when written: four of the seven
// used 2 KiB, one used 4 KiB, one a constant of its own, and Atlassian read a
// MEGABYTE, which is how a rendered HTML error page reached an operator's log
// in full.
//
// This is hardening, not a defect: these are outbound calls to
// operator-configured, authenticated endpoints, and json.Decoder already
// streams rather than buffering the whole body. What the cap refuses is a
// compromised or malfunctioning endpoint choosing this process's memory
// ceiling.
//
// 32 MiB, derived from the largest page any client asks for: 200 items at
// GitHub's and GitLab's maximum page size, whose largest item (an issue with
// full body text and every field expanded) runs to tens of kilobytes — call it
// 64 KiB, so ~13 MiB — with headroom for an endpoint that ignores a page-size
// request. Anything past that is not a page this engine asked for.
const MaxResponseBody = 32 << 20
