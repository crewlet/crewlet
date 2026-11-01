package llm_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// modelVendorSDKs are the Go modules that reach a model over its vendor's
// HTTP API: the vendors' own SDKs, the community clients that stand in for
// them, and the cloud SDK packages that front the same models.
var modelVendorSDKs = []string{
	"github.com/anthropics/",
	"github.com/openai/openai-go",
	"github.com/sashabaranov/go-openai",
	"google.golang.org/genai",
	"github.com/google/generative-ai-go",
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime",
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentruntime",
	"cloud.google.com/go/vertexai",
	"cloud.google.com/go/aiplatform",
	"github.com/cohere-ai/",
	"github.com/gage-technologies/mistral-go",
	"github.com/ollama/ollama",
	"github.com/tmc/langchaingo",
}

// embeddingsClient is the one place a model vendor's SDK is imported, and
// the one SDK it may import: no harness computes an embedding (ADR-0045).
const (
	embeddingsClient = "internal/providers/embeddings"
	embeddingsSDK    = "github.com/openai/openai-go"
)

// NO MODEL IS REACHED OVER A VENDOR'S HTTP API. Every providers.llm entry is
// a harness, and a client to a vendor's API beside it is the shape
// ADR-0045 records the cost of: a second, permanently trailing copy of the
// vendor's own harness, and an entry whose behaviour depends on which of two
// backends it named. So no package imports a model vendor's SDK, except the
// embeddings client importing the OpenAI one.
//
// The subject is the IMPORT, read with go/parser, so a doc comment naming a
// vendor is not a hit and a renamed import still is. It cannot see a client
// written by hand over net/http — that is a review question, and the
// package doc says so where the next reader looks.
func TestNoModelIsReachedOverAVendorsHTTPAPI(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	files, embeddingImports := 0, 0
	for _, dir := range []string{"internal", "cmd"} {
		err := sourcetree.Walk(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			files++
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			for _, spec := range file.Imports {
				imported, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				sdk, ok := vendorSDK(imported)
				if !ok {
					continue
				}
				if sdk == embeddingsSDK && filepath.ToSlash(filepath.Dir(rel)) == embeddingsClient {
					embeddingImports++
					continue
				}
				t.Errorf("%s imports %s, a model vendor's SDK: every model is reached "+
					"through its vendor's harness as a providers.llm entry, and only "+
					"%s imports %s (ADR-0045)", rel, imported, embeddingsClient, embeddingsSDK)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// A guard asserting an absence passes identically when the thing is
	// absent and when the walk read nothing. The embeddings client's own
	// import is the presence that tells the two apart.
	if files == 0 || embeddingImports == 0 {
		t.Fatalf("read %d Go files and found the embeddings client's import %d times — "+
			"the walk is not reaching the tree, so its silence certifies nothing",
			files, embeddingImports)
	}
}

// vendorSDK reports which entry of [modelVendorSDKs] an import path falls
// under, matched on a path boundary so a module that merely shares a prefix
// is not one.
func vendorSDK(imported string) (string, bool) {
	for _, sdk := range modelVendorSDKs {
		switch {
		case strings.HasSuffix(sdk, "/") && strings.HasPrefix(imported, sdk),
			imported == sdk,
			strings.HasPrefix(imported, sdk+"/"):
			return sdk, true
		}
	}
	return "", false
}
