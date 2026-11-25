package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/canon"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ErrOutcomeUnknown is a write that was sent and never answered. It may have
// landed.
//
// Two classes reach it, with different safe retries, so the sentence names
// neither (package doc, "What makes a retry safe, per operation"). A write a
// repeat is not safe for ([opOnceWrite]) is reported at the first node that
// did not answer, and is read back before it is written again. One carrying
// its caller's operation id ([opIdempotentWrite]) is reported only once no
// other data node ran it either; retry it under that SAME id, since a fresh
// one is a second write.
var ErrOutcomeUnknown = errors.New("estate: the write was sent to a data node " +
	"that did not answer, so whether it landed is unknown")

// sentinel is one error value whose identity crosses the wire.
type sentinel struct {
	code string
	err  error
}

// sentinels are every error value a serving node's answer may carry and a
// caller may compare with errors.Is.
//
// EVERY exported Err* of the crossing packages, not only those a tool compares
// today: that list grows in other packages and nothing would tell this one
// (TestEveryErrorOfTheCrossingPackagesIsRegistered fails on one missing). The
// code is the Go identifier, so a renamed sentinel fails the gate rather than
// leaving a dead code.
var sentinels = []sentinel{
	{"context.Canceled", context.Canceled},
	{"context.DeadlineExceeded", context.DeadlineExceeded},
	{"store.ErrNoEstate", store.ErrNoEstate},
	{"queue.ErrTooLarge", queue.ErrTooLarge},
	{"queue.ErrNotLive", queue.ErrNotLive},

	{"worklet.ErrAlreadyAnswered", worklet.ErrAlreadyAnswered},
	{"worklet.ErrBadCursor", worklet.ErrBadCursor},
	{"worklet.ErrBadQuery", worklet.ErrBadQuery},
	{"worklet.ErrForbidden", worklet.ErrForbidden},
	{"worklet.ErrInvalid", worklet.ErrInvalid},
	{"worklet.ErrIndexBuilding", worklet.ErrIndexBuilding},
	{"worklet.ErrNoChartActivation", worklet.ErrNoChartActivation},
	{"worklet.ErrNoComment", worklet.ErrNoComment},
	{"worklet.ErrNoFile", worklet.ErrNoFile},
	{"worklet.ErrNoProject", worklet.ErrNoProject},
	{"worklet.ErrNoTask", worklet.ErrNoTask},
	{"worklet.ErrNoView", worklet.ErrNoView},
	{"worklet.ErrNoType", worklet.ErrNoType},
	{"worklet.ErrNotAuthor", worklet.ErrNotAuthor},
	{"worklet.ErrNothingToRestore", worklet.ErrNothingToRestore},
	{"worklet.ErrReassignmentBudget", worklet.ErrReassignmentBudget},
	{"worklet.ErrReparentAcrossProjects", worklet.ErrReparentAcrossProjects},
	{"worklet.ErrRestoresOthers", worklet.ErrRestoresOthers},
	{"worklet.ErrStaleVersion", worklet.ErrStaleVersion},
	{"worklet.ErrState", worklet.ErrState},
	{"worklet.ErrStepUnresolved", worklet.ErrStepUnresolved},
	{"worklet.ErrStepUnvouched", worklet.ErrStepUnvouched},
	{"worklet.ErrTaken", worklet.ErrTaken},
	{"worklet.ErrTooBroad", worklet.ErrTooBroad},
	{"worklet.ErrUploadStale", worklet.ErrUploadStale},

	// The inbox rules', deciding a person's gesture on a domain's inbox on the
	// data node that writes it: ErrFull crosses as itself, since its remedy is
	// another gesture. ErrBadCursor is refused where an inbox is composed, not
	// across a hop today; registered so it would arrive as itself.
	{"inbox.ErrBadCursor", inbox.ErrBadCursor},
	{"inbox.ErrFull", inbox.ErrFull},
	{"inbox.ErrInvalid", inbox.ErrInvalid},

	{"doclet.ErrBadCursor", doclet.ErrBadCursor},
	{"doclet.ErrConflict", doclet.ErrConflict},
	{"doclet.ErrInvalid", doclet.ErrInvalid},
	{"doclet.ErrNoActivation", doclet.ErrNoActivation},
	{"doclet.ErrNotFound", doclet.ErrNotFound},
	{"doclet.ErrNotInTrash", doclet.ErrNotInTrash},
	{"doclet.ErrParent", doclet.ErrParent},
	{"doclet.ErrReserved", doclet.ErrReserved},
	{"doclet.ErrStaleVersion", doclet.ErrStaleVersion},
	{"doclet.ErrTitleTaken", doclet.ErrTitleTaken},
	{"doclet.ErrUnservedOrder", doclet.ErrUnservedOrder},

	{"search.ErrIndexBuilding", search.ErrIndexBuilding},
	{"search.ErrNoMethods", search.ErrNoMethods},
	{"search.ErrNoViewer", search.ErrNoViewer},

	// A cursor or range the asker formed: its mistake, answered by name
	// whichever data node read it.
	{"usage.ErrBadCursor", usage.ErrBadCursor},
	{"usage.ErrBadRange", usage.ErrBadRange},

	// Gitlet's, every one: ErrAnswerDue, ErrBadBundle, ErrBadLayer,
	// ErrMisplaced and ErrSuperseded never cross today, each answered on the
	// node that met it, but the list is complete for the reason above.
	{"gitlet.ErrAboveCeiling", gitlet.ErrAboveCeiling},
	{"gitlet.ErrAnswerDue", gitlet.ErrAnswerDue},
	{"gitlet.ErrBadBundle", gitlet.ErrBadBundle},
	{"gitlet.ErrBadCursor", gitlet.ErrBadCursor},
	{"gitlet.ErrBadLayer", gitlet.ErrBadLayer},
	{"gitlet.ErrBadLevel", gitlet.ErrBadLevel},
	{"gitlet.ErrChangeNotFound", gitlet.ErrChangeNotFound},
	{"gitlet.ErrCheckNotFound", gitlet.ErrCheckNotFound},
	{"gitlet.ErrConfigManaged", gitlet.ErrConfigManaged},
	{"gitlet.ErrInvalid", gitlet.ErrInvalid},
	{"gitlet.ErrInvalidLayer", gitlet.ErrInvalidLayer},
	{"gitlet.ErrInvalidProposal", gitlet.ErrInvalidProposal},
	{"gitlet.ErrMisplaced", gitlet.ErrMisplaced},
	{"gitlet.ErrNoActivation", gitlet.ErrNoActivation},
	{"gitlet.ErrNotABlob", gitlet.ErrNotABlob},
	{"gitlet.ErrNotATree", gitlet.ErrNotATree},
	{"gitlet.ErrNotConfigManaged", gitlet.ErrNotConfigManaged},
	{"gitlet.ErrNotDeleted", gitlet.ErrNotDeleted},
	{"gitlet.ErrNotPermitted", gitlet.ErrNotPermitted},
	{"gitlet.ErrNotServing", gitlet.ErrNotServing},
	{"gitlet.ErrNotYet", gitlet.ErrNotYet},
	{"gitlet.ErrObjectNotFound", gitlet.ErrObjectNotFound},
	{"gitlet.ErrPurging", gitlet.ErrPurging},
	{"gitlet.ErrRecordCap", gitlet.ErrRecordCap},
	{"gitlet.ErrRefNotFound", gitlet.ErrRefNotFound},
	{"gitlet.ErrRepoNotFound", gitlet.ErrRepoNotFound},
	{"gitlet.ErrSliceTooLarge", gitlet.ErrSliceTooLarge},
	{"gitlet.ErrSnapshotMoved", gitlet.ErrSnapshotMoved},
	{"gitlet.ErrSuperseded", gitlet.ErrSuperseded},

	{"chatlet.ErrArchived", chatlet.ErrArchived},
	{"chatlet.ErrConflict", chatlet.ErrConflict},
	{"chatlet.ErrMembersChanged", chatlet.ErrMembersChanged},
	{"chatlet.ErrForbidden", chatlet.ErrForbidden},
	{"chatlet.ErrInvalid", chatlet.ErrInvalid},
	{"chatlet.ErrNameTaken", chatlet.ErrNameTaken},
	{"chatlet.ErrNoActivation", chatlet.ErrNoActivation},
	{"chatlet.ErrNotFound", chatlet.ErrNotFound},
	{"chatlet.ErrReactionsFull", chatlet.ErrReactionsFull},
	{"chatlet.ErrRoomExists", chatlet.ErrRoomExists},
	{"chatlet.ErrUnknownKind", chatlet.ErrUnknownKind},

	{"statelog.ErrAdoptionUnrecorded", statelog.ErrAdoptionUnrecorded},
	{"statelog.ErrAheadOfLog", statelog.ErrAheadOfLog},
	{"statelog.ErrConflict", statelog.ErrConflict},
	{"statelog.ErrDonorConnectionClosed", statelog.ErrDonorConnectionClosed},
	{"statelog.ErrEstateNotRestored", statelog.ErrEstateNotRestored},
	{"statelog.ErrExists", statelog.ErrExists},
	{"statelog.ErrForeignPosition", statelog.ErrForeignPosition},
	{"statelog.ErrGenerationPassed", statelog.ErrGenerationPassed},
	{"statelog.ErrJoinerFault", statelog.ErrJoinerFault},
	{"statelog.ErrLogDiverged", statelog.ErrLogDiverged},
	{"statelog.ErrLogTruncated", statelog.ErrLogTruncated},
	{"statelog.ErrNoDecision", statelog.ErrNoDecision},
	{"statelog.ErrNoOffer", statelog.ErrNoOffer},
	{"statelog.ErrPositionRange", statelog.ErrPositionRange},
	{"statelog.ErrReanchorRefused", statelog.ErrReanchorRefused},
	{"statelog.ErrRecordTampered", statelog.ErrRecordTampered},
	{"statelog.ErrRecordUnverified", statelog.ErrRecordUnverified},
	{"statelog.ErrStopped", statelog.ErrStopped},
	{"statelog.ErrStreamRecreated", statelog.ErrStreamRecreated},
	{"statelog.ErrUnavailable", statelog.ErrUnavailable},
	{"recordseal.ErrUnsigned", recordseal.ErrUnsigned},
	{"statelog.ErrWaitAbandoned", statelog.ErrWaitAbandoned},
	{"statelog.ErrWrongStream", statelog.ErrWrongStream},

	// The identity directory's, read through a data node ([opIdentityBinding],
	// [opSeatHolders], [opRequester]), and the identity vocabulary's and the
	// session's, in which a person verb's questions are answered
	// ([iam.Holders], [session.ResolveSeat]).
	{"iamdomain.ErrAssistRefused", iamdomain.ErrAssistRefused},
	{"iamdomain.ErrCollapsed", iamdomain.ErrCollapsed},
	{"iamdomain.ErrInvalid", iamdomain.ErrInvalid},
	{"iamdomain.ErrInvalidLogin", iamdomain.ErrInvalidLogin},
	{"iamdomain.ErrInvalidToken", iamdomain.ErrInvalidToken},
	{"iamdomain.ErrNoBlindKey", iamdomain.ErrNoBlindKey},
	{"iamdomain.ErrNoCompany", iamdomain.ErrNoCompany},
	{"iamdomain.ErrNoInvitation", iamdomain.ErrNoInvitation},
	{"iamdomain.ErrNotCurrent", iamdomain.ErrNotCurrent},
	{"iamdomain.ErrNotEnrollable", iamdomain.ErrNotEnrollable},
	{"iamdomain.ErrNotFindable", iamdomain.ErrNotFindable},
	{"iamdomain.ErrNotFound", iamdomain.ErrNotFound},
	{"iamdomain.ErrOperationReused", iamdomain.ErrOperationReused},
	{"iamdomain.ErrRefused", iamdomain.ErrRefused},
	{"iamdomain.ErrSeatRequired", iamdomain.ErrSeatRequired},
	{"iamdomain.ErrTokenRefused", iamdomain.ErrTokenRefused},

	{"iam.ErrHolderUnseated", iam.ErrHolderUnseated},
	{"iam.ErrInvalidPrincipal", iam.ErrInvalidPrincipal},
	{"iam.ErrNoHolder", iam.ErrNoHolder},
	{"iam.ErrUnresolved", iam.ErrUnresolved},

	{"session.ErrMalformed", session.ErrMalformed},
	{"session.ErrNoCompany", session.ErrNoCompany},
	{"session.ErrNoKeyring", session.ErrNoKeyring},

	// The artifacts estate's ([Artifacts]) and its vocabulary's: an audience,
	// id or label a write refused, and the manifest grammar's and the media
	// table's refusals a publish wraps.
	{"artifactsdomain.ErrAboveOwnRole", artifactsdomain.ErrAboveOwnRole},
	{"artifactsdomain.ErrAgentCompanyShare", artifactsdomain.ErrAgentCompanyShare},
	{"artifactsdomain.ErrBadCursor", artifactsdomain.ErrBadCursor},
	{"artifactsdomain.ErrBrandContrast", artifactsdomain.ErrBrandContrast},
	{"artifactsdomain.ErrBrandIncomplete", artifactsdomain.ErrBrandIncomplete},
	{"artifactsdomain.ErrCommentCredential", artifactsdomain.ErrCommentCredential},
	{"artifactsdomain.ErrCommentLimit", artifactsdomain.ErrCommentLimit},
	{"artifactsdomain.ErrCommentNotFound", artifactsdomain.ErrCommentNotFound},
	{"artifactsdomain.ErrConflict", artifactsdomain.ErrConflict},
	{"artifactsdomain.ErrExists", artifactsdomain.ErrExists},
	{"artifactsdomain.ErrInvalid", artifactsdomain.ErrInvalid},
	{"artifactsdomain.ErrFolderCycle", artifactsdomain.ErrFolderCycle},
	{"artifactsdomain.ErrFolderDepth", artifactsdomain.ErrFolderDepth},
	{"artifactsdomain.ErrFolderLimit", artifactsdomain.ErrFolderLimit},
	{"artifactsdomain.ErrFolderNameTaken", artifactsdomain.ErrFolderNameTaken},
	{"artifactsdomain.ErrFolderNotFound", artifactsdomain.ErrFolderNotFound},
	{"artifactsdomain.ErrLabelTaken", artifactsdomain.ErrLabelTaken},
	{"artifactsdomain.ErrLabelsFull", artifactsdomain.ErrLabelsFull},
	{"artifactsdomain.ErrNotFound", artifactsdomain.ErrNotFound},
	{"artifactsdomain.ErrNotInTrash", artifactsdomain.ErrNotInTrash},
	{"artifactsdomain.ErrNotTheAuthor", artifactsdomain.ErrNotTheAuthor},
	{"artifactsdomain.ErrPurged", artifactsdomain.ErrPurged},
	{"artifactsdomain.ErrPurging", artifactsdomain.ErrPurging},
	{"artifactsdomain.ErrSourceTaken", artifactsdomain.ErrSourceTaken},
	{"artifactsdomain.ErrTrashed", artifactsdomain.ErrTrashed},
	{"artifactsdomain.ErrUploadStale", artifactsdomain.ErrUploadStale},

	{"artifacts.ErrInvalidAudience", artifacts.ErrInvalidAudience},
	{"artifacts.ErrInvalidID", artifacts.ErrInvalidID},
	{"artifacts.ErrInvalidLabel", artifacts.ErrInvalidLabel},
	{"manifest.ErrInvalid", manifest.ErrInvalid},
	{"manifest.ErrReserved", manifest.ErrReserved},
	{"mediatype.ErrRefused", mediatype.ErrRefused},

	// The datastore's ([ArtifactData]), its encoder's and query compiler's.
	{"artifactdata.ErrBadCursor", artifactdata.ErrBadCursor},
	{"artifactdata.ErrConflict", artifactdata.ErrConflict},
	{"artifactdata.ErrForbidden", artifactdata.ErrForbidden},
	{"artifactdata.ErrGone", artifactdata.ErrGone},
	{"artifactdata.ErrInvalid", artifactdata.ErrInvalid},
	{"artifactdata.ErrMigrationTooLarge", artifactdata.ErrMigrationTooLarge},
	{"artifactdata.ErrNotFound", artifactdata.ErrNotFound},
	{"artifactdata.ErrNotUndoable", artifactdata.ErrNotUndoable},
	{"canon.ErrDuplicateKey", canon.ErrDuplicateKey},
	{"canon.ErrInvalid", canon.ErrInvalid},
	{"canon.ErrTooDeep", canon.ErrTooDeep},
	{"query.ErrInvalid", query.ErrInvalid},
	{"query.ErrNotIndexed", query.ErrNotIndexed},

	// The executor log's ([ExecutorReads], [ExecutorWrites]): an ended turn, a
	// stale ordinal, a carry no longer named, the seat's budget, a delete no
	// longer due and a listing not answerable whole each mean a different next
	// step for the engine, so each crosses as itself.
	{"executordomain.ErrCarryGone", executordomain.ErrCarryGone},
	{"executordomain.ErrConflict", executordomain.ErrConflict},
	{"executordomain.ErrIncomplete", executordomain.ErrIncomplete},
	{"executordomain.ErrInvalid", executordomain.ErrInvalid},
	{"executordomain.ErrNotDue", executordomain.ErrNotDue},
	{"executordomain.ErrNotFound", executordomain.ErrNotFound},
	{"executordomain.ErrOverBudget", executordomain.ErrOverBudget},
	{"executordomain.ErrStaleCapture", executordomain.ErrStaleCapture},
	{"executordomain.ErrTurnEnded", executordomain.ErrTurnEnded},
	{"executordomain.ErrUnreadable", executordomain.ErrUnreadable},
	{"executordomain.ErrUploadStale", executordomain.ErrUploadStale},

	// The access log's ([AccessReads], [AccessWrites]).
	{"accessdomain.ErrConflict", accessdomain.ErrConflict},
	{"accessdomain.ErrInvalid", accessdomain.ErrInvalid},
	{"accessdomain.ErrNotDue", accessdomain.ErrNotDue},
	{"accessdomain.ErrRecorded", accessdomain.ErrRecorded},
	{"accessdomain.ErrUnreadable", accessdomain.ErrUnreadable},

	// The atlas log's ([AtlasReads], [AtlasWrites]).
	{"atlasdomain.ErrConflict", atlasdomain.ErrConflict},
	{"atlasdomain.ErrExists", atlasdomain.ErrExists},
	{"atlasdomain.ErrInvalid", atlasdomain.ErrInvalid},
	{"atlasdomain.ErrNotDue", atlasdomain.ErrNotDue},
	{"atlasdomain.ErrPurged", atlasdomain.ErrPurged},
	{"atlasdomain.ErrStaleVersion", atlasdomain.ErrStaleVersion},

	// The advisor log's ([AdvisorReads], [AdvisorWrites]).
	{"advisordomain.ErrConflict", advisordomain.ErrConflict},
	{"advisordomain.ErrExists", advisordomain.ErrExists},
	{"advisordomain.ErrHeld", advisordomain.ErrHeld},
	{"advisordomain.ErrIllegalMove", advisordomain.ErrIllegalMove},
	{"advisordomain.ErrInvalid", advisordomain.ErrInvalid},
	{"advisordomain.ErrNotFound", advisordomain.ErrNotFound},
	{"advisordomain.ErrStaleVersion", advisordomain.ErrStaleVersion},
	// The session bounds the executor log refuses a capture past live in
	// executorkeep, since the sandbox that takes a session stops at them too.
	{"executorkeep.ErrSessionTooLarge", executorkeep.ErrSessionTooLarge},
	{"executorkeep.ErrSessionPlainTooLarge", executorkeep.ErrSessionPlainTooLarge},

	{"estate.ErrOutcomeUnknown", ErrOutcomeUnknown},
	{"estate.ErrNoGitletNode", ErrNoGitletNode},
}

