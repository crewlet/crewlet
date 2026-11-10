package engine

import "github.com/crewlet/crewlet/internal/backup"

// BackupOptions is this node's backup service as the engine alone can wire it,
// so `crewlet run` and every suite standing a node up build the one service.
// Logs is the register's, so a domain registered later is in every manifest.
func (e *Engine) BackupOptions() backup.Options {
	return backup.Options{
		Store: e.backends.Store,
		// WHAT THIS NODE HOLDS, never whether the replicated estate is open,
		// which it is not while an adoption replaces it.
		Estate: backup.HoldingFor(HoldsEstate(e.boot)),
		// Nil on a node that dialled an external cluster, whose streams are
		// backed up there.
		Conn: e.backends.Conn(),
		API:  e.backends.API(),
		// The trim-hold register, and where a finished copy is announced: the
		// one fleet store the trim's backup term and the backup alarm read.
		Holds:   e.backends.Fleet,
		Backups: e.backends.Fleet,
		// The resolved id, never the raw node.id field: it keys this node's
		// trim hold and its announced backup point.
		NodeID: e.Node().ID(),
		Objects: &backup.Objects{Open: e.OpenObject, Stat: e.StatObject,
			Stream: e.ObjectsStream()},
		// The keyring the appliers open every record with, since the records a
		// restore replays past the copy are read for the files they name.
		Keyring: e.RecordKeyring(),
		Metrics: e.metrics,
		Logs:    registeredLogs(),
	}
}
