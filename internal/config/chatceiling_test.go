package config_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// WHAT AN UNSET CHAT CEILING ASKS FOR IS THE DOMAIN'S NUMBER, and every place
// an operator reads it states that number.
//
// The engine asks for [chatlet.ChatLogMaxBytes] itself, and config cannot import
// the domain, so the figure is written again in words wherever an operator
// meets the field: the Tier A description (which the generated schema carries
// into every editor), the field's own doc, the configuration reference's
// example and the two guides that size the logs. Nothing tied any of them
// to the constant, which is the drift internal/textcut and internal/whsec
// each record the cost of: the engine asking for one number while every page
// an operator can read names another.
func TestTheChatCeilingOperatorsReadIsTheOneTheDomainDeclares(t *testing.T) {
	t.Parallel()
	const gib = 1 << 30
	if chatlet.ChatLogMaxBytes%gib != 0 {
		t.Fatalf("chatlet.ChatLogMaxBytes is %d, not a whole number of GiB, so "+
			"every statement of it below is a rounding", chatlet.ChatLogMaxBytes)
	}
	asks := fmt.Sprintf("%d GiB", chatlet.ChatLogMaxBytes/gib)

	field, ok := reflect.TypeFor[config.Stream]().FieldByName("ChatLogMaxBytes")
	if !ok {
		t.Fatal("config.Stream has no ChatLogMaxBytes field")
	}
	if desc := field.Tag.Get("desc"); !strings.Contains(desc, "unset asks for "+asks) {
		t.Errorf("the Tier A description of chat_log_max_bytes does not say an "+
			"unset value asks for %s, the domain's own figure: %q", asks, desc)
	}
	if doc := streamFieldDoc(t, "ChatLogMaxBytes"); !strings.Contains(doc, "UNSET ASKS FOR "+asks) {
		t.Errorf("the ChatLogMaxBytes doc does not say an unset value asks for "+
			"%s, the domain's own figure:\n%s", asks, doc)
	}

	reference := readShipped(t, "docs", "getting-started", "configuration.md")
	example := regexp.MustCompile(`(?m)^\s*#\s*chat_log_max_bytes:\s*(\d+)\b`).
		FindStringSubmatch(reference)
	if example == nil {
		t.Fatal("the configuration reference no longer shows a chat_log_max_bytes example")
	}
	if got, err := strconv.ParseInt(example[1], 10, 64); err != nil || got != chatlet.ChatLogMaxBytes {
		t.Errorf("the configuration reference's example is chat_log_max_bytes: %s, "+
			"want the domain's own %d", example[1], int64(chatlet.ChatLogMaxBytes))
	}
	if !strings.Contains(reference, "ASKS FOR "+asks) {
		t.Errorf("the configuration reference does not say an unset chat log asks for %s", asks)
	}
	for _, guide := range [][]string{
		{"docs", "guides", "deployment.md"},
		{"docs", "guides", "replication.md"},
	} {
		text := strings.Join(strings.Fields(readShipped(t, guide...)), " ")
		if !strings.Contains(text, "the chat log for "+asks) {
			t.Errorf("%s does not say the chat log asks for %s",
				filepath.Join(guide...), asks)
		}
	}
}

// streamFieldDoc is the doc comment on one config.Stream field, read from the
// source because a comment is not reflectable.
func streamFieldDoc(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(sourcetree.Root(t), "internal", "config", "bootstrap.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var doc string
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name.Name != "Stream" {
			return doc == ""
		}
		st, ok := spec.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, f := range st.Fields.List {
			for _, ident := range f.Names {
				if ident.Name == name && f.Doc != nil {
					doc = f.Doc.Text()
				}
			}
		}
		return false
	})
	if doc == "" {
		t.Fatalf("config.Stream.%s has no doc comment in %s", name, path)
	}
	return doc
}

// readShipped reads a file this repository ships, failing when it cannot: a
// missing page is a rename nobody swept, never a reason to certify nothing.
func readShipped(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{sourcetree.Root(t)}, parts...)...)
	data, err := os.ReadFile(path) //nolint:gosec // a path this repository ships
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
