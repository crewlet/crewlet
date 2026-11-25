# ADR-0152: The company's sign-in policy is its own, within each node's limits

- **Status:** accepted
- **Authority:** `internal/config`
- **Enforced-by:**
  - `internal/config.TestANodeHoldsTheCompanysSignInPolicyWithinItsLimits`
  - `internal/config.TestAWritePastThisNodesSignInLimitIsRefused`
  - `internal/config.TestAWeakeningSignInChangeWarnsAndAStrengtheningOneDoesNot`
  - `internal/api/configapi.TestASignInValuePastThisNodesLimitIsRefusedAtItsDoor`
  - `internal/api/configapi.TestAWeakeningSignInChangeIsWarnedOfIt`
  - `internal/iam/session.TestALoweredLifetimeEndsALiveSessionAndARaisedOneStretchesNone`
  - `internal/iam/session.TestALoweredIdleLimitEndsALiveSessionAndARaisedOneStretchesNone`
  - `internal/api/authapi.TestASignInReadsTheCompanysPolicyAsItStandsThen`
  - `internal/api.TestTheSignInPolicyIsTheCompanysWithinThisNodesLimits`
- **Tag-status:** unreleased

## The decision

**How people sign in is the company's**, written in its document under
`sign_in` (`config.SignIn`): whether a second factor is required, the shortest
password, how long a session lives from sign-in, how long it lives unused, and
how long a proof of identity authorises a sensitive change. Every node enforces
the same values, read on every request, and an administrator changes them from
Settings › Sign-in policy without a restart.

**Each node holds them within its operator's limits**, `api.auth.limits` in its
own file (`config.SignInLimits`): the longest lifetime, idle limit and step-up
window, the lowest password floor, and whether the company may drop the second
factor at all (by default it may not). A write past the answering node's limit
is refused at the setting's path; a value already past one, because an operator
lowered a limit since, is enforced at the limit. The idle limit, an engine
constant before this, is a setting with a range like the others.

**A change asks for a recent proof and is recorded**, as every configuration
write is, and **a change that weakens the policy warns** saying what it allows
(`config.SignInWeakens`), which a screen asks a person to confirm.

**A lower lifetime or idle limit ends a live session at its next request; a
higher one reaches only sessions signed in after it.** The bearer carries its
sign-in and issue instants beside the deadlines it was signed with, and a node
ends a session at whichever comes first (`session.Bearer.EndsUnder`). The
step-up window is measured from a session's proof on every request, so a change
to it applies at the next request either way.

## Why the obvious alternatives are wrong

**Every value in each node's file**, as it was. A company's security posture
then differed node by node, changed only with a restart on every host, and a
fleet's dashboard could only report one node's reading of it. Two nodes behind
one load balancer disagreeing about the lifetime handed one browser two
answers.

**Every value in the company document, unbounded.** The document is written by
anybody holding admin, and so by anybody holding a stolen administrator
session: one write would drop the second factor for everybody. The operator's
limits are what such a write cannot cross, and they sit in the one tier the
company cannot write.

**A shortened lifetime reaching only new sessions**, as a signed deadline alone
does. The moment a company shortens its lifetime is usually the moment it
needs it to apply: after a lost laptop or a departure. A raised one reaching
live sessions, on the other hand, would revive a cookie that had already
lapsed.

## What this does not decide

What stays in each node's file: the API tokens and the keyring, which a node
needs before it can read any company; `admin`, `allowed_origins`,
`company_writers` and `accept_insecure`, the operator's posture for the node;
and the identity audit horizons, which an administrator shortening could use
to erase the trail of what they did. Who may read the policy is ADR-0033's:
an administrator.
