package artifactsdomain_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// TWO NODES WRITING ONE LOG AT ONCE CONVERGE, BYTE FOR BYTE, and a third node
// replaying the log from nothing rebuilds the same estate.
//
// This is the identity claim the domain declares ([artifactsdomain.Domain]
// claims identity), held across the cases that make it hard: creates, publishes
// that race on one artifact from both nodes, carries, retention, shares and
// narrowings, assets, pins and a purge, all through two independent publishers
// and appliers. Every Replicated table is compared whole.
//
// Mutation: write the applying node's own clock into any row (time.Now for a
// history row's instant), or the capability list from an unsorted map, and the
// two nodes' tables differ.
func TestTwoNodesConvergeOnOneLogAndAReplayRebuildsTheirEstate(t *testing.T) {
	t.Parallel()
	log := openLog(t)
	a := newNode(t, log, openStore(t, "a.db"), "node-a")
	b := newNode(t, log, openStore(t, "b.db"), "node-b")

	jane, bob := person("jane", artifacts.RoleOwner), person("bob", artifacts.RoleOwner)
	shared := a.create(jane, "Shared target", upload(t, "index.html", "<p>shared</p>"),
		upload(t, "style.css", "p{color:red}")).Artifact
	b.drain()
	if _, err := a.writer.Share(t.Context(), jane, shared, artifacts.Share{
		Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "bob"}, Role: artifacts.RoleEditor,
	}); err != nil {
		t.Fatalf("share: %v", err)
	}
	b.drain()

	var wg sync.WaitGroup
	failures := make(chan error, 64)
	run := func(n *node, c artifactsdomain.Call, label string) {
		defer wg.Done()
		ctx := context.Background()
		own := n.create(c, label, upload(t, "index.html", "<p>"+label+"</p>")).Artifact
		for i := range 5 {
			// RACE ON THE SHARED ARTIFACT, carrying its stylesheet: each
			// node re-reads the head and retries on a moved one.
			for {
				head, _, err := n.reader.Artifact(ctx, shared, statelog.Freshness{Level: statelog.ReadSession})
				if err != nil {
					failures <- err
					return
				}
				v, err := n.reader.Version(ctx, shared, head.HeadNumber)
				if err != nil {
					failures <- err
					return
				}
				css := v.Manifest.Files[1]
				in := page(upload(t, "index.html", fmt.Sprintf("<p>%s %d</p>", label, i)), carried(css))
				in.Artifact, in.Base = shared, head.HeadNumber
				_, err = n.writer.Publish(ctx, c, in)
				var moved *artifactsdomain.VersionMoved
				if errors.As(err, &moved) {
					n.drain()
					continue
				}
				if err != nil {
					failures <- fmt.Errorf("%s publish %d: %w", label, i, err)
					return
				}
				break
			}
			in := page(upload(t, "index.html", fmt.Sprintf("<p>own %s %d</p>", label, i)))
			in.Artifact, in.Base = own, i+1
			if _, err := n.writer.Publish(ctx, c, in); err != nil {
				failures <- fmt.Errorf("%s own publish %d: %w", label, i, err)
				return
			}
		}
		if _, _, err := n.writer.AddAsset(ctx, c, artifactsdomain.AssetInput{
			Artifact: own, Name: "logo.png", Object: upload(t, "logo.png", "png of "+label).Object,
		}); err != nil {
			failures <- fmt.Errorf("%s asset: %w", label, err)
			return
		}
		if _, err := n.writer.Pin(ctx, c, shared, true); err != nil {
			failures <- fmt.Errorf("%s pin: %w", label, err)
			return
		}
		if _, err := n.writer.Opened(ctx, c, shared); err != nil {
			failures <- fmt.Errorf("%s opened: %w", label, err)
		}
	}
	wg.Add(2)
	go run(a, jane, "jane")
	go run(b, bob, "bob")
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}

	// A NARROWING AND A PURGE, after the race.
	if _, err := a.writer.Share(t.Context(), jane, shared, artifacts.Share{
		Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "bob"}, Role: artifacts.RoleViewer,
	}); err != nil {
		t.Fatalf("narrow: %v", err)
	}
	doomed := b.create(bob, "Doomed", upload(t, "index.html", "<p>doomed</p>")).Artifact
	if _, err := b.writer.Purge(t.Context(), bob, doomed); err != nil {
		t.Fatalf("purge: %v", err)
	}
	a.drain()
	b.drain()

	if h := a.head(shared); h.HeadNumber != 11 || h.AccessEpoch != 1 {
		t.Errorf("the shared artifact is at %d with epoch %d, want 11 and 1", h.HeadNumber, h.AccessEpoch)
	}
	replay := newNode(t, log, openStore(t, "replay.db"), "node-c")
	replay.drain()

	for _, table := range replicatedTables() {
		want := a.table(table)
		if got := b.table(table); got != want {
			t.Errorf("%s differs between the two writing nodes:\n a: %s\n b: %s", table, want, got)
		}
		if got := replay.table(table); got != want {
			t.Errorf("%s differs on the node that replayed the log from nothing:\n a: %s\n c: %s",
				table, want, got)
		}
	}
	if a.table("artifacts") == "" || a.table("artifact_objects") == "" {
		t.Fatal("the compared estate is empty, so the comparison proves nothing")
	}
}

// replicatedTables is every table the domain claims identity over.
func replicatedTables() []string {
	var out []string
	for name, class := range (artifactsdomain.Domain{}).Tables() {
		if class == statelog.Replicated {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
