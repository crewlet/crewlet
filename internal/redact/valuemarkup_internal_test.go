package redact

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"
)

// markupValue is the password's value every shape of [markupShapes] holds.
const markupValue = "placeholder9x"

// markupShape is a text holding a password's value on a line below its key that
// opens with markup of its own, and the value, which no output may show; where
// region is set, after the last occurrence of region alone.
type markupShape struct {
	name, text, value, region string
}

// markupShapes is every way a password's value comes after markup that is no
// value: a table's gutter, a bullet, a quote's `>`, a diff's `+` before a
// bullet, a comment's `#` and a list's number, on a line below its key that
// the key's line opens otherwise (a list's number on the key's line too),
// under four keys,
// alone, under a key's END above, a marker line above and an END on a line of
// its own, written with line feeds, CRLF and as a JSON string's lines; a line
// of a wrapping that writes its content unquoted; a value beside another key
// on the key's line; and the texts of the bar's corpora that showed a value
// read past a key's block written apart.
func markupShapes() []markupShape {
	var shapes []markupShape
	add := func(name, text, value, region string) {
		shapes = append(shapes, markupShape{name: name, text: text, value: value, region: region})
	}
	for _, g := range []struct{ name, open, close string }{
		{"table", "| ", " |"}, {"bullet", "- ", ""}, {"quote", "> ", ""}, {"diff bullet", "+- ", ""},
		{"comment", "# ", ""}, {"tight table", "|", "|"}, {"numbered", "1. ", ""}, {"numbered paren", "2) ", ""},
	} {
		for _, key := range []string{"Root password:", "Root password: ", "passwd:", "pwd="} {
			for _, above := range []struct{ name, text string }{
				{"alone", ""}, {"an END above", "| a |\nq8Zr4TfL2w==" + testEnd + `", "`},
				{"a marker above", `[REDACTED:private-key]", "`}, {"an END on its own line", testEnd + "\n"},
			} {
				for _, at := range []string{"below", "on the key's line"} {
					lines := above.text + key + "\n" + g.open + markupValue + g.close + "\n" + "done\n"
					if at != "below" {
						if !strings.HasPrefix(g.name, "numbered") {
							// Punctuation on the key's own line that no
							// line below repeats is the value alone: the
							// word after it is the field after a prompt or
							// the wrapping's own (the package doc's lean).
							continue
						}
						lines = above.text + strings.TrimRight(key, " ") + " " + g.open + markupValue + g.close + "\n" + "done\n"
					}
					name := g.name + ", " + strings.TrimSpace(key) + ", " + above.name + ", " + at
					add(name, lines, markupValue, "")
					add(name+", crlf", strings.ReplaceAll(lines, "\n", "\r\n"), markupValue, "")
					add(name+", escaped", `{"log": "`+strings.ReplaceAll(strings.ReplaceAll(lines, `"`, `\"`), "\n", `\n`)+`"}`+"\n", markupValue, "")
				}
			}
		}
	}
	// A line of the wrapping that writes its content unquoted, under a key's
	// line whose string the quote closes.
	for _, w := range []struct{ name, key, value string }{
		{"echo", `echo "Password:\n" >> k`, `echo placeholder9x >> k`},
		{"echo, single-quoted", `echo "passwd:\n" >> k`, `echo 'placeholder9x' >> k`},
		{"echo, file opened", `echo "Password:\n" > k`, `echo placeholder9x >> k`},
		{"tee", `echo "pwd=\n" | tee -a k`, `echo placeholder9x | tee -a k`},
		{"table, escaped quote", `| user: bob password: \" |`, `| placeholder9x |`},
		{"echo, redirect glued", `echo "Password:\n">>k`, `echo "placeholder9x">>k`},
		{"echo, redirect glued, unquoted", `echo "passwd:\n">>k`, `echo placeholder9x>>k`},
	} {
		lines := w.key + "\n" + w.value + "\n"
		add(w.name, lines, markupValue, "")
		add(w.name+", crlf", strings.ReplaceAll(lines, "\n", "\r\n"), markupValue, "")
	}
	// The value on the key's line, beside another key there, and on the rest
	// of a line an escaped break left after the key.
	for _, c := range []struct{ name, text string }{
		{"beside another key", "password: | placeholder9x | pwd: x\n"},
		{"beside another key, bullet", "passwd: - placeholder9x, pwd= x\n"},
		{"on the rest after an escaped break", "\"password:\\n\" | placeholder9x | pwd: x\n"},
		{"on the rest after a row's escaped break", `{"text": "| Password: |\n| placeholder9x | pwd: x"}` + "\n"},
		{"a list's number on the key's line", "Password: 1. placeholder9x\n"},
	} {
		add(c.name, c.text, markupValue, "")
	}
	for _, c := range []struct{ name, text, value string }{
		{"crit 17249", "| Password: |\n| hunter2x9 MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 |\nq8Zr4TfL2w==-----END RSA PRIVATE KEY-----\", \"Root password: \n| q8Zr4TfL2w==-placeholder9x |\n| Retype password: |\n| placeholder9y |\n", "placeholder9x"},
		{"crit 2397", "| u1SU1Lf= |\\n\n| -----BEGIN DSA PRIVATE KEY----- MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun -----END DSA PRIVATE KEY-----\tPASSWORD= |\\n\n|  |\\n\n|  |\\n\n| \"placeholder9x\" |\\n\n| done |\\n\n", "placeholder9x"},
		{"crit 4850", "{\"log\": \"| pwd=abc123 MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 -----END DSA PRIVATE KEY-----pwd: |\\n|  |\\n| placeholder9x |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 -----END DSA PRIVATE KEY----- |\\n\"}\n", "placeholder9x"},
		{"crit 17820", "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\\n\nq8Zr4TfL2w==\\n\n| u1SU1Lf=-----END DSA PRIVATE KEY-----\tPassword: |\\n\n|  |\\n\n|  |\\n\n| \"placeholder9x\" |\\n\n| done |\\n\n", "placeholder9x"},
		{"crit 19225", "{\"log\": \"| -----BEGIN PRIVATE KEY----- |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun |\\n| q8Zr4TfL2w==-----END PRIVATE KEY----- |\\n| q8Zr4TfL2w==-----END PRIVATE KEY----- tail passwd: |\\n|  |\\n|  |\\n| placeholder9x |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 -----END PRIVATE KEY----- |\\n\"}\n", "placeholder9x"},
		{"crit 24806", "| -----BEGIN DSA PRIVATE KEY----- |\\n\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun |\\n\n| q8Zr4TfL2w==-----END DSA PRIVATE KEY----- |\\n\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun -----END DSA PRIVATE KEY----- tail Root password:  |\\n\n|  |\\n\n|  |\\n\n| 4567890 |\\n\n", "4567890"},
		{"crit 29033", "| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun |\\n\n| q8Zr4TfL2w== |\\n\n| u1SU1Lf=-----END PRIVATE KEY----- PASSWORD= |\\n\n|  |\\n\n| Пароль9x |\\n\n| Retype password: |\\n\n| placeholder9y |\\n\n", "Пароль9x"},
		{"crit2 3379", "{\"log\": \"| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun |\\n| lXvB9yN4qL2sT8wE6rK1mP3oJ7hF5gD0cZ9aV4bN2xM8uY6iO1pQ3wE5rT7yU9iA |\\n| u1SU1Lf= -----END ENCRYPTED PRIVATE KEY----- tail password:  |\\n|  |\\n| placeholder9x |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 -----END ENCRYPTED PRIVATE KEY----- |\\n\"}\n", "placeholder9x"},
		{"crit2 7071", "{\"log\": \"| -----BEGIN EC PRIVATE KEY----- |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun |\\n| lXvB9yN4qL2sT8wE6rK1mP3oJ7hF5gD0cZ9aV4bN2xM8uY6iO1pQ3wE5rT7yU9iA |\\n| some log line |\\n| another log line |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun |\\n| u1SU1Lf= -----END EC PRIVATE KEY----- tail passwd: |\\n|  |\\n| placeholder9x |\\n| done |\\n\"}\n", "placeholder9x"},
		{"crit2 7537", "{\"log\": \"MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun -----BEGIN RSA PRIVATE KEY-----\\n| Proc-Type: 4,ENCRYPTED |\\n| DEK-Info: AES-128-CBC,0123456789ABCDEF0123456789ABCDEF |\\n|  |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun |\\n| lXvB9yN4qL2sT8wE6rK1mP3oJ7hF5gD0cZ9aV4bN2xM8uY6iO1pQ3wE5rT7yU9iA |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun -----END RSA PRIVATE KEY----- MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 -----END RSA PRIVATE KEY----- passwd: |\\n|  |\\n| 4567890 |\\n| Retype password: |\\n\"}\n", "4567890"},
		{"crit2 14885", "{\"log\": \"| MIIEowIBAAKCAQEAu1SU1LfVLPHCoz[...truncated] |\\n| [...truncated]D0cZ9aV4bN2xM8uY6iO1pQ3wE5rT7yU9iA |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6[...truncated] |\\n| u1SU1Lf= -----END RSA PRIVATE KEY-----\tpwd= |\\n|  |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 -----END RSA PRIVATE KEY----- |\\n| done |\\n\"}\n", "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6"},
		{"crit2 17183", "{\"log\": \"| MIIEowIBAAKCAQEAu1SU1LfVLPHCoz… |\\n| …D0cZ9aV4bN2xM8uY6iO1pQ3wE5rT7yU9iA |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6… |\\n| q8Zr4TfL2w==-----END RSA PRIVATE KEY----- tail pwd= |\\n|  |\\n| 4567890 |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 -----END RSA PRIVATE KEY----- |\\n\"}\n", "4567890"},
		{"crit2 23388", "{\"log\": \"MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 -----BEGIN RSA PRIVATE KEY-----\\n| Proc-Type: 4,ENCRYPTED |\\n| DEK-Info: AES-128-CBC,0123456789ABCDEF0123456789ABCDEF |\\n|  |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun |\\n| lXvB9yN4qL2sT8wE6rK1mP3oJ7hF5gD0cZ9aV4bN2xM8uY6iO1pQ3wE5rT7yU9iA |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun |\\n| u1SU1Lf= -----END RSA PRIVATE KEY----- tail Root password: |\\n|  |\\n| q8Zr4TfL2w==-x9 |\\n| Retype password: |\\n\"}\n", "q8Zr4TfL2w==-x9"},
		{"dump R3", "{\"log\": \"| Password: |\\n{\"log\": \"hunter2x9\"}\\nAb3f9C2d7E1b pulled\\n+MIIEowIBAAKCAQEAuqSUqLfVLPHCozMxHxMoalgOEePzNmatRgeLezVaffAtagun\\n\"password:\\n\" + pwd: x\\n-----BEGIN PGP PRIVATE KEY BLOCK-----\\n\"}\n", "hunter2x9"},
	} {
		region := ""
		if c.name == "crit2 14885" {
			// The value is a key's run, which the key's block above holds
			// too: shown or not past the key alone.
			region = "pwd="
		}
		add(c.name, c.text, c.value, region)
	}
	return shapes
}

