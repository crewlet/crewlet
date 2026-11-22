package main

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// operation is one method of Apple's client.
type operation struct {
	ID, Method, Path string
}

var (
	annotation = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`@(GET|POST|PUT|PATCH|DELETE)\("([^"]*)"\)`)
	})
	method = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`Call<[^\n]*?>\s+([A-Za-z0-9_]+)\s*\(`)
	})
)

// readClient reads every Retrofit-annotated method of src: its HTTP method,
// its path below /v1, and the Java method's name, Apple's operation ID.
func readClient(src string) ([]operation, error) {
	var out []operation
	for _, m := range annotation().FindAllStringSubmatchIndex(src, -1) {
		verb, path := src[m[2]:m[3]], src[m[4]:m[5]]
		rest := src[m[1]:]
		name := method().FindStringSubmatch(rest)
		if name == nil {
			return nil, fmt.Errorf("specgen: no method follows @%s(%q)", verb, path)
		}
		out = append(out, operation{ID: name[1], Method: verb, Path: "/v1/" + strings.TrimPrefix(path, "/")})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("specgen: the file holds no Retrofit operation")
	}
	return out, nil
}
