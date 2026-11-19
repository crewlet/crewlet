package redact

// LookBack is how much of a text a reader that begins at a line holds before
// it, never shown, to show from that line on what [Secrets] of the whole text
// does: it redacts what it holds with the rest, and the rest alone, and shows
// from the line after the last place the two part.
//
// THE READS THAT CROSS A LINE BREAK, SUMMED along a [Stream]'s stages, each in
// its stage's input: a key's block read back from its END, and a password's
// value MaxReach below its key and its label MaxReach back, in the claims the
// keys' blocks read and again past them. Exactness ends at a password's
// sequence and at its reads across what an earlier rule replaced.
const LookBack = keyBehind + 4*stageReach
