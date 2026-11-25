package browser

import "slices"

// EnsureVerdict is what a box's ensure answered of its browser: ready, or
// withheld for an [EnsureReason]. The package doc says why ensure's closed
// values live here and not beside the helper that prints them.
type EnsureVerdict string

// The verdicts: the browser is up, or withheld for its reason.
const (
	EnsureReady    EnsureVerdict = "ready"
	EnsureWithheld EnsureVerdict = "withheld"
)

// Valid reports whether v is a verdict ensure gives.
func (v EnsureVerdict) Valid() bool { return v == EnsureReady || v == EnsureWithheld }

// EnsureReason is why a box's browser is withheld: one a box's ensure prints,
// first failing check first, or one the engine adds about a box
// ([EnsureReason.Printed]).
type EnsureReason string

// The reasons, the box's in the order ensure checks them.
const (
	ReasonNodeMissing                EnsureReason = "node_missing"
	ReasonNodeTooOld                 EnsureReason = "node_too_old"
	ReasonFolderUnusable             EnsureReason = "folder_unusable"
	ReasonPlaywrightMissing          EnsureReason = "playwright_missing"
	ReasonChromiumMissing            EnsureReason = "chromium_missing"
	ReasonChromiumSandboxUnavailable EnsureReason = "chromium_sandbox_unavailable"
	ReasonChromiumStartFailed        EnsureReason = "chromium_start_failed"
	ReasonSupervisorFailed           EnsureReason = "supervisor_failed"
)

// The engine's reasons, in the order a launch meets them: the box outside a
// container, on an entry that denies its harness the web, or on a harness
// that would then hide the bridge's tools, each read off the launch alone,
// before the engine asks its own host or the box anything; then the box's
// network, which a check in the box finds before anything is written there;
// then the helper's write; then ensure, silent past its bound or printing no
// line the engine reads.
const (
	ReasonCellNotContainer EnsureReason = "cell_not_container"
	ReasonWebDenied        EnsureReason = "web_denied"
	ReasonBridgeDeferred   EnsureReason = "bridge_deferred"
	ReasonHostNetwork      EnsureReason = "host_network"
	ReasonHelperUnwritten  EnsureReason = "helper_unwritten"
	ReasonEnsureTimeout    EnsureReason = "ensure_timeout"
	ReasonEnsureUnreadable EnsureReason = "ensure_unreadable"
)

// EnsureCause is what ensure could tell of a reason's cause.
type EnsureCause string

// The causes, grouped by the reasons that take them.
const (
	CauseNotOnPath    EnsureCause = "not_on_path"
	CauseBelowFloor   EnsureCause = "below_floor"
	CauseNotDirectory EnsureCause = "not_directory"
	CauseNotOwned     EnsureCause = "not_owned"
	CauseMode         EnsureCause = "mode"
	CausePathTooLong  EnsureCause = "path_too_long"
	// CauseNoSpace and CauseUnwritable are a failed write's: a full disk or a
	// spent quota, and a refused, read-only or vanished path.
	CauseNoSpace             EnsureCause = "no_space"
	CauseUnwritable          EnsureCause = "unwritable"
	CauseDigestMismatch      EnsureCause = "digest_mismatch"
	CauseNPMMissing          EnsureCause = "npm_missing"
	CauseNPMTooOld           EnsureCause = "npm_too_old"
	CauseRegistryUnreachable EnsureCause = "registry_unreachable"
	CauseRegistryRefused     EnsureCause = "registry_refused"
	CauseIntegrityMismatch   EnsureCause = "integrity_mismatch"
	CauseInstallFailed       EnsureCause = "install_failed"
	CauseNotFound            EnsureCause = "not_found"
	CauseRoot                EnsureCause = "root"
	CauseSeccomp             EnsureCause = "seccomp"
	CauseUserns              EnsureCause = "userns"
	CauseAppArmor            EnsureCause = "apparmor"
	// CauseLauncher is a browser whose launcher dropped its sandbox: run with
	// --no-sandbox although the cell requires it.
	CauseLauncher    EnsureCause = "launcher"
	CauseUnknown     EnsureCause = "unknown"
	CauseLibraries   EnsureCause = "libraries"
	CauseProfileHeld EnsureCause = "profile_held"
	CauseExited      EnsureCause = "exited"
	CauseTimeout     EnsureCause = "timeout"
	// CauseAnotherKernel to CauseUnchecked are ReasonHostNetwork's: the box on
	// another kernel than the engine's; the route by which it reached the
	// engine's loopback, the first in this order; or a check that could not
	// tell.
	CauseAnotherKernel   EnsureCause = "another_kernel"
	CauseLoopback        EnsureCause = "loopback"
	CauseGateway         EnsureCause = "gateway"
	CauseHostAlias       EnsureCause = "host_alias"
	CauseRootlessGateway EnsureCause = "rootless_gateway"
	CauseProxy           EnsureCause = "proxy"
	CauseUnchecked       EnsureCause = "unchecked"
	// CauseRefused and CauseFailed are ReasonHelperUnwritten's: the box refused
	// a path holding something other than a regular file, or the write failed.
	CauseRefused EnsureCause = "refused"
	CauseFailed  EnsureCause = "failed"
	// CauseNoLine and CauseBadLine are ReasonEnsureUnreadable's: no line at
	// all, or one off the grammar.
	CauseNoLine  EnsureCause = "no_line"
	CauseBadLine EnsureCause = "bad_line"
)

