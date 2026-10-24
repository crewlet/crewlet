// Package mediatype is the ONE table of the file types an artifact may publish,
// keyed by extension: the type the serving origin sends, the class it belongs
// to, whether it is compressed on the way out and whether a browser may execute
// it.
//
// The served type is always this table's and never a client's declaration: a
// client-declared type only disambiguates an extension the table already knows.
// A type outside it is refused at publish naming the path, so the serving
// origin never meets a file it cannot describe. Scriptable types (HTML, SVG,
// JavaScript) are safe to serve only because the artifact origin holds no
// credential; the dashboard origin never renders any of them.
//
// Read by the publish validator, the serving handler and the asset upload, so
// the three can never disagree about a file.
package mediatype
