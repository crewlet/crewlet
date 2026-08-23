# q — `Request.Temperature` and `Request.MaxTokens` have no "unset"

Raised by: implementing the Anthropic and OpenAI backends against
`llm.Request`. Both fields are plain numbers whose zero value is also a value a
caller might legitimately mean:

```go
type Request struct {
	Messages    []Message
	Tools       []ToolDef
	Temperature float64
	MaxTokens   int
	ToolChoice  string
}
```

## Observed

The only consumer in the tree never sets either. `toolloop.Run` builds every
provider call as:

```go
completion, err := cfg.Provider.Complete(ctx, llm.Request{
	Messages:   msgs,
	Tools:      tools,
	ToolChoice: choice,
})
```

So `Temperature` and `MaxTokens` arrive as `0` on **every** call the engine
makes — Plan, Execute, Review, the extension judge, a sub-agent. Whatever the
zero is decided to mean, it is what the whole engine runs on.

## The two readings

**Zero means zero.** Every phase runs at `temperature: 0` and asks for a token
cap of nothing. The cap reading is not tenable — a `max_tokens: 0` is required
by Anthropic and means "populate the cache and generate nothing" — so `0` must
already mean "unset" for `MaxTokens`, and it would be strange for the field
beside it to mean the opposite.

**Zero means unset.** The backend applies its configured default. That is what
the Python engine did (`temperature: float = 0.7` in both provider signatures,
`max_tokens: int | None = None`), and it makes the two fields consistent.

The cost is that a caller who genuinely wants `temperature: 0` — a judge or a
classifier that must be reproducible — cannot ask for it through this struct.
Negative temperatures are meaningless, so the whole range below zero is spare,
but "-1 means 0" is worse than either reading.

## What the backends do meanwhile

Both take the second reading, and both expose a `Temperature` on their own
config so an operator can set it per provider entry:

```go
temperature := p.temperature       // config, defaulting to 0.7
if req.Temperature > 0 {           // a request that named one wins
	temperature = req.Temperature
}
```

`MaxTokens` is the same shape. Anthropic must send one, so it falls back to the
provider's configured cap (4096 by default, as in Python); OpenAI sends none at
all when nothing asked for one, which is what the endpoint and the
openai-compatible gateways expect.

## Options (contract owner's call — I have not edited `llm.go`)

1. **Say so on the contract.** Document that a zero in either field means "the
   backend's configured default" and that a caller wanting deterministic output
   configures the provider entry rather than the request. Costs nothing, keeps
   the plain-number fields, and matches what both backends already do.
2. **`*float64` / `*int`.** Unambiguous, and `nil` reads as unset at a glance.
   Costs a pointer at every call site that ever sets one — currently none.
3. **A sentinel** (`TemperatureUnset = math.NaN()`, say). Rejected here: it puts
   a value in the struct that is not a temperature, and NaN compares false
   against itself, which is exactly the kind of surprise a contract should not
   ship.

I would take (1) unless a phase genuinely needs `temperature: 0`.

## Ruled — option 2 for temperature, option 1 for the cap, and implemented

The contract owner ruled that this is the exact class `decisions/000` names — a
Python keyword default becoming a Go struct zero inverts which mistake is safe
— and that the two fields should differ, because they differ honestly:

- `Temperature *float64`. Nil means "leave it to the provider". 0.0 is a
  legitimate value meaning deterministic, so a plain float cannot express
  unset.
- `MaxTokens int` stays a plain int with 0 meaning unset, because a request for
  zero tokens is not a thing anyone means. The asymmetry is documented at the
  fields rather than smoothed away — uniformity here would be the worse
  contract.

Implemented with two helpers on the contract, because the safe reading of a
zero has to be something backends CALL rather than a sentence they each
re-derive (the same move the coordination contract made with
`AcquireOptions.EffectiveProtocol`):

- `Request.TemperatureOr(fallback)` — the only way either backend reads the
  field. The obvious hand-written test, `if req.Temperature != nil &&
  *req.Temperature > 0`, quietly restores the bug.
- `llm.Temp(v)` — Go cannot take the address of a constant, and
  `t := 0.0; req.Temperature = &t` at every call site is how a caller ends up
  sharing one variable between two requests.

Both directions are pinned on both vendors, against the request body the fake
endpoint received: a nil temperature sends the provider's configured default
and NOT 0.0, and an explicit `llm.Temp(0)` sends 0.0 and not the default. They
are different bugs and only one of them shows up in a happy-path test.
