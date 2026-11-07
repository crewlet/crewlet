package store

import turso "turso.tech/database/tursogo"

// ErrFull is a statement the driver refused because the database file could
// not grow: the disk under it has no room. The driver's own mark, so every path
// that carries its error carries this, and a caller answers it as a condition
// of the host rather than a fault of the engine.
var ErrFull = turso.ErrTursoDatabaseFull
