# ADR-0160: An artifact link the engine handed out is cited as the artifact

- **Status:** accepted
- **Authority:** `internal/worklet`
- **Enforced-by:**
  - `internal/worklet.TestAnArtifactLinkThisEngineHandedOutIsCitedAsTheArtifact`
  - `internal/agent/builtin.TestADecisionCitingAPublishedArtifactsURLCitesTheArtifact`
  - `internal/decision.TestADecisionIsValidatedField`
- **Cost-when-tried:** on a deployment whose `api.external_url` is `http://`, a seat published an artifact, cited the `url` the publish answered in a decision to a person, and was refused because a `url` must be `https://`. It spent a further round of its whole context and left the person unasked.
- **Tag-status:** unreleased

## The decision

**A decision's evidence has an `artifact` kind, by the artifact's id, and a
`url` that is exactly the address this engine hands out for one of its
artifacts is stored as that kind.** `worklet.ValidateDecisionAt` resolves it
before the shape check, through `EvidenceLookup.OwnArtifact`, which answers
only an address equal to the one the artifact tools' single builder
(`Engine.ArtifactLink`) gives that id. Every other `url` is still held to an
absolute `https://` address. The dashboard draws `artifact` evidence as a link
to the artifact's own page.

## Why the obvious alternatives are wrong

**Accepting `http://` for the deployment's own origin.** The rule would read a
node's own configuration inside a shape check that every writer and reader of
a decision applies, so one stored decision could be valid on one node and
refused on another. It also leaves an address that moves when the deployment's
URL does.

**Accepting any address that carries `#/artifacts/<id>`.** A different host
with the same path is not this engine's artifact, and a person shown a button
for it would leave the dashboard believing they had not.

**Telling the seat to use the `artifact` kind instead.** The engine handed it
the address; refusing it back costs every seat the round the refusal was meant
to save.
