package toolloop_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/tools"
)

// cropTool is a builtin that answers a comment's crop as a seat tool does: its
// text names whose picture it is, and the picture rides beside it.
type cropTool struct {
	text   string
	images func() ([]crewletmcp.Image, error)
}

func (cropTool) Name() string        { return "read_comment" }
func (cropTool) Description() string { return "reads a comment and the crop its writer attached" }
func (cropTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (c cropTool) Call(context.Context, map[string]any) (tools.Result, error) {
	images, err := c.images()
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Output: c.text, Images: images}, nil
}

// cropPNG is a small PNG, encoded as a tool that crops a page encodes one.
func cropPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 5, 3))
	img.Set(2, 1, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// imagePhase runs one phase whose model calls tool once, over the real tool
// session, on a seat's surface: what the harness was answered, what the phase
// recorded, and what the surface recorded.
func imagePhase(t *testing.T, tool tools.Callable) (llmtest.Result, *toolloop.Result, []tools.Call) {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	surf := tools.NewSurface("execute", reg.Snapshot(), []string{tool.Name()}).
		ForTurn(&turnctx.Turn{RunID: "run-1", Calls: turnctx.NewCallLog()})
	prov := &llmtest.Script{Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Calls: []llmtest.Call{{Name: tool.Name()}}},
		{Text: "done"},
	}}}}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := prov.Results()
	if len(got) != 1 {
		t.Fatalf("the harness was answered %d calls, want one", len(got))
	}
	return got[0], res, surf.Calls()
}

// A BUILTIN'S PICTURE REACHES THE HARNESS BESIDE ITS TEXT, AND NO RECORD KEEPS
// IT. A seat tool answering text and one image is answered, over the phase's
// real session, as one text block and then one image block holding the tool's
// bytes; the phase's execution and the surface's call keep the text as the
// model read it and the image's type and size, and nothing either encodes
// holds the picture.
//
// Mutation: answer the harness from Output alone (toolloop's dispatcher or the
// session's server), and the image block is missing; write the picture into the
// text a record keeps, and the records hold its bytes.
func TestABuiltinsPictureReachesTheHarnessAndNoRecordKeepsIt(t *testing.T) {
	t.Parallel()
	data := cropPNG(t)
	const said = "Comment by ana on the chart, with the crop of the region she marked."
	answered, res, calls := imagePhase(t, cropTool{text: said, images: func() ([]crewletmcp.Image, error) {
		img, err := crewletmcp.NewImage(data)
		return []crewletmcp.Image{img}, err
	}})

	if answered.Failed || len(answered.Content) != 2 {
		t.Fatalf("the harness read %d blocks (failed %v): %q, want the text and one image",
			len(answered.Content), answered.Failed, answered.Output)
	}
	if got, ok := answered.Content[0].(*mcp.TextContent); !ok || got.Text != said {
		t.Errorf("the first block is %#v, want the tool's text", answered.Content[0])
	}
	if got, ok := answered.Content[1].(*mcp.ImageContent); !ok || got.MIMEType != crewletmcp.ImagePNG ||
		!bytes.Equal(got.Data, data) {
		t.Errorf("the second block is %#v, want the tool's PNG", answered.Content[1])
	}

	want := []crewletmcp.ImageRecord{{MIMEType: crewletmcp.ImagePNG, Bytes: len(data)}}
	if len(res.Executions) != 1 || res.Executions[0].Output != said ||
		!slices.Equal(res.Executions[0].Images, want) {
		t.Fatalf("the phase recorded %+v, want the text and %+v", res.Executions, want)
	}
	if len(calls) != 1 || calls[0].Output != said || !slices.Equal(calls[0].Images, want) {
		t.Fatalf("the surface recorded %+v, want the text and %+v", calls, want)
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	for name, record := range map[string]any{"execution": res.Executions, "surface call": calls,
		"conversation": res.Messages} {
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, data) || strings.Contains(string(raw), encoded) {
			t.Errorf("the %s record holds the picture's bytes", name)
		}
	}
}

// A PAYLOAD THAT IS NOT A SMALL IMAGE IS REFUSED WHERE THE ANSWER IS TAKEN,
// NEVER SENT. A builtin whose answer carries bytes no image type bears out, or
// a picture past its bound, is answered failed saying why, with no image block,
// and its record says the same failure and keeps no image.
//
// Mutation: drop the check where the surface takes a tool's answer, and the
// harness is handed the bytes.
func TestAPayloadThatIsNotASmallImageIsRefusedAndNeverSent(t *testing.T) {
	t.Parallel()
	oversized := append(cropPNG(t), make([]byte, crewletmcp.MaxImageBytes)...)
	for name, images := range map[string][]crewletmcp.Image{
		"not an image": {{MIMEType: crewletmcp.ImagePNG, Data: []byte("%PDF-1.7 a document, not a picture")}},
		"oversized":    {{MIMEType: crewletmcp.ImagePNG, Data: oversized}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			answered, res, calls := imagePhase(t, cropTool{text: "Comment by ana, with her crop.",
				images: func() ([]crewletmcp.Image, error) { return images, nil }})
			if !answered.Failed || len(answered.Content) != 1 ||
				!strings.Contains(answered.Output, "fault in the tool") {
				t.Errorf("the harness was answered %d blocks (failed %v): %q, want one failure saying why",
					len(answered.Content), answered.Failed, answered.Output)
			}
			if len(res.Executions) != 1 || !res.Executions[0].Failed || res.Executions[0].Images != nil ||
				res.Executions[0].Refusal != crewletmcp.RefusalInternalError {
				t.Errorf("the phase recorded %+v, want the failure and no image", res.Executions)
			}
			if len(calls) != 1 || !calls[0].Failed || calls[0].Images != nil {
				t.Errorf("the surface recorded %+v, want the failure and no image", calls)
			}
		})
	}
}
