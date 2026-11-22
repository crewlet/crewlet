// Command specgen holds the operation table to Apple's own client, the MIT
// apple/apple-ads-platform-api-java's src/main/java/com/apple/ads/api/AppleAdsApi.java,
// whose Retrofit annotations give every operation's method and path:
//
//	go run ./internal/appleads/specgen -client AppleAdsApi.java check
//
// check lists the operations the client adds to the table, which each need a
// class, those the table has and the client no longer does, and any whose ID
// differs; it fails while any is listed.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/crewlet/crewlet/internal/appleads"
)

func main() {
	path := flag.String("client", "", "Apple's AppleAdsApi.java")
	flag.Parse()
	if *path == "" || flag.NArg() != 1 || flag.Arg(0) != "check" {
		fmt.Fprintln(os.Stderr, "usage: specgen -client AppleAdsApi.java check")
		os.Exit(2)
	}
	src, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ops, err := readClient(string(src))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	have := map[string]string{}
	for _, op := range appleads.Catalog().Operations {
		have[op.Method+" "+op.Path] = op.ID
	}
	report := diff(ops, have)
	for _, line := range report {
		fmt.Println(line)
	}
	if len(report) > 0 {
		os.Exit(1)
	}
	fmt.Printf("the table holds all %d operations of Apple's client\n", len(ops))
}

// diff is every disagreement between the client's operations and the table,
// keyed "METHOD /v1/path" to the operation ID, sorted.
func diff(client []operation, table map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, op := range client {
		k := op.Method + " " + op.Path
		seen[k] = true
		switch id, ok := table[k]; {
		case !ok:
			out = append(out, "added: "+k+" ("+op.ID+")")
		case id != op.ID:
			out = append(out, "renamed: "+k+" is "+op.ID+", the table says "+id)
		}
	}
	for k, id := range table {
		if !seen[k] {
			out = append(out, "gone: "+k+" ("+id+")")
		}
	}
	sort.Strings(out)
	return out
}
