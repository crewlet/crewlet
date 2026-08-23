# q — `BatchOptions`'s zero value turns inbox coalescing off

Raised by: applying a pattern a teammate found in `coord` — *a Python keyword
default that becomes a Go struct zero inverts which mistake is safe* — to
`queue.BatchOptions`. It reproduces here, on the field whose whole purpose is
the coalescing.

## Measured

```
zero struct (a caller omitting the knobs)  -> EffectiveMaxBatch=1,  five queued events arrived as [1 1 1 1 1]
DefaultBatchOptions()                      -> EffectiveMaxBatch=20, five queued events arrived as [5]
```

(Five events published to one conversation while the subscription was held, then
released — the inbox-batching scenario exactly.)

## Why

Python:

```python
max_batch: int = 20     # a caller who omits it gets 20
```

Go:

```go
type BatchOptions struct { mu sync.RWMutex; lingerSeconds float64; maxBatch int }
func (o *BatchOptions) EffectiveMaxBatch() int { if o.maxBatch < 1 { return 1 }; return o.maxBatch }
```

`&queue.BatchOptions{}` compiles anywhere — the fields are unexported, but that
does not stop a composite literal — and yields `maxBatch == 0`, which the clamp
turns into **1**. So in Python omission gave you the safe value and in Go
omission gives you the dangerous one. The clamp is correct in isolation (a batch
size below 1 is meaningless); the problem is that it cannot distinguish "the
caller asked for 0" from "the caller did not ask".

## Why it matters more than a wrong number

`max_batch = 1` is not a smaller batch, it is **no batching**. The property the
subsystem exists for is that events queued while an agent was busy arrive as ONE
turn, not N — so a zero-valued `BatchOptions` silently restores the exact
behaviour inbox batching was built to remove, and it does it by charging N turns
of LLM spend where one was intended.

The symptom is also nearly undiagnosable: an agent handling five separate turns
for five messages looks like a busy agent, not a misconfiguration. Nothing logs,
nothing errors, and the events are all delivered.

## Options (contract owner's call — I have not edited `queue.go`)

1. **Make the zero value mean the default.** Have `EffectiveMaxBatch` treat 0 as
   "unset" and return 20, reserving negatives for the clamp. Cheapest, and it
   restores Python's polarity: omission is safe again. Costs the ability to
   *request* 1, which is `NewBatchOptions(0, 1)` if anyone ever wants it.

   This is also the shape `coord` settled on for the same class of bug
   (`86458a8`): the safe-zero rule became a *function on the contract* —
   `AcquireOptions.EffectiveProtocol` / `StoredProtocol` — that backends must
   call rather than a convention each one implements from a doc sentence.
   `EffectiveMaxBatch` is already that method, so the whole fix here is to make
   it read 0 as "unset" instead of clamping it to 1. That commit also recorded
   the standing rule this note is an instance of: **anywhere the port turns a
   defaulted keyword argument into a struct field, the zero has to be made safe
   on purpose, and the Python default is not evidence — it was chosen in a
   language where omission was already safe.**

   Note the two cases differ in polarity, which is worth knowing when ranking
   this: an unset `Protocol` fails closed and loudly (one such record refuses
   every gated claim fleet-wide). An unset `maxBatch` fails *open* and silently
   — every event is delivered, nothing errors, coalescing just stops — so it is
   far less damaging and considerably harder to notice.
2. **Make the zero value unusable**, so the mistake is loud rather than quiet —
   e.g. backends reject a `BatchOptions` that was never initialised.
3. **Leave it and document it** on the struct, accepting that the safe path is
   `DefaultBatchOptions()` and that a literal is a footgun.

I would take (1): it is the only option where the mistake cannot be made, and it
matches the value the rest of the corpus already calls the default.

Note this is *not* reachable through the in-memory backend's own guard —
`SubscribeBatch` substitutes `DefaultBatchOptions()` for a **nil** options
pointer, which is why no test caught it. A non-nil zero struct sails past that.
Any backend copying the nil-guard has the same hole.
