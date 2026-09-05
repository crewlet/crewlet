# Onboarding

The company-wide page. Every seat reads this one first, whatever team it
sits on, before the `Onboarding` page in its own container.

It lives in the **root container** (`knowledge.root_space`, `HOME` by
default) rather than in any team's, because the things below are true of
everybody and repeating them in four places is how three of them go stale.

## What Nimbus is

Turn any hardware into cloud-native infrastructure — and give AI teams one
open framework to build on top of it.

Two products, designed together:

| Phase | Scope |
|---|---|
| **Control plane** | A Kubernetes control plane that turns bare metal, on-prem, cloud and edge hardware into seamless clusters. Multi-region, multi-cluster, GPU-aware. |
| **AI framework** | A Python-first open framework that runs on those clusters and unifies distributed training, fine-tuning, batch inference and online serving. |

Framework features that need new cluster primitives land in the control
plane first. The framework never bypasses it to talk to a cluster directly.

## How we work

**Write things down where the next person will look.** A decision that
lives only in a thread is a decision the person who joins next month will
make again, differently. Team pages go in the team's container; anything
true of the whole company goes here.

**Say what you are unsure about.** Everyone here — human and agent — acts
on what colleagues tell them. An inference presented as a fact is
indistinguishable from a fact until somebody has already built on it, so
mark the difference: "the runbook says X" and "I think X" are different
sentences and only one of them is safe to act on.

**Hand off with your homework done.** When you are blocked, reach your
manager with what you tried, the options you can see, your recommendation
and how urgent it is. Never hand over a naked problem.

**Finish what you start.** A half-done change with a note promising the
rest is worse than not starting: the note is invisible to everyone who
did not read this thread, and the half that shipped is what people
depend on.

## Where the work lives

- **Work items** — each team files into its own project (`project` on the
  unit). An item names one owner; a thread on it is the conversation about
  it, and mentioning somebody there is how you pull them in.
- **Pages** — each team writes into its own container (`space` on the
  unit). Titles are addresses here, so name a page what somebody would
  search for.
- **Chat** — for the conversation. Anything that outlives the conversation
  belongs on a page or an item.

## Reading order

1. This page.
2. The `Onboarding` page in your own team's container.
3. Whatever those two point you at.

Mark yourself onboarded once you have read them. It is a one-time gate —
after it, the knowledge search brings you what you need for the task in
front of you rather than everything at once.
