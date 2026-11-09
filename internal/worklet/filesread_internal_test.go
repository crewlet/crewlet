package worklet

import "testing"

// A FILE PAGE IS HELD BETWEEN THE DEFAULT AND THE CAP: none asked is the
// default page, and more than the cap is the cap, so a listing never answers
// more rows than one answer carries, whatever a caller sends.
//
// Mutation: answer what was asked past MaxFilePage and the cap rows fail;
// answer it below one and the default rows fail.
func TestAFilePageIsHeldBetweenTheDefaultAndTheCap(t *testing.T) {
	t.Parallel()
	for asked, want := range map[int]int{
		-1: DefaultFilePage, 0: DefaultFilePage, 1: 1, 7: 7,
		MaxFilePage: MaxFilePage, MaxFilePage + 1: MaxFilePage, 5000: MaxFilePage,
	} {
		if got := filePage(asked); got != want {
			t.Errorf("a listing asked for %d files answers a page of %d, want %d",
				asked, got, want)
		}
	}
}
