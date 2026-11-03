package builtin

// EncodeAnswer is [encodeAnswer], so the cases that weigh a tool answer weigh
// the bytes a reader is actually handed rather than another encoder's.
var EncodeAnswer = encodeAnswer

// CodeStatusJudges is how many grant reads one code_status call runs at once,
// for the cases that hold a call to it.
const CodeStatusJudges = codeStatusJudges
