package coordtest

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// PositionsWidest is one key family of the positions register at its widest:
// every field at the bound its record's Validate holds it to, each byte of
// free text one JSON writes as six. Record is what its writer hands the store,
// Write writes it through a fleet and Past writes it with one field past its
// bound, which every backend refuses before anything is stored.
type PositionsWidest struct {
	// Class is the family's key class, and Key the key Record is written
	// under.
	Class, Key string
	// Bound is the most its class holds (the coord constant the KV
	// backend's holding declares for it).
	Bound int
	// Record is the record, or the value of an opaque family.
	Record any
	// Write and Past write the widest record and one past it.
	Write, Past func(ctx context.Context, f coord.Fleet) error
}

// Stored is how many bytes Record is stored as: the value itself for an
// opaque family, else its JSON as the store encodes it.
func (w PositionsWidest) Stored() int {
	if raw, ok := w.Record.([]byte); ok {
		return len(raw)
	}
	raw, err := json.Marshal(w.Record)
	if err != nil {
		return math.MaxInt32
	}
	return len(raw)
}

// widestAt is an instant at its longest as a record encodes it: UTC, to the
// nanosecond.
var widestAt = time.Date(2026, 11, 24, 10, 0, 0, 123456789, time.UTC)

// escaped is n bytes JSON writes as six each, and plain n it writes as one.
func escaped(n int) string { return strings.Repeat("<", n) }
func plain(n int) string   { return strings.Repeat("a", n) }

// numbered is a plain identifier of n bytes ending in i, so several differ.
func numbered(i, n int) string { return fmt.Sprintf("a%0*d", n-1, i) }

// storedJSON is a JSON value stored as exactly size bytes, every one it can
// escaped.
func storedJSON(size int) json.RawMessage {
	frame := len(`{"d":""}`)
	text := escaped((size-frame)/6) + plain((size-frame)%6)
	raw, err := json.Marshal(map[string]string{"d": text})
	if err != nil || len(raw) != size {
		panic(fmt.Sprintf("coordtest: a JSON value of %d bytes came out as %d (%v)", size, len(raw), err))
	}
	return raw
}

// widestStreams is a position on each of the most logs a record names, each
// under its own stream at its longest.
func widestStreams() map[string]coord.Position {
	out := map[string]coord.Position{}
	for i := range coord.MaxLogDomains {
		stream := numbered(i, coord.MaxStreamNameBytes)
		out[stream] = coord.Position{Stream: stream, Generation: math.MaxUint32, Seq: math.MaxUint64}
	}
	return out
}

