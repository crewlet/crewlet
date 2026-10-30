package checkrun

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
)

// A JUNIT REPORT, read as the test runners that write one write it: a
// <testsuites> of <testsuite> elements, or one <testsuite> alone, each holding
// <testcase> elements, a failed case carrying a <failure> or an <error> with a
// message attribute and, as its text, the runner's account — Go's
// go-junit-report and gotestsum, JUnit 4 and 5, pytest, Jest, Surefire all
// write this one shape. A case's file and line, where its runner writes them
// (`file` and `line` attributes, pytest's and Jest's), are its failure's
// place; its classname and name its test's id.
//
// STREAMED, element by element (encoding/xml's tokens), never decoded whole:
// a report is up to sandbox.MaxFileBytes, and only the failures an ending
// carries are held. encoding/xml expands no entity a document declares, so a
// report cannot grow past its bytes as it is read.

// readJUnit reads the JUnit report r into fs: its failed cases, and the counts
// its suites state. A report that is no XML, or no JUnit, is an error naming
// what is wrong, and fs keeps what it named before it.
func readJUnit(r io.Reader, fs *failures) error {
	d := xml.NewDecoder(r)
	d.Strict = true
	var (
		tests, failed int
		suites        int
		inCase        bool
		tc            testCase
	)
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("the JUnit report is no XML a test runner writes: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "testsuite":
				suites++
				tests += intAttr(t, "tests")
				failed += intAttr(t, "failures") + intAttr(t, "errors")
			case "testcase":
				inCase = true
				tc = testCase{class: attr(t, "classname"), name: attr(t, "name"), file: attr(t, "file"),
					line: intAttr(t, "line")}
			case "failure", "error":
				if !inCase {
					continue
				}
				body, err := text(d)
				if err != nil {
					return fmt.Errorf("the JUnit report's %s of %s: %w", t.Name.Local, tc.id(), err)
				}
				fs.add(tc.failure(attr(t, "message"), body))
			}
		case xml.EndElement:
			if t.Name.Local == "testcase" {
				inCase = false
			}
		}
	}
	if suites == 0 {
		return errors.New("the JUnit report holds no <testsuite>")
	}
	fs.tests, fs.failed = tests, failed
	return nil
}

// testCase is a <testcase>'s attributes.
type testCase struct {
	class, name, file string
	line              int
}

// id is the case's test id: its class and its name, as a runner prints them.
func (c testCase) id() string {
	switch {
	case c.class == "":
		return c.name
	case c.name == "":
		return c.class
	}
	return c.class + "." + c.name
}

// failure is the case failed with message and its runner's account body.
func (c testCase) failure(message, body string) code.CheckFailure {
	text := strings.TrimSpace(message)
	if body = strings.TrimSpace(body); body != "" && body != text {
		if text != "" {
			text += "\n"
		}
		text += body
	}
	return code.CheckFailure{Path: c.file, Line: c.line, TestID: c.id(), Message: text}
}

// text is the character data inside the element d has just
// opened, up to its end — at most CheckFailureMessageBytes of it held, the
// rest read and dropped.
func text(d *xml.Decoder) (string, error) {
	var (
		b     bytes.Buffer
		depth = 1
	)
	for depth > 0 {
		tok, err := d.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if room := code.CheckFailureMessageBytes - b.Len(); room > 0 {
				b.Write(t[:min(len(t), room)])
			}
		}
	}
	return b.String(), nil
}

// attr is t's attribute named name, "" where it has none.
func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// intAttr is t's attribute named name as a count, 0 where it has none or it
// is no count.
func intAttr(t xml.StartElement, name string) int {
	n, err := strconv.Atoi(strings.TrimSpace(attr(t, name)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