// typedKind is one error type whose value crosses the wire.
type typedKind struct {
	code string
	typ  reflect.Type
}

// rebuiltByPeer are the typed kinds that cannot be set field by field, each
// with what rebuilds it. A refusal's person phrasing is the one: its words are
// rebuilt only here ([caveat.SaidFromPeer]).
var rebuiltByPeer = map[string]func(typedError) (reflect.Value, bool){
	"caveat.Said": saidFromPeer,
}

// typedKinds are every error TYPE a caller may unwrap with errors.As, keyed
// by the Go identifier for the reason [sentinels] is. Each crosses field by
// field: a plain field as JSON, a field of type error (a refusal's cause) as
// a nested wire error, so the cause still answers errors.Is on the far side.
var typedKinds = []typedKind{
	{"worklet.AlreadyAnsweredError", reflect.TypeFor[*worklet.AlreadyAnsweredError]()},
	{"worklet.ErrAmbiguous", reflect.TypeFor[*worklet.ErrAmbiguous]()},
	{"worklet.ErrAmbiguousAnswer", reflect.TypeFor[*worklet.ErrAmbiguousAnswer]()},
	{"worklet.ErrFutureVersion", reflect.TypeFor[*worklet.ErrFutureVersion]()},
	{"worklet.MoveStopped", reflect.TypeFor[*worklet.MoveStopped]()},
	{"worklet.Refusal", reflect.TypeFor[*worklet.Refusal]()},
	{"worklet.SubtreeStopped", reflect.TypeFor[*worklet.SubtreeStopped]()},
	{"worklet.TagClash", reflect.TypeFor[*worklet.TagClash]()},
	{"worklet.TagsFull", reflect.TypeFor[*worklet.TagsFull]()},
	{"worklet.TextCapError", reflect.TypeFor[*worklet.TextCapError]()},

	{"doclet.ErrFutureVersion", reflect.TypeFor[*doclet.ErrFutureVersion]()},
	{"doclet.ErrUnknownVersion", reflect.TypeFor[doclet.ErrUnknownVersion]()},
	{"doclet.InvalidError", reflect.TypeFor[*doclet.InvalidError]()},

	{"chatlet.ErrFutureVersion", reflect.TypeFor[*chatlet.ErrFutureVersion]()},
	{"chatlet.ErrUnknownVersion", reflect.TypeFor[chatlet.ErrUnknownVersion]()},
	{"chatlet.InvalidError", reflect.TypeFor[*chatlet.InvalidError]()},

	{"inbox.Refusal", reflect.TypeFor[*inbox.Refusal]()},

	{"search.ErrFutureVersion", reflect.TypeFor[*search.ErrFutureVersion]()},

	{"usage.ErrFutureVersion", reflect.TypeFor[*usage.ErrFutureVersion]()},

	// Gitlet's, every one: BadBundle never crosses today (the node that
	// stored the bundle answers it), but the list is complete regardless.
	{"gitlet.BadBundle", reflect.TypeFor[*gitlet.BadBundle]()},
	{"gitlet.ChartRefusal", reflect.TypeFor[*gitlet.ChartRefusal]()},
	{"gitlet.ErrFutureVersion", reflect.TypeFor[*gitlet.ErrFutureVersion]()},
	{"gitlet.ErrGitUnready", reflect.TypeFor[*gitlet.ErrGitUnready]()},
	{"gitlet.TooWideError", reflect.TypeFor[*gitlet.TooWideError]()},

	{"statelog.CallerOpIDError", reflect.TypeFor[*statelog.CallerOpIDError]()},
	{"statelog.ErrSkipped", reflect.TypeFor[*statelog.ErrSkipped]()},
	{"statelog.FreshnessError", reflect.TypeFor[*statelog.FreshnessError]()},
	{"statelog.EvictionRefusal", reflect.TypeFor[*statelog.EvictionRefusal]()},
	{"statelog.ReadmissionRefusal", reflect.TypeFor[*statelog.ReadmissionRefusal]()},
	{"statelog.ReanchorNotApplied", reflect.TypeFor[*statelog.ReanchorNotApplied]()},
	{"statelog.Refused", reflect.TypeFor[*statelog.Refused]()},
	{"statelog.Telling", reflect.TypeFor[*statelog.Telling]()},
	{"statelog.Unavailable", reflect.TypeFor[*statelog.Unavailable]()},

	{"iamdomain.ErrFutureVersion", reflect.TypeFor[*iamdomain.ErrFutureVersion]()},
	{"iamdomain.ErrTaken", reflect.TypeFor[*iamdomain.ErrTaken]()},
	{"iamdomain.ErrUnknownVersion", reflect.TypeFor[iamdomain.ErrUnknownVersion]()},
	{"iamdomain.InvitationRedeemed", reflect.TypeFor[*iamdomain.InvitationRedeemed]()},
	{"iamdomain.NotReadBack", reflect.TypeFor[*iamdomain.NotReadBack]()},
	{"iamdomain.Refusal", reflect.TypeFor[*iamdomain.Refusal]()},

	{"iam.LookRefused", reflect.TypeFor[*iam.LookRefused]()},

	{"artifactsdomain.AlreadyExists", reflect.TypeFor[*artifactsdomain.AlreadyExists]()},
	{"artifactsdomain.BrandContrast", reflect.TypeFor[*artifactsdomain.BrandContrast]()},
	{"artifactsdomain.BrandIncomplete", reflect.TypeFor[*artifactsdomain.BrandIncomplete]()},
	{"artifactsdomain.CarriedObjectGone", reflect.TypeFor[*artifactsdomain.CarriedObjectGone]()},
	{"artifactsdomain.ErrFutureVersion", reflect.TypeFor[*artifactsdomain.ErrFutureVersion]()},
	{"artifactsdomain.FolderAmbiguous", reflect.TypeFor[*artifactsdomain.FolderAmbiguous]()},
	{"artifactsdomain.QuotaExceeded", reflect.TypeFor[*artifactsdomain.QuotaExceeded]()},
	{"artifactsdomain.VersionMoved", reflect.TypeFor[*artifactsdomain.VersionMoved]()},

	{"artifactdata.ErrFutureVersion", reflect.TypeFor[*artifactdata.ErrFutureVersion]()},
	{"artifactdata.RateLimited", reflect.TypeFor[*artifactdata.RateLimited]()},

	{"accessdomain.ErrFutureVersion", reflect.TypeFor[*accessdomain.ErrFutureVersion]()},
	{"atlasdomain.ErrFutureVersion", reflect.TypeFor[*atlasdomain.ErrFutureVersion]()},
	{"advisordomain.ErrFutureVersion", reflect.TypeFor[*advisordomain.ErrFutureVersion]()},
	{"executordomain.ErrFutureVersion", reflect.TypeFor[*executordomain.ErrFutureVersion]()},

	{"estate.ErrUnserved", reflect.TypeFor[*ErrUnserved]()},

	// A request a node's build cannot read whole has its own code, so the
	// asking router reads it as "this node ran nothing" and asks the next
	// rather than as a decode failure, and a second hop
	// carries it back as itself (package doc, "A request is read whole, or
	// runs nowhere").
	{"estate.ErrUnknownKeys", reflect.TypeFor[*ErrUnknownKeys]()},

	// A refusal's words for a person (ADR-0060), so a write refused on a data
	// node answers the person on this one in them. Without them (an older
	// peer) the refusal reads in its code's own sentence, never a model's.
	{"caveat.Said", reflect.TypeFor[*caveat.Said]()},

	// The field a Gitlet refusal names, so a surface on this node says which
	// value a data node refused (ADR-0060). An older peer drops it, and the
	// refusal reads in its code's own sentence.
	{"gitlet.FieldRefused", reflect.TypeFor[*gitlet.FieldRefused]()},
}

