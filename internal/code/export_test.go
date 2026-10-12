package code

// SetPageLimits has r end every page at rows rows or bytes of encoded rows —
// the cases that read a snapshot across several pages without seeding tens of
// thousands of rows. The shipped limits are MaxSnapshotItems and MaxPageBytes.
func SetPageLimits(r *Reader, rows, bytes int) { r.pageRows, r.pageBytes = rows, bytes }
