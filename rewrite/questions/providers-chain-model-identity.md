# q — `Provider.Model()` cannot answer per-call for a shared fallback chain

Raised by: building `internal/providers/llm/chain` against the `Provider`
contract. `llm.go` states the requirement plainly:

> `Model` is a method rather than a field so a chain wrapper can answer for
> whichever member actually served the call — telemetry reads it directly, and a
> chain reporting its own name instead of the model that answered makes the
> per-model token breakdown wrong.

The chain can honour that for a *sequential* caller. It cannot honour it for a
shared one, because the answer is returned by a method that takes no argument
identifying the call it is about.

## Measured

One `*Chain`, 200 goroutines, each calling `Complete` and then reading `Model()`
in the same goroutine, exactly as `toolloop.Run` does:

```go
completion, err := cfg.Provider.Complete(ctx, ...)
if model == "" { model = cfg.Provider.Model() }
```

```
shared chain: 21/200 Model() reads named a model other than the one
              this goroutine's call used
```

Roughly one read in ten. The window is small — between `Complete` returning and
`Model()` being read — but it is a window, and it does not shrink with care on
the reader's side because the reader has no way to say which call it means.

## Why it happens

`Model()` reports state, and the only state a chain can hold is "the model that
answered most recently". With one caller that is the same thing as "the model
that answered *your* call". With N callers it is not, and no implementation of
this signature can make it so:

- a mutable field is a data race (and this build is `-race` gated);
- an `atomic.Pointer[string]`, which is what the package uses, removes the race
  but not the skew — it is still last-writer-wins across goroutines;
- Go has no goroutine-local storage, and `context.Context`, which is the
  mechanism for exactly this, is not a parameter of `Model()`.

## Why it matters

The figure lands in `AgentTurnCompleted`'s model attribution and the dashboard's
per-model token breakdown. A skewed read does not lose the tokens; it files them
under the wrong model — so a fallback chain whose backup is ten times the price
reports a cost split that is wrong in the direction nobody audits, and the
`Model()` value is the only record of which member answered.

It is also silent: both strings are real model ids, so nothing downstream can
tell a skewed attribution from a true one.

## Options (contract owner's call — I have not edited `llm.go`)

1. **Put the serving model on the answer.** Add `Model string` to
   `llm.Completion`. It is one field, it is per-call by construction, and the
   caller already holds the completion at the point it wants the name. A backend
   fills in its own model id; a chain fills in the member's. `Provider.Model()`
   stays for the "what is this provider configured as" question, which is what
   config surfaces and log lines want.

   This is the only option where the skew cannot be produced.

2. **Thread the context.** `Complete` stores the serving member in a value on a
   context the caller prepared. Correct, but it makes the attribution opt-in,
   and a caller that forgets gets the current behaviour with no signal.

3. **Document that a `*Chain` is a per-consumer view** and require the engine to
   build one per concurrent caller. A chain holds pointers to shared backends
   and costs nothing to build, so this is affordable — but it is a rule enforced
   by nothing, and the failure it prevents is invisible.

I would take (1).

## Ruled — option 1, and implemented

The contract owner agreed the defect is the contract's, not the chain's, and
ruled for option 1:

- `llm.Completion` gains `Model string`, filled in by whichever backend served
  the call. That is the field the per-model token breakdown is built from.
- `Provider.Model()` stays, with its meaning narrowed to the provider's
  CONFIGURED identity — what the entry is by default, for logs and config
  display — and its doc now explicitly disclaims the per-call role.

Consequences carried through:

- `chain.Chain.Model()` no longer tracks the last member that answered. The
  atomic is gone; it returns the head member's model, which is stable and
  race-free. A chain reporting its own configured name is harmless now that
  nothing bills against it.
- The chain fills `Completion.Model` in for a member that left it empty, so a
  third-party `Provider` still produces a billable answer.
- Both backends set it to the CONFIGURED model id rather than the one the
  response echoes. A vendor alias resolving to a dated snapshot would otherwise
  re-key the breakdown the day the alias moves, splitting one model's spend
  across two names the config never mentions. Pinned by a test in each backend.
- `toolloop.Run` reads `completion.Model` first and falls back to
  `Provider.Model()` only for a backend that named nothing.