// shownPast reports whether out shows value, past the last region in it where
// region is set.
func shownPast(out, value, region string) bool {
	if region != "" {
		out = out[strings.LastIndex(out, region)+1:]
	}
	return strings.Contains(out, value)
}

// A VALUE BELOW ITS KEY IS READ PAST ITS LINE'S OWN MARKUP: over every shape
// of [markupShapes], Secrets shows no value and a second pass changes nothing.
//
// Mutation: take the line's first word for the value whatever it holds
// ([ownMarkup] answering from), and the markup is replaced and the value shown;
// read past a key, a marker or an armour, and a second pass reads otherwise.
func TestAValueBelowItsKeyIsReadPastItsLinesOwnMarkup(t *testing.T) {
	t.Parallel()
	for _, c := range markupShapes() {
		once := Secrets(c.text)
		if shownPast(once, c.value, c.region) {
			t.Errorf("%s: the value %q is shown:\n text %q\n once %q", c.name, c.value, c.text, once)
		}
		if twice := Secrets(once); twice != once {
			t.Errorf("%s: a second pass changed\n%q\nto\n%q", c.name, once, twice)
		}
	}
}

// A STREAM READS A VALUE PAST ITS LINE'S OWN MARKUP AS THE WHOLE TEXT DOES:
// over every shape of [markupShapes], a stream at its own hold and at 256 KiB,
// written in one piece and in pieces of 7 bytes, writes Secrets.
//
// Mutation: as [TestAValueBelowItsKeyIsReadPastItsLinesOwnMarkup]'s, and a
// stream that read the value as the whole text does still writes Secrets, so
// this case holds the stream to the same reading, not to the fix.
func TestAStreamReadsAValuePastItsLinesOwnMarkupAsTheWholeTextDoes(t *testing.T) {
	t.Parallel()
	for _, c := range markupShapes() {
		want := Secrets(c.text)
		for _, h := range []int{holdBytes(), 256 << 10} {
			for _, n := range []int{len(c.text), 7} {
				var out bytes.Buffer
				s := newStreamHolding(&out, h)
				for at := 0; at < len(c.text); at += n {
					_, _ = s.Write([]byte(c.text[at:min(len(c.text), at+n)]))
				}
				if err := s.Close(); err != nil {
					t.Errorf("%s: hold %d, pieces of %d: Close: %v", c.name, h, n, err)
				}
				if got := out.String(); got != want {
					t.Errorf("%s: hold %d, pieces of %d: the stream wrote otherwise than Secrets, %s", c.name, h, n, parted(got, want))
				}
			}
		}
	}
}