// reasons is every reason with the causes it takes, the box's in the order
// ensure checks them, then the engine's in the order a launch meets them; an
// engine reason with nothing more to tell takes none. playwright_missing's
// causes are in the order an install meets them.
var reasons = []struct {
	reason  EnsureReason
	printed bool
	causes  []EnsureCause
}{
	{ReasonNodeMissing, true, []EnsureCause{CauseNotOnPath}},
	{ReasonNodeTooOld, true, []EnsureCause{CauseBelowFloor}},
	{ReasonFolderUnusable, true, []EnsureCause{CauseNotDirectory, CauseNotOwned, CauseMode, CausePathTooLong,
		CauseNoSpace, CauseUnwritable}},
	{ReasonPlaywrightMissing, true, []EnsureCause{CauseDigestMismatch, CauseNPMMissing, CauseNPMTooOld,
		CauseRegistryUnreachable, CauseRegistryRefused, CauseIntegrityMismatch, CauseTimeout, CauseNoSpace,
		CauseUnwritable, CauseInstallFailed}},
	{ReasonChromiumMissing, true, []EnsureCause{CauseNotFound}},
	{ReasonChromiumSandboxUnavailable, true, []EnsureCause{CauseRoot, CauseSeccomp, CauseUserns, CauseAppArmor,
		CauseLauncher, CauseUnknown}},
	{ReasonChromiumStartFailed, true, []EnsureCause{CauseLibraries, CauseProfileHeld, CauseExited, CauseTimeout,
		CauseUnknown}},
	{ReasonSupervisorFailed, true, []EnsureCause{CauseExited, CauseTimeout}},
	{ReasonCellNotContainer, false, nil},
	{ReasonWebDenied, false, nil},
	{ReasonBridgeDeferred, false, nil},
	{ReasonHostNetwork, false, []EnsureCause{CauseAnotherKernel, CauseLoopback, CauseGateway, CauseHostAlias,
		CauseRootlessGateway, CauseProxy, CauseUnchecked}},
	{ReasonHelperUnwritten, false, []EnsureCause{CauseRefused, CauseFailed}},
	{ReasonEnsureTimeout, false, nil},
	{ReasonEnsureUnreadable, false, []EnsureCause{CauseNoLine, CauseBadLine}},
}

// EnsureReasons is every reason, the box's in the order ensure checks them,
// then the engine's.
func EnsureReasons() []EnsureReason {
	out := make([]EnsureReason, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, r.reason)
	}
	return out
}

// Valid reports whether r is a reason of the list.
func (r EnsureReason) Valid() bool { return slices.Contains(EnsureReasons(), r) }

// Printed reports whether a box's ensure prints r, rather than the engine
// adding it about a box, so a box's line naming one of the engine's is no
// line ensure gives.
func (r EnsureReason) Printed() bool {
	for _, e := range reasons {
		if e.reason == r {
			return e.printed
		}
	}
	return false
}

// Causes is every cause r takes, none for a reason off the list or one that
// takes none.
func (r EnsureReason) Causes() []EnsureCause {
	for _, e := range reasons {
		if e.reason == r {
			return slices.Clone(e.causes)
		}
	}
	return nil
}

// Takes reports whether c may be recorded as r's cause: one of r's own, or,
// for a reason that takes none, no cause at all. The one rule for a valid
// pair, so a printed line and a launch's record are held to the same one.
func (r EnsureReason) Takes(c EnsureCause) bool {
	for _, e := range reasons {
		if e.reason == r {
			if c == "" {
				return len(e.causes) == 0
			}
			return slices.Contains(e.causes, c)
		}
	}
	return false
}

// Valid reports whether c is a cause some reason takes.
func (c EnsureCause) Valid() bool {
	for _, e := range reasons {
		if slices.Contains(e.causes, c) {
			return true
		}
	}
	return false
}

// ChromiumBuild is what ensure knows of the Chromium it chose: expected, the
// build the pinned Playwright expects ([ExpectedChromiumVersion]), its version
// matched exactly; untested, any other.
type ChromiumBuild string

// The builds ensure records.
const (
	BuildExpected ChromiumBuild = "expected"
	BuildUntested ChromiumBuild = "untested"
)

// Valid reports whether b is a build ensure records.
func (b ChromiumBuild) Valid() bool { return b == BuildExpected || b == BuildUntested }

// ChromiumSandbox is a cell's Chromium sandbox: required, Chromium's own
// sandbox on or no browser; or waived, Chromium run with --no-sandbox. Its
// zero value is neither, and is refused rather than read as either.
type ChromiumSandbox string

// The settings a cell may give.
const (
	SandboxRequired ChromiumSandbox = "required"
	SandboxWaived   ChromiumSandbox = "waived"
)

// ChromiumSandboxes is every setting a cell may give.
func ChromiumSandboxes() []ChromiumSandbox { return []ChromiumSandbox{SandboxRequired, SandboxWaived} }

// Valid reports whether s is a setting a cell may give.
func (s ChromiumSandbox) Valid() bool { return slices.Contains(ChromiumSandboxes(), s) }

// SupervisorStart is how ensure came by the box's live supervisor: found
// running as this launch needs it, started where none ran, or restarted from
// one of another helper, launch or failed browser.
type SupervisorStart string

// How ensure came by the supervisor.
const (
	SupervisorFound     SupervisorStart = "found"
	SupervisorStarted   SupervisorStart = "started"
	SupervisorRestarted SupervisorStart = "restarted"
)

// Valid reports whether s is a start ensure reports.
func (s SupervisorStart) Valid() bool {
	return s == SupervisorFound || s == SupervisorStarted || s == SupervisorRestarted
}
