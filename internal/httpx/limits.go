package httpx

// MaxResponseBody bounds a decoded third-party app response.
//
// Every third-party app client caps the body it DRAINS (see [DrainBytes]) and
// left the SUCCESS decode unbounded, an asymmetry inside one function, where
// the unbounded arm is the one that runs on every successful call.
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
//
// A CAP IS NOT A CUT. A client that hands this to an io.LimitReader and then
// decodes reads a body one byte short of the truth as an ordinary syntax
// error — "unexpected end of JSON input", naming nothing. The clients that
// BUFFER therefore read one byte past it and REFUSE, naming the endpoint and
// the ceiling, in the idiom the whole tree uses: a value with a limit is
// refused naming the field rather than silently cut to fit.
const MaxResponseBody = 32 << 20

// DrainBytes is how much of an ignored response body is read before the
// connection goes back to the pool.
//
// A MEBIBYTE, which is what all nine sites that do this already spelled
// inline. It is not a correctness bound and nothing decodes what it reads:
// net/http returns a connection to the idle pool only when its body has been
// read to EOF, so a client that discards an answer still has to drain one —
// and a client that drains without a cap has handed a malfunctioning endpoint
// the same lever [MaxResponseBody] exists to take away.
//
// Named rather than repeated because the three caps in this file are one
// decision each, and the tree had already proved what happens to an unnamed
// one: see [RefusalBytes], which replaced six spellings of a number whose doc
// comment in each of the six asserted it matched the others.
const DrainBytes = 1 << 20
