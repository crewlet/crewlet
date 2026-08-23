# q — the Review prompt's budget is 600 tokens, not 450

Status: **RESOLVED — 600** · Decided by the contract owner · The plan was wrong

The escalation was correct and the plan's 450 is a transcription error, not a
tightening anyone intended. `tests/test_agent/test_prompts.py:979` asserts
`approx_tokens < 600`, and `git log -S` shows the value went 560 → 600 and was
never 450. `REVIEW_HEADER` alone measures ~555, so 450 is below the floor of a
constant the same build item requires be carried VERBATIM — the only way to
reach it is to delete decision rules, which is the one thing that item forbids.

Shipped 600 with its provenance at the test, and the plan is corrected so the
next reader does not re-derive the same contradiction.

Worth noting how it was caught: the brief carried the wrong number and the
instruction "port the prose verbatim", and those two are only contradictory if
you measure. An executor that trusted the brief would have trimmed the prompt
to fit and reported success.


Status: **OPEN — shipped at 600, the spec's value** · Raised by the
`agent/prompts` port · Spec: `src/crewlet/agent/prompts.py` +
`tests/test_agent/test_prompts.py` · Port: `go/internal/agent/prompts/`

## What happened

The port brief named three budgets: Plan < 2400, Execute < 300, **Review <
450**. The first two are the spec's values. The third is not: the Python suite
asserts **< 600**, and 450 has never been its value.

I shipped 600 and am raising the difference rather than quietly meeting either
number, because meeting 450 is not a matter of tightening assembly — it
requires deleting prompt text that the brief also forbids touching.

## Measured

`approxTokens` is the Python suite's own measure, carried unchanged: Unicode
code points ÷ 4 (see below). Against the reference lead seat with no
summaries — the exact fixture the Python budget test uses:

| prompt  | measured | spec budget | headroom |
|---------|---------:|------------:|---------:|
| Plan (100-tool catalogue) | ~2286 | 2400 | 114 |
| Execute | ~204 | 300 | 96 |
| Review  | **~587** | **600** | **13** |

`REVIEW_HEADER` **on its own** is 2223 characters — ~555 tokens. The identity
line and the two always-rendered evidence headings account for the remaining
~32. A 450 budget is therefore unreachable while the header is carried
verbatim: it is under the floor of the constant, never mind the assembly
around it.

The 13 tokens of headroom are not an accident of the port. The Python test
says so in as many words: *"Headroom is ~13 tokens on purpose: the next
addition should have to justify itself here, not slip in."* The Go port
reproducing 587 against a stated 13 is independent evidence that the prose
crossed byte-for-byte.

## Where 600 comes from

`git log -S` over the Python suite gives the whole history — two values, never
450:

- **560** at the initial commit: identity + the decision enum + the
  tool-delivery, missing-tool and blocked/needs-a-colleague rules.
- **600** in `9ed75e6 fix(seat): four ways a healthy node stopped doing its
  job`, when the duplicate-delivery rule had to be re-keyed on *target and
  content* rather than on tool name. Keyed on the name alone it fired on the
  in-thread follow-up that `PRIOR_WORK_HEADER` explicitly asks for, so every
  corrected turn looped to `max_iterations` and terminated `failed`.

The docstring also records the budget's own origin: it started at 300
(identity + a six-line enum) and the rules added ~290 tokens, each mapping to
an observed turn-ending bug — silent half-finished turns when Plan did not
list the right tools; the sandbox rule, which stops Review looping forever by
misreading a `run_sandbox`-delegated investigation as fabrication; and the
cross-iteration duplicate rule above.

## Why not just meet 450

Three ways to get there, all bad:

1. **Reword or cut rules.** ~550 characters have to go — roughly two of the
   five rules. Each one is a named production failure, and the brief's own
   standing instruction is that the English is load-bearing and carried
   verbatim.
2. **Change the measure.** A real tokenizer gives a different number for the
   same bytes, which would "meet" 450 by measuring something else. The brief
   forbids exactly this, and it is right to: a budget nobody can trace to the
   failure that set it is not a budget.
3. **Assert 450 and skip the test.** Not on the table.

## The question

Is 600 confirmed as the carried budget, or is a Review-prompt reduction
actually wanted?

If a reduction is wanted, the decision has to name **which rule is deleted**
and accept the failure it re-opens — the rules are not padding, and there is
no third source of the ~290 tokens they cost. My recommendation is to keep
600: the tightest headroom in the whole prompt surface is already doing the
job the brief wants a budget to do, and the value is traceable to two commits
and five incidents.

## The measure, for the record

The Python suite approximates tokens as `len(prompt) // 4` and says so:
*"We approximate tokens as chars/4 (conservative) so the test is stable across
tokenisers."* The Go port carries the same approximation, with one detail that
is easy to get wrong and would silently re-tighten every budget above:

Python's `len()` counts **code points**; Go's `len()` counts **bytes**. These
prompts carry em dashes and arrows at 3 bytes each, so a byte count measures a
different prompt than the budgets were set against — Review reads 594 instead
of 587, spending more than half of the 13 tokens of headroom that were left
deliberately, and Plan reads 2290 instead of 2286. Neither crosses today; both
would answer a later "does this addition fit?" wrongly, which is the only
question a budget exists to answer. `approxTokens` uses
`utf8.RuneCountInString`, and a test pins the distinction (four em dashes must
measure 1 token, not 3) so the measure cannot be "simplified" back into a
different contract.