// WidestPositionsRecords is every key family the positions register declares
// a class for at its widest, beside the classes earlier records of their own
// cover (an MCP listing's, definition's and check's, an access review's, a
// seat's access record's, a Home board's, an atlas wake's and a device
// sign-in's).
func WidestPositionsRecords() []PositionsWidest {
	actor, credential := plain(coord.MaxActorBytes), plain(coord.MaxActorBytes)
	seat := uuid.MustParse("01938a6e-7a3e-7cd1-9b2a-0f1e2d3c4b5a")
	var out []PositionsWidest
	add := func(w PositionsWidest) { out = append(out, w) }

	node := coord.NodePositions{NodeID: plain(coord.MaxNodeIDBytes), At: widestAt,
		EngineVersion: plain(coord.MaxEngineVersionBytes), Domains: map[string]coord.DomainPosition{},
		SnapshotBytes: math.MinInt64, SnapshotSkip: plain(coord.MaxSnapshotSkipBytes)}
	for i := range coord.MaxLogDomains {
		node.Domains[numbered(i, coord.MaxDomainNameBytes)] = coord.DomainPosition{
			Seq: math.MaxUint64, Generation: math.MaxUint32, AppliedThrough: math.MaxUint64,
			StreamCreatedAt: widestAt, CheckpointStoredAt: widestAt, SnapshotSeq: math.MaxUint64,
			SnapshotGeneration: math.MaxUint32, SnapshotAt: widestAt, Deferred: math.MinInt64,
			RecordVersion: math.MinInt64, LogDiverged: true}
	}
	add(PositionsWidest{Class: coord.PositionClass, Key: coord.PositionKey(node.NodeID),
		Bound: coord.SmallValueBytes, Record: node,
		Write: func(ctx context.Context, f coord.Fleet) error { return f.PutPositions(ctx, node) },
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := node
			past.Domains = map[string]coord.DomainPosition{"past": {}}
			for name, d := range node.Domains {
				past.Domains[name] = d
			}
			return f.PutPositions(ctx, past)
		}})

	hold := coord.TrimHold{Owner: plain(coord.MaxHoldOwnerBytes), Streams: widestStreams(),
		Reason: escaped(coord.MaxHoldReasonBytes)}
	add(PositionsWidest{Class: coord.HoldClass, Key: coord.HoldKey(hold.Owner), Bound: coord.SmallValueBytes,
		Record: hold,
		Write:  func(ctx context.Context, f coord.Fleet) error { return f.PutHold(ctx, hold) },
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := hold
			past.Reason += "<"
			return f.PutHold(ctx, past)
		}})

	floor := coord.TrimFloor{Domain: plain(coord.MaxDomainNameBytes), Generation: math.MaxUint32,
		TrimTo: math.MaxUint64, Floor: math.MaxUint64, BlockedBy: plain(coord.MaxTrimTermNameBytes),
		BlockedSince: widestAt, At: widestAt, By: plain(coord.MaxNodeIDBytes)}
	for range coord.MaxTrimTerms {
		floor.Terms = append(floor.Terms, coord.TrimTerm{Name: plain(coord.MaxTrimTermNameBytes),
			Seq: math.MaxUint64, Known: true, Absent: true,
			Detail: coord.FitText(escaped(coord.MaxTrimDetailBytes), coord.MaxTrimDetailBytes)})
	}
	add(PositionsWidest{Class: coord.FloorClass, Key: coord.FloorKey(floor.Domain), Bound: coord.SmallValueBytes,
		Record: floor,
		Write:  func(ctx context.Context, f coord.Fleet) error { return f.PutFloor(ctx, floor) },
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := floor
			past.Terms = append(append([]coord.TrimTerm{}, floor.Terms...), floor.Terms[0])
			return f.PutFloor(ctx, past)
		}})

	backup := coord.BackupPoint{Owner: plain(coord.MaxNodeIDBytes), At: widestAt, Streams: widestStreams(),
		Dir: escaped(coord.MaxBackupDirBytes), Verified: true, Bytes: math.MinInt64}
	add(PositionsWidest{Class: coord.BackupPointClass, Key: coord.BackupPointKey(backup),
		Bound: coord.SmallValueBytes, Record: backup,
		Write: func(ctx context.Context, f coord.Fleet) error { return f.PutBackupPoint(ctx, backup) },
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := backup
			past.Dir += "<"
			return f.PutBackupPoint(ctx, past)
		}})

	op := WidestMaintenanceOperation()
	add(PositionsWidest{Class: coord.MaintenanceClass, Key: coord.MaintenanceKey(op.Stream),
		Bound: coord.SmallValueBytes, Record: op,
		Write: func(ctx context.Context, f coord.Fleet) error {
			_, _, err := f.OpenMaintenance(ctx, op)
			return err
		},
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := op
			past.Stream = "past"
			past.Participants = append(append([]string{}, op.Participants...), "past")
			_, _, err := f.OpenMaintenance(ctx, past)
			return err
		}})

	admission := coord.Admission{NodeID: plain(coord.MaxNodeIDBytes),
		Incarnation: plain(coord.MaxIncarnationBytes), At: widestAt}
	add(PositionsWidest{Class: coord.AdmissionClass, Key: coord.AdmissionKey(admission.NodeID),
		Bound: coord.SmallValueBytes, Record: admission,
		Write: func(ctx context.Context, f coord.Fleet) error { return f.PutAdmission(ctx, admission) },
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := admission
			past.Incarnation += "a"
			return f.PutAdmission(ctx, past)
		}})

	ack := coord.MaintenanceAck{Stream: plain(coord.MaxStreamNameBytes), NodeID: plain(coord.MaxNodeIDBytes),
		OperationID: plain(coord.MaxIncarnationBytes), Attempt: math.MaxInt,
		Incarnation: plain(coord.MaxIncarnationBytes), Mode: plain(64), At: widestAt}
	add(PositionsWidest{Class: coord.MaintenanceAckClass, Key: coord.MaintenanceAckKey(ack.Stream, ack.NodeID),
		Bound: coord.SmallValueBytes, Record: ack,
		Write: func(ctx context.Context, f coord.Fleet) error { return f.PutMaintenanceAck(ctx, ack) },
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := ack
			past.Mode += "a"
			return f.PutMaintenanceAck(ctx, past)
		}})

	retirement := coord.NodeRetirement{NodeID: plain(coord.MaxNodeIDBytes), By: actor,
		ByKind: iam.ActorOperator, OperatorID: credential, At: widestAt}
	add(PositionsWidest{Class: coord.RetirementClass, Key: coord.RetirementKey(retirement.NodeID),
		Bound: coord.SmallValueBytes, Record: retirement,
		Write: func(ctx context.Context, f coord.Fleet) error { return f.RetireNode(ctx, retirement) },
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := retirement
			past.NodeID += "a"
			return f.RetireNode(ctx, past)
		}})

	mark := coord.FeedMark{Stream: plain(coord.MaxStreamNameBytes), Group: plain(coord.MaxDomainNameBytes),
		Generation: math.MaxUint32, Floor: math.MaxUint64, At: widestAt}
	add(PositionsWidest{Class: coord.FeedMarkClass, Key: coord.FeedMarkKey(mark.Stream, mark.Group),
		Bound: coord.SmallValueBytes, Record: mark,
		Write: func(ctx context.Context, f coord.Fleet) error {
			_, err := f.RaiseFeedMark(ctx, mark)
			return err
		},
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := mark
			past.Group += "a"
			_, err := f.RaiseFeedMark(ctx, past)
			return err
		}})

	pause := coord.SeatPause{Seat: seat, By: actor, ByKind: iam.ActorSystem, OperatorID: credential,
		Reason: escaped(coord.MaxSeatPauseReasonRunes), StopRunning: true, At: widestAt,
		Failure: &coord.SeatFailure{Kind: plain(128), Basis: plain(128)}}
	add(PositionsWidest{Class: coord.SeatPauseClass, Key: coord.SeatPauseKey(seat), Bound: coord.SmallValueBytes,
		Record: pause,
		Write: func(ctx context.Context, f coord.Fleet) error {
			_, _, err := f.CreateSeatPause(ctx, pause)
			return err
		},
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := pause
			past.Reason += "<"
			_, _, err := f.CreateSeatPause(ctx, past)
			return err
		}})

	reading := coord.BoxReading{Seat: seat, Cell: escaped(256), Basis: escaped(256), MemoryBytes: math.MaxInt64,
		Browser: coord.BoxPlaywrightChromium, ContainerRuntime: "podman", At: widestAt}
	add(PositionsWidest{Class: coord.BoxReadingClass, Key: coord.BoxReadingKey(seat, reading.Cell),
		Bound: coord.SmallValueBytes, Record: reading,
		Write: func(ctx context.Context, f coord.Fleet) error { return f.PutBoxReading(ctx, reading) },
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := reading
			past.Cell += "<"
			return f.PutBoxReading(ctx, past)
		}})

	held := coord.ConversationHold{Seat: seat, Conversation: escaped(coord.MaxConversationIDBytes),
		Kind: escaped(256), Basis: escaped(256), At: widestAt}
	add(PositionsWidest{Class: coord.ConversationHoldClass,
		Key: coord.ConversationHoldKey(seat, held.Conversation), Bound: coord.SmallValueBytes, Record: held,
		Write: func(ctx context.Context, f coord.Fleet) error {
			_, err := f.CreateConversationHold(ctx, held)
			return err
		},
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := held
			past.Conversation += "<"
			_, err := f.CreateConversationHold(ctx, past)
			return err
		}})

	request := []byte(`"` + plain(coord.MaxGrantRequestBytes-2) + `"`)
	add(PositionsWidest{Class: coord.GrantRequestClass, Key: coord.GrantRequestKey("abcdefghij"),
		Bound: coord.MaxGrantRequestBytes, Record: request,
		Write: func(ctx context.Context, f coord.Fleet) error {
			return f.PutGrantRequest(ctx, "abcdefghij", request)
		},
		Past: func(ctx context.Context, f coord.Fleet) error {
			return f.PutGrantRequest(ctx, "abcdefghik", append([]byte{' '}, request...))
		}})

	retry := coord.ModelRetry{Key: escaped(coord.MaxModelEntryBytes), Mode: coord.RetryProbe, At: widestAt,
		By: actor, ByKind: iam.ActorOperator, OperatorID: credential}
	add(PositionsWidest{Class: coord.ModelRetryClass, Key: coord.ModelRetryKey(retry.Key),
		Bound: coord.SmallValueBytes, Record: retry,
		Write: func(ctx context.Context, f coord.Fleet) error {
			_, err := f.PutModelRetry(ctx, retry)
			return err
		},
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := retry
			past.Key += "<"
			_, err := f.PutModelRetry(ctx, past)
			return err
		}})

	check := coord.ModelCheck{ID: seat, Entry: escaped(coord.MaxModelEntryBytes), Node: plain(coord.MaxNodeIDBytes),
		Draft: true, Smoke: true, StartedAt: widestAt, Deadline: widestAt.Add(time.Hour),
		EndedAt: widestAt.Add(time.Minute), Answer: storedJSON(coord.MaxModelCheckAnswerBytes),
		By: actor, ByKind: iam.ActorOperator, OperatorID: credential}
	add(PositionsWidest{Class: coord.ModelCheckClass, Key: coord.ModelCheckKey(check.Entry),
		Bound: coord.MaxModelCheckBytes, Record: check,
		Write: func(ctx context.Context, f coord.Fleet) error {
			_, _, err := f.CreateModelCheck(ctx, check)
			return err
		},
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := check
			past.Entry += "<"
			_, _, err := f.CreateModelCheck(ctx, past)
			return err
		}})

	accepted := coord.MCPAcceptedTool{Server: escaped(coord.MaxMCPServerNameBytes),
		Tool: escaped(coord.MaxMCPToolNameBytes), Digest: strings.Repeat("f", 64), Auto: true,
		By: escaped(coord.MaxMCPAcceptedByBytes), At: widestAt, Title: escaped(coord.MaxMCPToolTitleBytes),
		Description: escaped(coord.MaxMCPToolDescriptionBytes),
		Hints: coord.MCPToolHints{ReadOnly: coord.HintUnknown, Destructive: coord.HintUnknown,
			Idempotent: coord.HintUnknown, OpenWorld: coord.HintUnknown},
		Parts: coord.MCPToolParts{Title: strings.Repeat("f", coord.MCPToolPartLength),
			Description: strings.Repeat("f", coord.MCPToolPartLength),
			Schema:      strings.Repeat("f", coord.MCPToolPartLength),
			Hints:       strings.Repeat("f", coord.MCPToolPartLength)}}
	add(PositionsWidest{Class: coord.MCPAcceptedClass, Key: coord.MCPAcceptedKey(accepted.Server, accepted.Tool),
		Bound: coord.SmallValueBytes, Record: accepted,
		Write: func(ctx context.Context, f coord.Fleet) error {
			_, _, err := f.PutMCPAccepted(ctx, accepted, 0)
			return err
		},
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := accepted
			past.Server += "<"
			_, _, err := f.PutMCPAccepted(ctx, past, 0)
			return err
		}})

	restart := coord.MCPRestart{Server: escaped(coord.MaxMCPServerNameBytes), Seat: seat, At: widestAt,
		By: actor, ByKind: iam.ActorOperator, OperatorID: credential}
	add(PositionsWidest{Class: coord.MCPRestartClass, Key: coord.MCPRestartKey(restart.Entry()),
		Bound: coord.SmallValueBytes, Record: restart,
		Write: func(ctx context.Context, f coord.Fleet) error {
			_, err := f.PutMCPRestart(ctx, restart)
			return err
		},
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := restart
			past.Server += "<"
			_, err := f.PutMCPRestart(ctx, past)
			return err
		}})

	ssh := coord.SSHTest{Host: plain(coord.MaxSSHHostBytes),
		Authority:  "SHA256:" + plain(coord.MaxSSHAuthorityBytes-len("SHA256:")),
		HostDigest: plain(coord.MaxSSHAuthorityBytes), At: widestAt, By: actor, ByKind: iam.ActorOperator,
		OperatorID: credential}
	for range 8 {
		ssh.Steps = append(ssh.Steps, coord.SSHTestStep{ID: plain(64),
			Detail: coord.FitText(escaped(coord.MaxSSHTestDetailBytes), coord.MaxSSHTestDetailBytes)})
	}
	add(PositionsWidest{Class: coord.SSHTestClass,
		Key: coord.SSHTestKey(coord.SSHTestEntry(ssh.Host, ssh.Authority)), Bound: coord.SmallValueBytes,
		Record: ssh,
		Write: func(ctx context.Context, f coord.Fleet) error {
			_, err := f.PutSSHTest(ctx, ssh)
			return err
		},
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := ssh
			past.Host += "a"
			_, err := f.PutSSHTest(ctx, past)
			return err
		}})

	report := coord.IssueReport{Fingerprint: strings.Repeat("f", 64),
		Class: "a." + plain(coord.MaxCatalogueClassBytes-2), Status: coord.IssueReportDuplicate,
		FirstSent: widestAt, ClaimedAt: widestAt, Issue: escaped(coord.MaxIssueReportAddressBytes),
		Payload: storedJSON(coord.MaxIssueReportPayloadBytes)}
	anyOpen := func(coord.IssueReport) bool { return true }
	add(PositionsWidest{Class: coord.IssueReportClass, Key: coord.IssueReportKey(report.Fingerprint),
		Bound: coord.SmallValueBytes, Record: report,
		Write: func(ctx context.Context, f coord.Fleet) error {
			_, _, err := f.ClaimIssueReport(ctx, report, anyOpen)
			return err
		},
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := report
			past.Fingerprint = strings.Repeat("e", 64)
			past.Class += "a"
			_, _, err := f.ClaimIssueReport(ctx, past, anyOpen)
			return err
		}})

	insight := coord.InsightRecord{Class: "a." + plain(coord.MaxCatalogueClassBytes-2), Week: "2026-W48",
		Status: coord.InsightScheduled, Day: "2026-11-24", SendAt: widestAt, Attempts: math.MaxInt,
		ClaimedAt: widestAt, Answer: 599, Payload: storedJSON(coord.MaxInsightPayloadBytes)}
	anyInsight := func(coord.InsightRecord) bool { return true }
	add(PositionsWidest{Class: coord.InsightRecordClass,
		Key: coord.InsightRecordKey(coord.InsightEntry(insight.Class, insight.Week)), Bound: coord.SmallValueBytes,
		Record: insight,
		Write: func(ctx context.Context, f coord.Fleet) error {
			_, _, err := f.ClaimInsightRecord(ctx, insight, anyInsight)
			return err
		},
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := insight
			past.Class += "a"
			_, _, err := f.ClaimInsightRecord(ctx, past, anyInsight)
			return err
		}})

	identity := coord.GoogleIdentity{Handle: plain(coord.MaxActorBytes), UniqueID: escaped(128),
		Email: escaped(coord.MaxEmailBytes), Disabled: true, Delegated: true, CreatedAt: widestAt}
	add(PositionsWidest{Class: coord.GoogleIdentityClass, Key: coord.GoogleIdentityKey(identity.Handle),
		Bound: coord.SmallValueBytes, Record: identity,
		Write: func(ctx context.Context, f coord.Fleet) error {
			_, _, err := f.CreateGoogleIdentity(ctx, identity)
			return err
		},
		Past: func(ctx context.Context, f coord.Fleet) error {
			past := identity
			past.Email += "<"
			_, _, err := f.CreateGoogleIdentity(ctx, past)
			return err
		}})

	for _, fam := range coord.GrantFamilies {
		grant, signIn := widestGrant(), widestSignIn()
		add(PositionsWidest{Class: fam.GrantClass, Key: fam.GrantKey(grant.Name), Bound: coord.MaxOAuthGrantBytes,
			Record: grant,
			Write: func(ctx context.Context, f coord.Fleet) error {
				_, _, err := f.CreateOAuthGrant(ctx, fam, grant)
				return err
			},
			Past: func(ctx context.Context, f coord.Fleet) error {
				past := grant
				past.Scopes = append(append([]string{}, grant.Scopes[1:]...), escaped(coord.MaxGrantScopeBytes+1))
				_, _, err := f.CreateOAuthGrant(ctx, fam, past)
				return err
			}})
		add(PositionsWidest{Class: fam.SignInClass, Key: fam.SignInKey(signIn.Name), Bound: coord.MaxOAuthSignInBytes,
			Record: signIn,
			Write: func(ctx context.Context, f coord.Fleet) error {
				_, _, err := f.CreateOAuthSignIn(ctx, fam, signIn)
				return err
			},
			Past: func(ctx context.Context, f coord.Fleet) error {
				past := signIn
				past.Sealed += "a"
				_, _, err := f.CreateOAuthSignIn(ctx, fam, past)
				return err
			}})
	}
	return out
}

