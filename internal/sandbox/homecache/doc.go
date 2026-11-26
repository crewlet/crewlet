// Package homecache is a node's cache of provisioned box homes: the home a
// container box's setup steps leave, kept once per key so that a later box
// starts from a copy of it instead of running those steps again (#2005).
//
// # A home, not an image
//
// Setup steps write inside the box's home, and a container box's home is a
// bind mount of a directory on the engine host (`-v <box home>:/home/user`).
// `docker commit` captures no mounted volume, so an image committed from a
// provisioned box holds none of what its setup did. The home itself is the
// thing to keep. Restoring it is a copy: measured on the build VM (ext4, warm
// page cache), 0.18 s for a home holding Claude Code (364 MB) and 0.5-0.6 s
// for one holding Claude Code, Gemini CLI and Codex (888 MB), where
// installing those took 3.8 s and 14.3 s more.
//
// Only a container box's home is kept. Every container box's home is
// /home/user, so an absolute path a step writes (the git-auth recipe's
// credential helper, `"!'$HOME/...'"`) still names the right file in the next
// box; a `direct` box's home is minted per box, and a copy would point at the
// build box's home, gone.
//
// # What is cached: the prefix of the steps that need no credential
//
// The build that fills the cache is handed no credential, so none can be in
// what it leaves. [CachedPrefix] is the steps up to the first one that needs
// one: a step whose env carries a ${VAR} reference, or one of whose files is
// exactly one or holds a credential value ([Input.Secrets]), a step whose
// commands name a variable the build is not handed ([Input.Withheld]), or a
// step marked to run in every box ([Step.EveryBox]).
// That step and every later one run in each box after the copy, with the
// launch's environment, in the order they are written. A step whose need for a
// credential none of those see fails in the build, which is handed nothing,
// rather than caching an empty token: the remedy is to mark it.
//
// A step whose effects leave the home (a rootless-Podman box runs as root,
// and may install a package system-wide) cannot be kept in a home either. The
// builder reports the first such step ([Built.Escaped]); the cache records a
// CUT for the key it was building, so it and every later step run in each box,
// and builds once more with the shorter prefix. The cut is kept with the key,
// so nothing tries the longer prefix again while the setup is unchanged.
//
// After a build, every value the caller names as a credential ([Input.Secrets]:
// what a launch of the seat would hand its box, at least eight bytes long) is
// looked for in the tree's paths, link targets and file contents. The build
// was handed none of them, so a hit is an engine bug or a credential that is
// also ordinary text: the tree is discarded, and the key is cut at its first
// step ([Cut.Secret], naming the path, never the value), so every step runs in
// each box as if nothing were cached, and the caller says so.
//
// # Rebuilt when its key changes, and at no age
//
// The key ([KeyOf]) is the container runtime, the image's ID (so pulling a
// newer tag rebuilds), the cached steps' whole content and the environment
// the build is handed (the seat's identity facts among it). Nothing else
// rebuilds a tree: a step that installs a harness unpinned is frozen at the
// release the build found until the setup or the image changes. That is the
// safer default. Harnesses release about daily (Claude Code, OpenCode, Codex
// and Copilot over the 90 days to 2026-11-26), and the engine runs each on the
// release its profile is written for: a rebuild on a clock would move seats
// onto releases nobody measured, and, for OpenCode, end every session's
// continuation at a random time. A pin moves a seat, deliberately.
//
// # This node's alone
//
// The cache is derived from company configuration and this host's image, read
// only by this node's box creation, and costs one build to lose. So nobody
// else has to agree on it: no coordination store, no object store, no
// replicated row. Replicating it would ship gigabytes to save a build of
// seconds once per node.
//
// # Layout and concurrency
//
// Under the root: trees/<key> is a published tree, cuts/<key>.json a key's
// cut, objects/ the content every tree shares, locks/<key> a key's lock and
// owners/ whose each key is.
//
//   - A tree is published by renaming its build into place, so a reader sees
//     a whole tree or none. The builder's directory must be on the cache's
//     filesystem.
//   - A key's lock ([flock.Try], waited for) is held while it is built and
//     while it is copied out, so two takers of one key, in one process or in
//     two, build it once. [Cache.Collect] takes it without waiting, and leaves
//     a key whose lock is held, so no copy in flight loses its tree.
//   - A lock file is never removed ([flock]'s rule: a peer that has opened it
//     would lock an unlinked inode). One empty file per key ever built is
//     what that costs.
//   - Identical files of different trees are one inode (objects/<sha256>-<mode>,
//     hard-linked), so seats that differ only by their identity facts share
//     their harness's bytes. A box is handed a copy, never a link: nothing
//     writes a cached file.
//   - Each key a box is provisioned through is recorded as its owner's
//     (owners/, a seat), and an owner's older keys are removed as its new
//     one is recorded. [Cache.Collect] removes what no owner it is told to
//     keep holds, and every object no tree holds. The disk the cache takes is
//     the bytes of its owners' current keys, so it needs no cap of its own.
package homecache
