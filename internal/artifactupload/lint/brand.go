package lint

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
)

// BrandOffAttribute is the attribute a page's `<html>` element carries to keep
// its own design rather than the company's adopted brand: with the value
// [BrandOffValue], the artifact origin places no brand stylesheet in the page,
// and a version whose entry page carries it pins none.
const (
	BrandOffAttribute = "data-crewlet-brand"
	BrandOffValue     = "off"
)

// BrandOff reports whether a page opts out of the adopted brand: its `<html>`
// start tag, the first element of the page, carries `data-crewlet-brand="off"`
// (the value read case-insensitively, as HTML reads an enumerated attribute).
//
// ONE READING FOR THE PUBLISH AND THE ORIGIN: the publish pipeline asks it of a
// version's entry page to decide the version's pin, and the artifact origin
// asks it of every document it serves before it places the stylesheet's link,
// so a page that opts out is never branded whichever document of a site it is.
//
// A BOUNDED READ OF THE PAGE'S OPENING: tokens up to the first element, past a
// byte order mark, a doctype, comments and white space. A page whose first
// element is not `<html>` (the tag is optional in HTML) has nowhere to say it,
// and is branded.
func BrandOff(page []byte) bool {
	z := html.NewTokenizer(bytes.NewReader(bytes.TrimPrefix(page, []byte("\xef\xbb\xbf"))))
	for {
		switch z.Next() {
		case html.ErrorToken, html.EndTagToken:
			return false
		case html.DoctypeToken, html.CommentToken:
			continue
		case html.TextToken:
			if len(bytes.TrimSpace(z.Text())) > 0 {
				return false
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			if !strings.EqualFold(tok.Data, "html") {
				return false
			}
			for _, a := range tok.Attr {
				if a.Namespace == "" && strings.EqualFold(a.Key, BrandOffAttribute) {
					return strings.EqualFold(strings.TrimSpace(a.Val), BrandOffValue)
				}
			}
			return false
		}
	}
}