// A VALUE LINE'S MARKUP STAYS THE VALUE BEFORE A KEY, A MARKER OR AN ARMOUR,
// AND AN EMPTY STRING AFTER IT IS THE VALUE: a key names its own value, below
// it, and a marker and an armour are no password's; an empty quoted string is
// a value ([isValue]), as on a key's line; and a list's number with no value
// after it is the value ([listMarker]), as any word is.
//
// Mutation: read past the markup to a key, a marker or an armour, past an
// empty quoted string, or past a number alone ([ownMarkup]), or match a
// suffix without its quote where no redirect or pipe follows it ([unquoted]),
// and a line is written otherwise.
func TestAValueLinesMarkupStaysTheValueBeforeAKeyAMarkerAnArmourOrAnEmptyString(t *testing.T) {
	t.Parallel()
	m := "[REDACTED:password]"
	for name, c := range map[string][2]string{
		"a key":                 {"Password:\n| password: |\n| hunter2x9 |\n", "Password:\n" + m + " password: |\n" + m + " |\n"},
		"a marker":              {"Password:\n| " + m + " |\n", "Password:\n" + m + " " + m + " |\n"},
		"an armour":             {"Password:\n| -----END CERTIFICATE----- |\n", "Password:\n" + m + " -----END CERTIFICATE----- |\n"},
		"an empty string":       {"Password:\n| \"\" hunter |\n", "Password:\n" + m + " hunter |\n"},
		"a lone number":         {"Password:\n1.\ndone\n", "Password:\n" + m + "\ndone\n"},
		"a number value":        {"Password: 123.\ndone\n", "Password: " + m + "\ndone\n"},
		"a number after markup": {"Password:\n- 2.\ndone\n", "Password:\n" + m + "\ndone\n"},
		"a field after a prompt's quote, over a line ending in it": {
			"msg=\"password:\" level=info\nmsg=started level=info\n", "msg=\"password:" + m + " level=info\nmsg=started level=info\n",
		},
	} {
		if got := Secrets(c[0]); got != c[1] {
			t.Errorf("%s: Secrets(%q)\n got  %q\n want %q", name, c[0], got, c[1])
		}
	}
}