// WidestMaintenanceOperation is a capacity window at its widest: the most
// participants, each recorded, excluded and named by its excluder at their
// longest, and the most journal records, each retired with its evidence whole.
func WidestMaintenanceOperation() coord.MaintenanceOperation {
	actor, credential := plain(coord.MaxActorBytes), plain(coord.MaxActorBytes)
	op := coord.MaintenanceOperation{Stream: plain(coord.MaxStreamNameBytes),
		OperationID: plain(coord.MaxIncarnationBytes), TargetMaxBytes: math.MaxUint64,
		OriginalMaxBytes: math.MaxUint64, Phase: coord.PhaseBaselined, Attempt: math.MaxInt,
		WriteIncarnations: map[string]string{}, ExcludedBy: map[string]coord.MaintenanceParty{},
		ObservedMaxBytes: math.MaxUint64, Blocked: coord.MaintenanceBlock(plain(64)), Outgrown: math.MaxInt,
		AbandonedBy: coord.MaintenanceParty{By: actor, OperatorID: credential}, EnteredAt: widestAt,
		By: actor, OperatorID: credential}
	for i := range coord.MaxMaintenanceParticipants {
		node := numbered(i, coord.MaxNodeIDBytes)
		op.Participants = append(op.Participants, node)
		op.Excluded = append(op.Excluded, node)
		op.WriteIncarnations[node] = plain(coord.MaxIncarnationBytes)
		op.ExcludedBy[node] = coord.MaintenanceParty{By: actor, OperatorID: credential}
	}
	for range coord.MaxMaintenanceJournal {
		op.Journal = append(op.Journal, coord.JournalRecord{Attempt: math.MaxInt,
			By: plain(coord.MaxIncarnationBytes), State: coord.JournalState(plain(64)), At: widestAt,
			ResolvedAt: widestAt, Evidence: plain(coord.MaxJournalEvidenceBytes)})
	}
	return op
}

