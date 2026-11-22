package fs

import "os"

// OSFile is an *os.File opened by a [SystemRoot].
// Its Name returns the path relative to the root as it was requested in Open,
// while *os.File.Name contains the root prefix too.
// Wrappers and openers rely on Name to locate related files (i.e. keys) through the same root.
type OSFile struct {
	*os.File
	name string
}

func NewOSFile(f *os.File, name string) *OSFile {
	return &OSFile{File: f, name: name}
}

func (f *OSFile) Name() string {
	return f.name
}