// A KEY'S RUN AFTER A KEY'S QUOTE IS NO WRAPPING: where the key's quote is
// followed by words, a key's run and its END, a line below ending in them is
// no line of the key's wrapping, and the key's line below is the key's,
// redacted with its block ([unquoted]).
//
// Mutation: strip the key's quote from any suffix, and the key's line below is
// read as the password's value, kept from the keys, and shown.
func TestAKeysRunAfterAKeysQuoteIsNoWrapping(t *testing.T) {
	t.Parallel()
	pc := testKeyLine[:56]
	text := "hunter2x9 " + pc + "\nPassword: \" " + pc + " " + testEnd + "\n" + testKeyLine + " " + pc + " " + testEnd + "\n"
	if once := Secrets(text); strings.Contains(once, testKeyLine) {
		t.Errorf("a key's line is shown:\n text %q\n once %q", text, once)
	}
}

// A KEY'S RUN AFTER MARKUP IS THE KEY'S: where markup below a key opens a line
// whose next word is a run of a private key's body, the markup is the value
// and the run stays the key's, read with its block, so the lines the block
// holds stay redacted with it.
//
// Mutation: read past the markup to a key's run ([ownMarkup]), and the run is
// claimed as the password's value, the key's block no longer holds the lines
// below, and the value on one of them is shown.
func TestAKeysRunAfterMarkupIsTheKeys(t *testing.T) {
	t.Parallel()
	pc := testKeyLine[:56]
	text := "+" + testKeyLine + "\n+" + testEnd + "|passwd:\n+\" " + pc + "\n+Password: \"\nhunter2x9 \" " + pc + " " + testEnd + "\n+u1SU1Lf=\n"
	if once := Secrets(text); strings.Contains(once, "hunter2x9") {
		t.Errorf("the value is shown:\n text %q\n once %q", text, once)
	}
}

