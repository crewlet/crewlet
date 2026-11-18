// Package harnessprofile is what each harness accepts: the profile table the
// harness backend drives a harness from — its binary, its flags, the JSON shapes
// its answer and usage arrive in, where it keeps its login and which
// environment variables carry a credential — plus the merge of an operator's
// `overrides` into it and the rules a merged profile is held to.
//
// A PACKAGE OF ITS OWN, and a leaf, because config validates an entry against
// it. Whether a typo in `overrides` names a real field, whether the merged
// profile can drive its harness, and which variable the harness reads its key from
// decide whether a `providers.llm` entry runs, and runs signed in. Judged only
// where the backend is built, they would be refused by `crewlet validate` and
// by every node's apply but ADMITTED by every API write, which never builds a
// provider: a revision the API activates and the fleet then refuses. Config
// cannot import the backend itself: that would pull a subprocess supervisor
// into the package everything imports, and the backend's own tests read
// config. So the table lives here, depending on the standard library and the
// YAML decoder alone.
//
// TWO RULE SETS, because config holds them differently. [Profile.Validate],
// which [Load] runs, is whether the merged profile can drive a harness at all —
// RUNNABLE, so a node refuses to apply a revision that breaks it.
// [Profile.ValidateCredentials] is where the profile lets a credential in
// around auth — ADMISSION, so a write is refused and an applied revision
// only warned about, since such a profile still runs.
//
// It is DATA, not code. Every field is replaceable from YAML because these
// flags and JSON shapes belong to vendors who rename them between releases,
// and a vendor renaming --output-format must be an operator's config edit
// rather than a Crewlet release.
//
// # Platforms
//
// A built-in may carry a `platforms` section ([Platforms]: darwin, linux),
// merged in BEFORE the operator's `overrides`, so an operator's field outranks
// it. Every built-in is validated as each platform reads it, not only the one
// CI runs on.
//
// # Admission and apply
//
// A credential no variable of the harness carries, or one handed over in a
// variable outside the reference grammar, is refused by every write path and
// only warned about on an apply: the entry still runs, signed in by whatever
// its harness reads, and refusing a revision a newer peer admitted would take
// the node off the fleet's epoch. A profile that does not load is refused by
// both.
//
// # Tool route and web stance
//
// The tool route is profile data, measured per harness: `mcp` (how the harness
// is told of the `crewlet` server), `requests` (how its own record shows each
// model request), `cap` (its own turn cap), `failure_markers` (a stderr line
// saying its confinement failed to load, which stops the run) and
// `key_seed_files` (settings a KEY home alone gets). Seeds go ONLY into the
// call's working directory, because the socket is the run's alone and the
// seat's home is shared. A profile with no `mcp` answers text functions and
// refuses a seat's phase naming `overrides.mcp`; one with no `requests` counts a
// run as one round, never refused.
//
// The seats keep a harness's own web tools; `overrides: {web_tools: denied}`
// runs the profile's measured `web_denial` instead, refused on a profile with
// none, and [Load] applies it so every reader sees the argv that runs.
// `tools_off` is the argv runs that leave a harness no tool of its own, which
// [Profile.ToolsOffProblem] checks against the argv and the `mcp` allow list (the
// Dashboard Assistant's gate): a deny list is always a release behind, and Codex
// keeps apply_patch, which has no switch.
//
// # Binary
//
// A profile's executable is a [Binary], not a string, carried as that type
// into every invocation and server start, so a harness started anywhere in the
// module can be found by type. It is a string on the wire.
package harnessprofile
