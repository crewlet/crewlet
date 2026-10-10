package stream

// Routed reports whether the delivery table names kind at all — a push, the
// handshake or a reply — for TestEveryPushKindHasAReach, which holds the table
// total over every Kind constant this package declares.
func Routed(kind Kind) bool {
	_, ok := kinds[kind]
	return ok
}