// A KEY'S RUN IS NO VALUE OF A SUFFIX SPELLED WITHOUT ITS QUOTE: a key's row
// `| Password: " |` over rows of a private key's body ending in ` |` reads none
// of them as the wrapping's line ([valueRead.ending]), the key's quote is its
// value, and the key's block reads its rows, the lines it holds redacted.
//
// Mutation: match the suffix without its quote over a key's run, and the run
// is claimed as the value, the block stops short, and a word it held is shown.
func TestAKeysRunIsNoValueOfASuffixSpelledWithoutItsQuote(t *testing.T) {
	t.Parallel()
	pc := testKeyLine[:56]
	text := "| Password: \" |\n| fAt0gun+ |\n| hunter2x9 \" " + pc + " " + testEnd + " |\n"
	if once := Secrets(text); strings.Contains(once, "hunter2x9") {
		t.Errorf("a word the key's block holds is shown:\n text %q\n once %q", text, once)
	}
}

// A KEY BEFORE A VALUE PAST MARKUP IS FOUND WITHIN ITS LABEL'S RUN
// ([keyBefore]): what it answers at every word end of a text is what the
// first key read [MaxReach] past the word answers, over random lines of keys,
// labels, markers, `_`, markup and bytes no UTF-8 holds. A key past where a
// label read back to the word would stop ([labelEnd]) has a label beginning
// at the word or past it, so the search ends there.
//
// Mutation: end the label's run at a key's marker, at `_`, or at the word
// itself, and an answer differs.
func TestAKeyBeforeAValuePastMarkupIsFoundWithinItsLabelsRun(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1897, 1))
	pieces := []string{
		"Password: ", "password=", "Retype password:", "db_password=", "pwd ", "pass", "x",
		" ", "  ", "\t", "\n", "|", "**", "- ", ":", ",", "_", "(", ".", "/", "'",
		"hunter2", "é", "\xff", "\xe2\x82", keyMarker, keyMarker[:5], Marker,
	}
	whole := func(text string, from, w int) bool {
		k := keyedIndex(passwordRule(), text[from:min(len(text), w+MaxReach)])
		return k >= 0 && labelStart(text, from, from+k) < w
	}
	for range 4000 {
		var b strings.Builder
		for range 1 + r.IntN(24) {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		text := b.String()
		from := r.IntN(len(text) + 1)
		before := keyBefore(text, from)
		for w := from; w <= len(text); w++ {
			if w < len(text) && !valueSpace(text[w]) && text[w] != '\\' {
				continue // a word ends at a space, an escaped break or the text's end
			}
			if got, want := before(w), whole(text, from, w); got != want {
				t.Fatalf("%q from %d, a word ending at %d: a key before it %v, read MaxReach past it %v", text, from, w, got, want)
			}
		}
	}
}

// A VALUE PAST MARKUP TRIES NO KEY PAST ITS LABEL'S RUN ([keyBefore]): read
// past the markup of `Password: | hunter2x9 |`, the value asks whether a key's
// label begins before its word, and the pattern is tried only before where
// that label would stop ([labelEnd]), at the `|`, however many key words
// follow on the line before its next key. Counted, not timed ([keyTried]):
// the cost is the tries, and a count is the same on any runner.
//
// Mutation: search to [MaxReach] past the word, as keyBefore once did, and
// the tries grow with the key words after the value. Not parallel: it sets
// keyTried, which parallel tests would race on.
func TestAValuePastMarkupTriesNoKeyPastItsLabelsRun(t *testing.T) {
	tries := 0
	keyTried = func() { tries++ }
	defer func() { keyTried = nil }()
	read := func(words int) int {
		tries = 0
		_ = Secrets("Password: | hunter2x9 | " + strings.Repeat("password ", words) + "password: x\n")
		return tries
	}
	if short, long := read(16), read(4096); long != short {
		t.Errorf("a value past markup tried the pattern %d times before 4096 key words and %d before 16: "+
			"the search reads past its label's run", long, short)
	}
}
