package executor

// checkoutBudget is the most bytes of repository instructions a run is handed
// for the checkouts below its working directory, its launch's and its nested
// hook's together, whatever its harness: Codex 0.171.0's own
// project_doc_max_bytes default (32 KiB), the bound a native session holds its
// project docs to and the one such cap measured among the harnesses. A file
// past what is left is cut to it, as Codex cuts one; a checkout found once
// nothing is left is handed nothing.
const checkoutBudget = 32 << 10
