# q — what does an unset `AcquireOptions.Protocol` mean?

Status: **RESOLVED** · Raised by the coordtest port · Decided by the contract owner
· Contract: `go/internal/coord/coord.go`

## The question

`AcquireOptions.Protocol` is an `int` and `coord.go` did not say what `0` meant.
The Python original made this safe by accident: its value was a **keyword
argument defaulting to 1**, so omitting it was harmless. Go's is a **struct
zero**, which inverts the risk — omitting it is now the thing that happens by
accident.

Read as "oldest", a single `AcquireOptions{Owner, TTL}` anywhere in the engine
would hold a live lease below every newer node's protocol floor and stall the
whole fleet's claims — presenting exactly like a rolling upgrade that never
finishes, with nothing in any log naming the cause.

## The decision: the zero value is SAFE, and the two sides differ

- **Write side.** An omitted protocol claims at **this build's**
  `ProtocolVersion`, which is what the caller meant. `AcquireOptions.EffectiveProtocol()`
  is the one implementation; backends must call it rather than reading the field.
- **Read side.** A **stored** record carrying no protocol still reads as **1**,
  the oldest. That record genuinely predates the concept, so the fail-closed
  reading is the honest one: it holds newer nodes back rather than letting them
  claim beside a build whose meaning of ownership they cannot know.
  `coord.StoredProtocol(raw)` is that half.

Both are stated at the field in `coord.go`, and `AcquireOptions` now documents
that its zero value is deliberately usable — every field whose zero would be
dangerous says what its zero means.

## Why this is the general rule, not a special case

"Make the zero value safe" is a Go-native design obligation the Python original
never had to think about, because Python has no struct zeros. Anywhere the port
turns a defaulted keyword argument into a struct field, the same question has to
be asked again — and the answer is not automatic, because the Python default was
chosen in a language where omission was already safe.

The suite pins both halves: `an_omitted_protocol_claims_at_this_build` and
`a_stored_record_with_no_protocol_reads_as_the_oldest`.
