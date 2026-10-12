package tracker

import (
	"fmt"
)

// fileMatches refuses a file record whose payload is not the file its subject
// arbitrated, or whose object could not be read back.
//
// THE SUBJECT IS THE ADDRESS, for the page title's reason: a writer that took
// one path at the broker and wrote another into every node's row would leave
// the arbitrated path held by nothing and the written one held by two.
//
// AND AN OBJECT IS NAMED WITH A DIGEST AND A SIZE IT CAN BE CHECKED AGAINST:
// the backup verifies every object it copies against the row's digest and
// size, reading the digest back through [objstore.ParseHash], so a row naming
// an object beside something that is not a digest would stop every backup for
// good, on every node. The key needs no check of its own here — it decodes
// only in its one canonical spelling ([objstore.Key]). A LIVE FILE MUST NAME
// ONE, and only a removal names none: a live row naming nothing would be a
// file listed with content no reader can get.
//
// Every refusal here is one the writer makes first ([checkFilePut]), so a
// record that reaches it was written by something else.
func fileMatches(c applyContext, id string, file File) error {
	path, err := NormalizeFilePath(file.Path)
	switch {
	case err != nil:
		return fmt.Errorf("tracker: the file record at %s: %w", c.position, err)
	case path != file.Path || FileSubject(file.Project, file.Path).ID != id:
		return fmt.Errorf("tracker: the record at %s arbitrated file %s and its "+
			"payload claims %s/%q — the subject IS the address", c.position, id,
			file.Project, file.Path)
	}
	o, named := file.Content()
	switch {
	case !named && !file.Removed():
		return fmt.Errorf("tracker: the file record at %s writes %s/%q live and "+
			"names no object holding its content", c.position, file.Project, file.Path)
	case named:
		if err := o.Validate(); err != nil {
			return fmt.Errorf("tracker: the file record at %s names its object as "+
				"something no reader could check: %w", c.position, err)
		}
	}
	return nil
}
