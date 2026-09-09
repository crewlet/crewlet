package config_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// THE WIDTH IS A PROPERTY OF THE MODEL, and this is the inversion: an unset
// `dimensions` resolves from the model rather than from a constant.
//
// The default this replaces justified itself by a case that could not exist —
// it was documented as "what an entry that names no model gets", while
// validation refuses exactly that entry. What it actually did was give every
// company 1536 whatever they had named, so a company on
// text-embedding-3-large stored half a vector's worth of information and
// nothing said so.
func TestAnUnsetWidthComesFromTheModel(t *testing.T) {
	t.Parallel()
	for model, want := range map[string]int{
		"text-embedding-3-large": 3072,
		"text-embedding-3-small": 1536,
		"gemini-embedding-001":   3072,
		"embed-v4.0":             1536,
	} {
		t.Run(model, func(t *testing.T) {
			e := &config.EmbeddingProvider{Type: config.EmbeddingOpenAI, Model: model}
			if got := e.Width(); got != want {
				t.Errorf("Width() = %d, want %d", got, want)
			}
		})
	}

	// AN EXPLICIT WIDTH IS AN OVERRIDE, for a model that shortens on
	// request.
	e := &config.EmbeddingProvider{
		Type: config.EmbeddingOpenAI, Model: "text-embedding-3-large", Dimensions: 1024,
	}
	if got := e.Width(); got != 1024 {
		t.Errorf("an explicit width read back as %d", got)
	}
}

// AN UNKNOWN MODEL WITH NO WIDTH IS REFUSED, not guessed at.
//
// The width decides whether a vector written today can be read tomorrow, so a
// guess is a company whose recall silently stops working the day it is turned
// on. The refusal names both ways out.
func TestAnUnknownModelWithNoWidthIsRefused(t *testing.T) {
	t.Parallel()
	c := embeddingCompany(t, &config.EmbeddingProvider{
		Type: config.EmbeddingOpenAI, Model: "some-new-model-v9",
	})
	err := c.Validate()
	if err == nil {
		t.Fatal("an unknown model with no width was accepted")
	}
	for _, want := range []string{"some-new-model-v9", "dimensions", "text-embedding-3-large"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to say %q", err, want)
		}
	}

	// AND STATING THE WIDTH IS THE WAY THROUGH, which is what makes the
	// refusal a question rather than a wall.
	c = embeddingCompany(t, &config.EmbeddingProvider{
		Type: config.EmbeddingOpenAI, Model: "some-new-model-v9", Dimensions: 2048,
	})
	if err := c.Validate(); err != nil {
		t.Fatalf("an unknown model WITH a width was refused: %v", err)
	}
}

// THE EXPLICIT WIDTH IS BOUNDED, and the floor cannot be expressed in the
// schema — "0 or 64..4096" is not a JSON Schema range — so the published
// schema carries 0..4096 and the floor lives in the validator, in a message
// naming both forms.
func TestAnExplicitWidthIsBounded(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		width  int
		accept bool
		says   string
	}{
		"unset takes the model's": {0, true, ""},
		"at the floor":            {64, true, ""},
		"below the floor":         {32, false, "leave it unset"},
		"a sensible override":     {1024, true, ""},
		"at the ceiling":          {4096, true, ""},
		"past the ceiling":        {4097, false, "widest any model"},
		"negative":                {-1, false, "own width"},
	} {
		t.Run(name, func(t *testing.T) {
			c := embeddingCompany(t, &config.EmbeddingProvider{
				Type: config.EmbeddingOpenAI, Model: "text-embedding-3-large",
				Dimensions: tc.width,
			})
			err := c.Validate()
			if tc.accept {
				if err != nil {
					t.Fatalf("width %d was refused: %v", tc.width, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("width %d was accepted", tc.width)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("refusal = %q, want it to say %q", err, tc.says)
			}
		})
	}
}

func embeddingCompany(t *testing.T, e *config.EmbeddingProvider) *config.Company {
	t.Helper()
	c := config.DefaultCompany()
	c.Name = "Acme"
	c.Providers.Embeddings = e
	return &c
}

// AND THE CONSTANT IS GONE FROM THE TREE.
//
// A default width that still exists somewhere is one something still reads,
// and the whole point of this change is that there is no answer to "how wide
// are vectors" that does not name a model. A grep is the only thing that can
// say so: a deleted constant leaves no compile error behind if a copy was
// made.
func TestNoDefaultWidthConstantSurvives(t *testing.T) {
	t.Parallel()
	root := moduleRootFor(t)
	var found []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		body, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		// This file names it to assert its absence, which is the one
		// legitimate occurrence.
		if strings.HasSuffix(p, "embedwidth_test.go") {
			return nil
		}
		if strings.Contains(string(body), "defaultEmbeddingDimensions") {
			rel, _ := filepath.Rel(root, p)
			found = append(found, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("defaultEmbeddingDimensions survives in %v — a default width is "+
			"a width nobody named, and it is what this change removed", found)
	}
}

func moduleRootFor(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source file")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected the module root at %s: %v", root, err)
	}
	return root
}
