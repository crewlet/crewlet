package doclet_test

import (
	"slices"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/doclet"
)

// THE DASHBOARD BOUNDS A PAGE'S TEXT WHERE THE ENGINE DOES.
//
// Every form that names or writes a page (the composer, a folder named in
// place, the inbox's "save as a page") and the editor's save note refuse a
// value past the engine's cap before the press, saying by how much, because
// the engine refuses such a value rather than cutting it: a title, a body, a
// save's note, and how many labels and how long each. The fields used to carry `maxLength={200}`,
// which the browser applies by cutting a paste silently, in characters,
// short of the engine's 256 bytes. That only works while the two figures
// agree: above the engine's, a title the engine refuses gets through; below
// it, one the engine would take is refused.
func TestTheDashboardBoundsPageTextAtTheEnginesCaps(t *testing.T) {
	t.Parallel()
	for name, engine := range map[string]int{
		"PAGE_TITLE_MAX_BYTES":   doclet.MaxTitle,
		"PAGE_MESSAGE_MAX_BYTES": doclet.MaxMessage,
		"PAGE_BODY_MAX_BYTES":    doclet.MaxBody,
		"PAGE_LABELS_MAX":        doclet.MaxLabels,
		"PAGE_LABEL_MAX_BYTES":   doclet.MaxLabelLength,
	} {
		raw, err := clientsource.Scalar(clientsource.Tree(t), name)
		if err != nil {
			t.Fatal(err)
		}
		client, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("%s is %q, which is not an integer: %v", name, raw, err)
		}
		if client != engine {
			t.Errorf("the dashboard bounds %s at %d bytes and the engine at %d — "+
				"change it in contract/pages.ts to the engine's figure", name, client, engine)
		}
	}
}

// EVERY CHANGE THE ENGINE WRITES TO A PAGE READS AS A SENTENCE ON THE CLIENT.
//
// The company's feed and a page's own history draw each change as words
// after the writer's name, from `PAGE_CHANGES`: a verb before the page's title
// on the feed, a phrase on the page itself. A kind the engine writes that the
// dashboard does not name reads as the bare kind ("Ana comment_edited"), and a
// kind the dashboard names that the engine never writes is a sentence nothing
// can reach. Both directions are held, and a row that lost half of its words
// is caught too.
func TestEveryPageChangeTheEngineWritesReadsAsASentence(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "PAGE_CHANGES")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Field(body, "kind")
	verbs := clientsource.Field(body, "verb")
	phrases := clientsource.Field(body, "phrase")
	if len(client) == 0 {
		t.Fatal("the dashboard names no page change kinds at all, so this gate certifies nothing")
	}
	if len(verbs) != len(client) || len(phrases) != len(client) {
		t.Errorf("the dashboard names %d page change kinds, %d verbs and %d phrases: "+
			"every kind carries both, so a row is missing one of them",
			len(client), len(verbs), len(phrases))
	}
	for _, kind := range doclet.ChangeKinds() {
		if !slices.Contains(client, string(kind)) {
			t.Errorf("the engine writes the page change kind %q and the dashboard has no "+
				"words for it: the feed and the page's history draw it as the bare kind. "+
				"The dashboard names %v", kind, client)
		}
	}
	for _, name := range client {
		if !doclet.ChangeKind(name).Valid() {
			t.Errorf("the dashboard words the page change kind %q, which this engine "+
				"never writes: the sentence is unreachable", name)
		}
	}
}

// THE DASHBOARD ASKS FOR A LISTING IN EXACTLY THE ORDERS THE ENGINE READS.
//
// Agent skills offers the tool skills by name and newest first, and Home's
// Agent skills widget asks for the newest, each as the `order` a `pages`
// question carries, from `PAGE_LIST_ORDERS`. An order the dashboard sends that
// the engine does not read is no mis-sorted list: the question refuses it as
// bad params and the whole screen goes with it. An order the engine grew that
// the dashboard does not name is one nobody can ask for. Both directions are
// held.
func TestTheDashboardAsksForExactlyTheOrdersAListingIsReadIn(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "PAGE_LIST_ORDERS")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	if len(client) == 0 {
		t.Fatal("the dashboard names no listing order at all, so this gate certifies nothing")
	}
	for _, name := range client {
		if !doclet.ListOrder(name).Valid() {
			t.Errorf("the dashboard asks for a listing in the order %q, which the engine "+
				"does not read: the question is refused and the screen with it. The "+
				"engine reads %v", name, doclet.ListOrders())
		}
	}
	for _, order := range doclet.ListOrders() {
		if !slices.Contains(client, string(order)) {
			t.Errorf("the engine reads a listing in the order %q and the dashboard does "+
				"not name it, so nobody can ask for it. The dashboard names %v",
				order, client)
		}
	}
}
