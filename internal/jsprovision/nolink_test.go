package jsprovision

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
)

// THIS PACKAGE LINKS NO NATS-SERVER PACKAGE: internal/statelog imports it, so
// every binary that links the state log would link nats-server too, and its
// pse package samples the process every second from init, allocating beside
// whatever the binary measures (#2160). A walk of the package's own files'
// imports, since what a file imports is what the binary links.
//
// Mutation: classify nats-server's *server.ApiError here again — this fails.
func TestThisPackageLinksNoNATSServer(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		for _, spec := range f.Imports {
			if path, _ := strconv.Unquote(spec.Path.Value); strings.HasPrefix(path, "github.com/nats-io/nats-server/") {
				t.Errorf("%s imports %s, which every binary linking internal/statelog would then link", name, path)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("the walk read no file of the package, so it proved nothing")
	}
}

// THE NO-LEADER CODE IS THE SERVER'S: [errCodeNoLeader] is spelled rather than
// read off nats-server's server package, which only this test links, so a
// nats-server bump that renumbers JSClusterNotAvailErr fails here rather than
// leaving every election's answer unread.
func TestTheNoLeaderCodeIsTheServers(t *testing.T) {
	t.Parallel()
	if want := jetstream.ErrorCode(server.JSClusterNotAvailErr); errCodeNoLeader != want {
		t.Errorf("errCodeNoLeader is %d; nats-server answers a group with no leader %d", errCodeNoLeader, want)
	}
}
