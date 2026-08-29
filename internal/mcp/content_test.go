package mcp

import (
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRenderBlock(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		block Block
		want  string
	}{
		{
			name:  "text is the payload, verbatim",
			block: Block{Type: BlockText, Text: "hi"},
			want:  "hi",
		},
		{
			// The incident: this used to fall into a catch-all that kept only
			// the type, so a tool returning a file as an embedded resource
			// reached the model as the opaque {'type': 'resource'} and left it
			// blind to the contents it had just asked for.
			name: "an embedded TEXT resource surfaces its body",
			block: Block{
				Type: BlockResource, URI: "file:///app/main.py",
				MIME: "text/x-python", Text: "print('hello')\n",
			},
			want: "print('hello')\n",
		},
		{
			name: "a binary resource is described, not inlined",
			block: Block{
				Type: BlockResource, URI: "file:///app/logo.png",
				MIME: "image/png", Blob: true,
			},
			want: "[resource: file:///app/logo.png (image/png)]",
		},
		{
			name:  "a resource with no uri still names its type",
			block: Block{Type: BlockResource, MIME: "application/pdf"},
			want:  "[resource: (application/pdf)]",
		},
		{
			name:  "a resource with nothing at all says unknown",
			block: Block{Type: BlockResource},
			want:  "[resource: (unknown)]",
		},
		{
			name: "a resource link carries name and mime",
			block: Block{
				Type: BlockResourceLink, URI: "https://example/file.txt",
				Name: "file.txt", MIME: "text/plain",
			},
			want: "[resource_link: https://example/file.txt file.txt (text/plain)]",
		},
		{
			name:  "a bare resource link is just the uri",
			block: Block{Type: BlockResourceLink, URI: "https://example/x"},
			want:  "[resource_link: https://example/x]",
		},
		{
			name:  "an image is described by mime",
			block: Block{Type: BlockImage, MIME: "image/png"},
			want:  "[image: image/png]",
		},
		{
			name:  "audio likewise",
			block: Block{Type: BlockAudio, MIME: "audio/wav"},
			want:  "[audio: audio/wav]",
		},
		{
			name:  "an unknown type is named, not dropped",
			block: Block{Type: "*mcp.SomethingNew"},
			want:  "[unsupported content: *mcp.SomethingNew]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.block.Render(); got != tc.want {
				t.Fatalf("Render() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderBlocksJoinsAndDropsEmpties(t *testing.T) {
	t.Parallel()
	got := renderBlocks([]Block{
		{Type: BlockText, Text: "first"},
		{Type: BlockText, Text: ""},
		{Type: BlockText, Text: "second"},
	})
	if got != "first\nsecond" {
		t.Fatalf("renderBlocks = %q; a server padding with blank blocks must not pad the context", got)
	}
	if got := renderBlocks(nil); got != "" {
		t.Fatalf("renderBlocks(nil) = %q", got)
	}
}

func TestErrorTextTakesOnlyWords(t *testing.T) {
	t.Parallel()
	got := errorText([]Block{
		{Type: BlockImage, MIME: "image/png"},
		{Type: BlockText, Text: "permission"},
		{Type: BlockText, Text: "denied"},
	})
	if got != "permission denied" {
		t.Fatalf("errorText = %q: a descriptor must not read as the server's own words", got)
	}
}

func TestBlocksFromSDKContent(t *testing.T) {
	t.Parallel()
	blocks := blocksFrom([]sdk.Content{
		&sdk.TextContent{Text: "hello"},
		&sdk.ImageContent{MIMEType: "image/png", Data: []byte("abc")},
		&sdk.AudioContent{MIMEType: "audio/wav", Data: []byte("def")},
		&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{
			URI: "file:///a.py", MIMEType: "text/x-python", Text: "code",
		}},
		&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{
			URI: "file:///a.png", MIMEType: "image/png", Blob: []byte("bin"),
		}},
		&sdk.ResourceLink{URI: "https://x/y", Name: "y", MIMEType: "text/plain"},
		&sdk.EmbeddedResource{}, // a resource with no body at all
	})
	if len(blocks) != 7 {
		t.Fatalf("got %d blocks, want 7", len(blocks))
	}
	if blocks[0].Text != "hello" {
		t.Errorf("text block = %+v", blocks[0])
	}
	if string(blocks[1].Data) != "abc" || blocks[1].MIME != "image/png" {
		t.Errorf("image block = %+v", blocks[1])
	}
	if string(blocks[2].Data) != "def" {
		t.Errorf("audio block = %+v", blocks[2])
	}
	if blocks[3].Text != "code" || blocks[3].Blob {
		t.Errorf("text resource = %+v: the body must be surfaced and not marked binary", blocks[3])
	}
	if !blocks[4].Blob || blocks[4].Text != "" {
		t.Errorf("blob resource = %+v: a binary body must be flagged, not rendered", blocks[4])
	}
	if blocks[5].Name != "y" || blocks[5].URI != "https://x/y" {
		t.Errorf("resource link = %+v", blocks[5])
	}
	if blocks[6].Blob {
		t.Errorf("an empty resource must not claim a binary body: %+v", blocks[6])
	}
}

func TestToolResultRenderingEndToEnd(t *testing.T) {
	t.Parallel()
	// Every content shape a server can return, over a real pipe, rendered as
	// the agent would see it.
	c := mustConnect(t, helperSpec(t, "blocks", "serve", map[string]string{
		helperCallEnv: "blocks",
	}))
	defs, err := c.listTools(t.Context())
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	tool := newTool(c, defs[0], Spec{Name: "blocks"})

	res, err := tool.Call(t.Context(), nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	want := "lead\n" +
		"print('hello')\n\n" +
		"[resource: file:///app/logo.png (image/png)]\n" +
		"[resource_link: https://example/file.txt file.txt (text/plain)]\n" +
		"[image: image/png]\n" +
		"[audio: audio/wav]"
	if res.Output != want {
		t.Fatalf("rendered:\n%q\nwant:\n%q", res.Output, want)
	}
	if res.Failed {
		t.Fatal("a successful call reported failure")
	}
}

func TestEmptyResultIsNotSilence(t *testing.T) {
	t.Parallel()
	c := mustConnect(t, helperSpec(t, "quiet", "serve", map[string]string{
		helperCallEnv: "empty",
	}))
	defs, err := c.listTools(t.Context())
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	tool := newTool(c, defs[0], Spec{Name: "quiet"})
	res, err := tool.Call(t.Context(), nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Output != "Tool returned no output." {
		t.Fatalf("output = %q; a blank tool message reads to a model as a dropped turn", res.Output)
	}
}
