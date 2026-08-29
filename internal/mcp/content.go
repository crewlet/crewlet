package mcp

import (
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Block kinds, matching the MCP spec's own "type" discriminator.
const (
	BlockText         = "text"
	BlockImage        = "image"
	BlockAudio        = "audio"
	BlockResource     = "resource"
	BlockResourceLink = "resource_link"
)

// Block is one piece of a tool result, in this package's own vocabulary rather
// than the SDK's type hierarchy.
//
// The engine renders these into the text a model reads. The reason this is a
// struct and not the SDK's Content interface is the EmbeddedResource case
// below: the body of a resource lives one level down, and the code that has to
// remember to reach for it should be in one place.
type Block struct {
	// Type is one of the Block* constants, or whatever a future spec version
	// sends that this build does not know.
	Type string

	// Text is the payload of a text block, and the body of an embedded
	// resource whose content is textual.
	Text string

	// Data is the decoded payload of an image or audio block.
	Data []byte

	// MIME is the media type where the block carries one.
	MIME string

	// URI identifies a resource or resource link.
	URI string

	// Name labels a resource link.
	Name string

	// Blob marks an embedded resource whose body is binary: there is a
	// payload, it just cannot be inlined as text.
	Blob bool
}

// blocksFrom converts the SDK's typed content into Blocks.
//
// The EmbeddedResource branch is the one that matters. Its payload is NOT on
// the block, it is on block.Resource — and a conversion that fell through to a
// catch-all kept only the type and silently dropped the body. A tool like
// GitHub's get_file_contents, which returns a file as an embedded text
// resource, then reached the model as the opaque string {'type': 'resource'}
// and left it blind to the contents it had just asked for.
func blocksFrom(content []sdk.Content) []Block {
	out := make([]Block, 0, len(content))
	for _, c := range content {
		switch b := c.(type) {
		case *sdk.TextContent:
			out = append(out, Block{Type: BlockText, Text: b.Text})
		case *sdk.ImageContent:
			out = append(out, Block{Type: BlockImage, Data: b.Data, MIME: b.MIMEType})
		case *sdk.AudioContent:
			out = append(out, Block{Type: BlockAudio, Data: b.Data, MIME: b.MIMEType})
		case *sdk.EmbeddedResource:
			blk := Block{Type: BlockResource}
			if r := b.Resource; r != nil {
				blk.URI, blk.MIME, blk.Text = r.URI, r.MIMEType, r.Text
				// A resource carries text OR blob. Text wins because it can
				// be shown; the flag is how a caller tells "binary body" from
				// "empty body", which read identically otherwise.
				blk.Blob = r.Text == "" && len(r.Blob) > 0
			}
			out = append(out, blk)
		case *sdk.ResourceLink:
			out = append(out, Block{
				Type: BlockResourceLink,
				URI:  b.URI,
				Name: b.Name,
				MIME: b.MIMEType,
			})
		default:
			// A content type this build does not know. Name it rather than
			// dropping it: the model asked for something and got a shape we
			// could not read, which is a fact worth showing.
			out = append(out, Block{Type: fmt.Sprintf("%T", c)})
		}
	}
	return out
}

// Render turns one block into the text an agent sees.
//
// Textual payloads are surfaced verbatim. Everything else gets a compact
// descriptor: a binary body cannot be inlined, but a line saying what came
// back is the difference between the model knowing the call succeeded and the
// model seeing nothing.
func (b Block) Render() string {
	mime := b.MIME
	if mime == "" {
		mime = "unknown"
	}
	switch b.Type {
	case BlockText:
		return b.Text
	case BlockResource:
		if b.Text != "" {
			return b.Text
		}
		if b.URI != "" {
			return fmt.Sprintf("[resource: %s (%s)]", b.URI, mime)
		}
		return fmt.Sprintf("[resource: (%s)]", mime)
	case BlockResourceLink:
		detail := b.Name
		if b.MIME != "" {
			if detail != "" {
				detail += " "
			}
			detail += "(" + b.MIME + ")"
		}
		label := b.URI
		if detail != "" {
			label = strings.TrimSpace(b.URI + " " + detail)
		}
		return fmt.Sprintf("[resource_link: %s]", label)
	case BlockImage:
		return fmt.Sprintf("[image: %s]", mime)
	case BlockAudio:
		return fmt.Sprintf("[audio: %s]", mime)
	default:
		return fmt.Sprintf("[unsupported content: %s]", b.Type)
	}
}

// renderBlocks joins the renderable parts of a result, dropping empties so a
// server padding its reply with blank text blocks does not pad the model's
// context with blank lines.
func renderBlocks(blocks []Block) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if s := b.Render(); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

// errorText joins the TEXT blocks of a failed call.
//
// Only text: an error whose explanation is an image is not an explanation, and
// splicing a descriptor into the error string would make "[image: image/png]"
// read as the server's own words.
func errorText(blocks []Block) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == BlockText && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, " ")
}
