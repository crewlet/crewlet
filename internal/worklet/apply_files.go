package worklet

import (
	"fmt"
)

// decodeFile is a file record's post-state, refused where the applier would
// refuse to apply it: the payload decoded and held to the subject that
// arbitrated it and to the object rule ([fileMatches]).
//
// ONE READING OF A FILE RECORD, for the applier and for
// [FileObjectReferences]' Records alike, so a backup carries what the applier
// writes and refuses what it refuses (ADR-0082).
func decodeFile(mutation []byte, id string) (File, error) {
	var file File
	if err := decodePayload(mutation, &file); err != nil {
		return File{}, fmt.Errorf("decode the file: %w", err)
	}
	if err := fileMatches(id, file); err != nil {
		return File{}, err
	}
	return file, nil
}

// fileMatches refuses a file record whose payload is not the file its subject
// arbitrated, or whose object could not be read back.
//
// THE SUBJECT IS THE ADDRESS: a payload path other than the arbitrated one
// would leave that path held by nothing and the written one by two.
//
// THE OBJECT'S DIGEST MUST PARSE ([objstore.ParseHash]): the backup verifies
// every object against it, so a bad one stops every backup on every node
// (the key decodes only canonically, [objstore.Key]). A LIVE FILE MUST NAME
// AN OBJECT, or it is listed with content no reader can get.
//
// The writer refuses all of this first ([checkFilePut]).
func fileMatches(id string, file File) error {
	path, err := NormalizeFilePath(file.Path)
	switch {
	case err != nil:
		return err
	case path != file.Path || FileSubject(file.Project, file.Path).ID != id:
		return fmt.Errorf("it arbitrated file %s and its payload claims %s/%q — the "+
			"subject IS the address", id, file.Project, file.Path)
	}
	o, named := file.Content()
	switch {
	case !named && !file.Removed():
		return fmt.Errorf("it writes %s/%q live and names no object holding its "+
			"content", file.Project, file.Path)
	case named:
		if err := o.Validate(); err != nil {
			return fmt.Errorf("it names its object as something no reader could "+
				"check: %w", err)
		}
	}
	return nil
}