// widestGrant is a grant at its widest.
func widestGrant() coord.OAuthGrant {
	g := coord.OAuthGrant{Name: plain(coord.MaxGrantNameBytes),
		Grant: uuid.MustParse("01938a6e-7a3e-7cd1-9b2a-0f1e2d3c4b5b"), Client: plain(16),
		ClientID: escaped(coord.MaxGrantAddressBytes), Issuer: escaped(coord.MaxGrantAddressBytes),
		Resource: escaped(coord.MaxGrantAddressBytes), By: plain(coord.MaxActorBytes),
		ByKind: iam.ActorOperator, OperatorID: plain(coord.MaxActorBytes), At: widestAt,
		Expiry: &widestAt, Failure: escaped(coord.MaxGrantWords), FailedAt: &widestAt}
	for range coord.MaxGrantScopes {
		g.Scopes = append(g.Scopes, escaped(coord.MaxGrantScopeBytes))
	}
	for range coord.MaxGrantAccounts {
		g.Accounts = append(g.Accounts, escaped(coord.MaxGrantAccount))
	}
	return g
}

// widestSignIn is a sign-in at its widest.
func widestSignIn() coord.OAuthSignIn {
	return coord.OAuthSignIn{ID: uuid.MustParse("01938a6e-7a3e-7cd1-9b2a-0f1e2d3c4b5c"),
		Name: plain(coord.MaxGrantNameBytes), Status: coord.OAuthSignInCompleting,
		Detail: escaped(coord.MaxGrantWords), StartedAt: widestAt, ExpiresAt: widestAt.Add(time.Hour),
		Sealed: plain(coord.MaxOAuthSealedBytes), By: plain(coord.MaxActorBytes), ByKind: iam.ActorOperator,
		OperatorID: plain(coord.MaxActorBytes)}
}

// widestPositionsCases hold every backend to each positions family's widest
// record: written whole, and with one field past its bound refused before
// anything is stored, so a twin and the KV backend agree on every record the
// register's classes take.
//
// Mutation: drop a field's bound from its record's Validate (a hold's reason,
// a window's participants), and its past case is written.
var widestPositionsCases = []fleetCase{
	{"every positions family's widest record is written, and one past a field's bound is refused", func(h *fleetHarness) {
		for _, w := range WidestPositionsRecords() {
			if err := w.Write(h.ctx, h.f); err != nil {
				h.t.Errorf("%s's widest record, %d bytes as stored = %v, want it written", w.Class, w.Stored(), err)
			}
			if err := w.Past(h.ctx, h.f); err == nil {
				h.t.Errorf("%s's record with a field past its bound was written, want it refused", w.Class)
			}
		}
	}},
}