// wireError is an error as it crosses: what it says, and what it is.
type wireError struct {
	Message   string       `json:"message"`
	Sentinels []string     `json:"sentinels,omitempty"`
	Typed     []typedError `json:"typed,omitempty"`
}

// typedError is one typed value in an error's chain.
type typedError struct {
	Code   string                     `json:"code"`
	Fields map[string]json.RawMessage `json:"fields,omitempty"`
	Causes map[string]*wireError      `json:"causes,omitempty"`
}

var errorType = reflect.TypeFor[error]()

// encodeError is err as it crosses, or nil.
func encodeError(err error) *wireError {
	if err == nil {
		return nil
	}
	w := &wireError{Message: err.Error()}
	for _, s := range sentinels {
		if errors.Is(err, s.err) {
			w.Sentinels = append(w.Sentinels, s.code)
		}
	}
	for _, k := range typedKinds {
		target := reflect.New(k.typ)
		if !errors.As(err, target.Interface()) {
			continue
		}
		typed, encErr := encodeTyped(k.code, target.Elem())
		if encErr != nil {
			// The message and sentinels still cross; only this type's
			// fields are lost, which the registry gate keeps from happening.
			continue
		}
		w.Typed = append(w.Typed, typed)
	}
	return w
}

// encodeTyped carries one typed error's exported fields.
func encodeTyped(code string, v reflect.Value) (typedError, error) {
	out := typedError{Code: code}
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return out, nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return out, fmt.Errorf("estate: %s is not a struct", code)
	}
	for i := range v.NumField() {
		field := v.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		value := v.Field(i)
		if field.Type == errorType {
			if value.IsNil() {
				continue
			}
			if out.Causes == nil {
				out.Causes = map[string]*wireError{}
			}
			out.Causes[field.Name] = encodeError(value.Interface().(error))
			continue
		}
		raw, err := json.Marshal(value.Interface())
		if err != nil {
			return out, fmt.Errorf("estate: %s.%s: %w", code, field.Name, err)
		}
		if out.Fields == nil {
			out.Fields = map[string]json.RawMessage{}
		}
		out.Fields[field.Name] = raw
	}
	return out, nil
}

