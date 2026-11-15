// Package jsonscan reads one JSON text from an io.Reader a token at a time, and never holds a string, a
// name, a number or a run of blanks whole.
//
// # Why it exists
//
// A coding CLI's session holds JSON no reader of it may hold whole: Claude Code keeps no bound on a
// transcript line (a whole-PDF Read writes its base64 twice on one line, 56 MB at the CLI's 20 MiB file
// limit; a prompt or an api-request-blob has no bound at all) and OpenCode keeps a message part of 27 MiB.
// encoding/json and encoding/json/jsontext both hand a string over as one token, held whole in memory before
// the caller sees its first byte, so a reader on either is held to some bound of its own, past which it
// refuses the session or reads nothing. This scanner is the one reader the session's readers move onto (the
// take's scrub, the Claude Code thread layer, the context meter, a person's transcript): one grammar, so a
// line one of them reads as JSON is a line every one of them reads as JSON.
//
// # The contract
//
// [New] reads one JSON text: exactly one value, with blanks (space, tab, line feed, carriage return)
// around it. [Scanner.Next] answers each token's [Kind] in order, and io.EOF once the value and the blanks
// after it are read, which is when the text is known to be one value. A [Name]'s or a [String]'s decoded
// text, or a [Number]'s literal as written, is read with [Scanner.Read] like any io.Reader, a piece at a
// time, never ending a read inside a UTF-8 sequence; what is not read, the next Next passes over.
// [Scanner.Skip] passes over a whole value and [Scanner.Capture] answers a value's bytes as written up to a
// bound its caller names, passing over a longer one: a reader that hands a short value out raw holds it, at
// that bound, and passes media over unheld. [Scanner.Depth] and [Scanner.Offset] say where a token sits, for
// a reader that models a byte-level load by where a key sits in its line.
//
// EVERY BYTE IS VALIDATED, the ones a reader passes over too: a reader moved onto the scanner keeps the
// acceptance it had, and a reader that decides from a line (the thread layer, which reads a line that is no
// JSON value as Claude Code's own load reads it) needs to know the line was one value before it decides.
// A refusal is an [*Error] with the [Fault] and the Offset it was met at, five faults a reader maps onto its
// own; an error of the reader beneath passes through, wrapped. Either ends the scan.
//
// # One grammar, both decoders'
//
// RFC 8259 JSON, as both decoders this tree reads sessions with accept it: encoding/json (json.Valid, and a
// Decoder with UseNumber decoding into any) and encoding/json/jsontext under the thread layer's options
// (AllowDuplicateNames and AllowInvalidUTF8 on, encoding/json/v2's MatchCaseInsensitiveNames off). Under them
// a name may be written twice, a string's invalid UTF-8 reads as U+FFFD one byte at a time (utf8.DecodeRune's
// reading) and a surrogate's \u escape that makes no pair reads as U+FFFD, the escape after it read on its
// own; a value [MaxDepth] deep is read and one deeper refused; and a NUL or a byte order mark before the
// value is a refused byte like any other.
//
// THE TWO DO NOT DISAGREE, in acceptance or in a decoded text, and this package's differential fuzzing holds
// the scanner to each of them and the two to each other, so the day a release makes them disagree its test
// fails naming the input. In the toolchain this module pins they cannot disagree by construction, IN ITS
// DEFAULT BUILD, where the jsonv2 experiment is on: since Go 1.27, where encoding/json/v2 and jsontext joined
// the standard library, encoding/json is built on jsontext, json.Valid is jsontext's own read of one value
// under AllowDuplicateNames and AllowInvalidUTF8, and every string either decodes is unquoted by the one
// function both share. A build with GOEXPERIMENT=nojsonv2 compiles encoding/json's own decoder instead (its
// files carry `//go:build !goexperiment.jsonv2`), and the agreement then rests on the fuzzing alone.
//
// WHERE THEY EVER DO, THE SCANNER FOLLOWS JSONTEXT: the thread layer's acceptance must not change by a single
// line, and a tool result's text it digests is compared with a log's output, so a decoded text that moved
// would reach that comparison. A reader that needs encoding/json's side then gets an option for it, used
// only there. How a reader folds a name written twice into one value is the reader's, not the grammar's:
// the scanner hands every member over, in order.
//
// # Memory and speed
//
// Whatever the input, a Scanner holds its 64 KiB read buffer and a 1,536-byte struct, the open containers
// at one bit each (1,250 bytes at MaxDepth) among it: 67,072 bytes, allocated once by New and kept by
// [Scanner.Reset]. A text's escape or character in progress sits in the read buffer, and the decoded text
// goes straight into the caller's p. A Capture whose value outlasts one fill of the read buffer holds it in
// a buffer of its own, at most the bound it names, allocating at most twice that on its way there, and THE
// SCANNER KEEPS THAT BUFFER for its life, [Scanner.Reset] included, for the next Capture to reuse: a reader
// that captures at its bound line after line allocates it once, and one that has captured at a bound of B
// holds up to B bytes more than the figure above from then on. A scan allocates nothing else: its test
// reads a 64 MiB string, one of escapes and characters past ASCII, a 64 MiB name, a 64 MiB number, 64 MiB
// of blanks and a value nested MaxDepth deep, and passes a 64 MiB string over, each within 68 KiB allocated
// from New to io.EOF, and captures one past a 1 MiB bound within that and twice the bound.
//
// A long string is copied out in bulk: a run of plain bytes is found eight bytes at a time and copied in one
// step, and passed over the same way, never stepped through a byte at a time. On two efficiency cores of a
// laptop its benchmarks read a 16 MiB base64 string at about 1.4 GB/s and pass it over at about 2.9 GB/s,
// where jsontext reads it at about 0.8 GB/s holding it whole, and read a transcript line dense with escapes
// at about 0.5 GB/s: the scrub's two readings of a 256 MiB session cost about a second of scanning, against
// its two-minute budget.
package jsonscan
