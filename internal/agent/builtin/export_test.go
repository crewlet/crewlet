package builtin

// EncodeAnswer is [encodeAnswer], so the cases that weigh a tool answer weigh
// the bytes a reader is actually handed rather than another encoder's.
var EncodeAnswer = encodeAnswer