// remoteError is an error rebuilt from the wire: the message the serving node
// wrote, answering errors.Is and errors.As for every identity it carried.
type remoteError struct {
	msg       string
	sentinels []error
	typed     []reflect.Value
}

func (e *remoteError) Error() string { return e.msg }

// Is answers for every sentinel the original matched, anywhere in its chain.
func (e *remoteError) Is(target error) bool {
	for _, s := range e.sentinels {
		if s == target {
			return true
		}
	}
	return false
}

// As assigns the first carried value of the target's type.
func (e *remoteError) As(target any) bool {
	tv := reflect.ValueOf(target)
	if tv.Kind() != reflect.Pointer || tv.IsNil() {
		return false
	}
	want := tv.Type().Elem()
	for _, v := range e.typed {
		if v.Type().AssignableTo(want) {
			tv.Elem().Set(v)
			return true
		}
	}
	return false
}

// decodeError rebuilds an error from the wire, or nil.
//
// An identity this build does not know (a newer peer's) is DROPPED and its
// message kept: the caller loses no branch it could have taken anyway.
func decodeError(w *wireError) error {
	if w == nil {
		return nil
	}
	out := &remoteError{msg: w.Message}
	for _, code := range w.Sentinels {
		for _, s := range sentinels {
			if s.code == code {
				out.sentinels = append(out.sentinels, s.err)
				break
			}
		}
	}
	for _, t := range w.Typed {
		if v, ok := decodeTyped(t); ok {
			out.typed = append(out.typed, v)
		}
	}
	return out
}

