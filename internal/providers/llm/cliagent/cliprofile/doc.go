// Package cliprofile is what each coding CLI accepts: the profile table the
// cli-agent backend drives a CLI from — its binary, its flags, the JSON shapes
// its answer and usage arrive in, where it keeps its login and which
// environment variables carry a credential — plus the merge of an operator's
// `cli.overrides` into it and the rules a merged profile is held to.
//
// A PACKAGE OF ITS OWN, and a leaf, because config validates an entry against
// it. Whether a typo in `cli.overrides` names a real field, whether the merged
// profile can drive its CLI, and which variable the CLI reads its key from
// decide whether a `providers.llm` entry runs, and runs signed in. Judged only
// where the backend is built, they would be refused by `crewlet validate` and
// by every node's apply but ADMITTED by every API write, which never builds a
// provider: a revision the API activates and the fleet then refuses. Config
// cannot import the backend itself: that would pull a subprocess supervisor
// into the package everything imports, and the backend's own tests read
// config. So the table lives here, depending on the standard library, the
// YAML decoder and internal/hostbox — itself standard library alone — the same
// arrangement providers/llm/anthropic/claudemodel has for the Claude request
// shapes.
//
// TWO RULE SETS, because config holds them differently. [Profile.Validate],
// which [Load] runs, is whether the merged profile can drive a CLI at all —
// RUNNABLE, so a node refuses to apply a revision that breaks it.
// [Profile.ValidateCredentials] is where the profile lets a credential in
// around cli.auth, [Profile.ValidateEnv] where it names a variable the engine
// sets itself (a relocation naming a base variable included), and
// [Profile.ValidateAuthoring] where it declares what its CLI never acts on as
// written — usage counts that are not a pair where they are read, a setting
// its own mode never reads — all three ADMISSION, so a write is refused and an
// applied revision only warned about, since such a profile still runs. The
// line between the two is whether the profile CAN RUN, never how serious the
// slip looks: a relocation that leaves the seat home shares state between
// seats and is runnable, one that names a base variable is ignored and is not.
//
// It is also where a seat's ISOLATION is assembled ([Profile.SeatEnv]): which
// variables point a CLI's state inside one seat's home, and therefore which
// ones no other layer of a child's environment may name
// ([Profile.EngineEnv]). Here rather than in the backend because both read
// it — the backend sets them, config refuses a document that names one. The
// BASE of it is not spelled here: it is hostbox.Isolation, the one spelling
// the local sandbox's direct boxes use too, and what this package adds over it
// is only each vendor's own relocation variables ([Profile.ConfigEnv]).
//
// It is DATA, not code. Every field is replaceable from YAML because these
// flags and JSON shapes belong to vendors who rename them between releases,
// and a vendor renaming --output-format must be an operator's config edit
// rather than a Crewlet release.
package cliprofile