// decodeTyped rebuilds one typed value, reporting false for a code this build
// does not know or fields it cannot read.
func decodeTyped(t typedError) (reflect.Value, bool) {
	var kind *typedKind
	for i := range typedKinds {
		if typedKinds[i].code == t.Code {
			kind = &typedKinds[i]
			break
		}
	}
	if kind == nil {
		return reflect.Value{}, false
	}
	if rebuilt, ok := rebuiltByPeer[t.Code]; ok {
		return rebuilt(t)
	}
	structType := kind.typ
	if structType.Kind() == reflect.Pointer {
		structType = structType.Elem()
	}
	ptr := reflect.New(structType)
	v := ptr.Elem()
	for name, raw := range t.Fields {
		field := v.FieldByName(name)
		if !field.IsValid() || !field.CanSet() {
			continue
		}
		if err := json.Unmarshal(raw, field.Addr().Interface()); err != nil {
			return reflect.Value{}, false
		}
	}
	for name, cause := range t.Causes {
		field := v.FieldByName(name)
		if !field.IsValid() || !field.CanSet() || field.Type() != errorType {
			continue
		}
		if err := decodeError(cause); err != nil {
			field.Set(reflect.ValueOf(err))
		}
	}
	if kind.typ.Kind() == reflect.Pointer {
		return ptr, true
	}
	return v, true
}

// saidFromPeer rebuilds a refusal's person phrasing a peer carried as
// `{"Words": {"person": "<sentence>", "remedy": {...}}}`, or reports false for
// missing or unreadable words, which leave the refusal its code's own sentence.
func saidFromPeer(t typedError) (reflect.Value, bool) {
	raw, ok := t.Fields["Words"]
	if !ok {
		return reflect.Value{}, false
	}
	said, err := caveat.SaidFromPeer(raw)
	if err != nil || said == nil {
		return reflect.Value{}, false
	}
	return reflect.ValueOf(said), true
}
