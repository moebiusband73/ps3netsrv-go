package fs

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/xakep666/ps3netsrv-go/internal/handler"
)

// RelaxedSystemRoot just adds provided prefix to the file paths before making a system call.
type RelaxedSystemRoot struct {
	path string
}

func NewRelaxedSystemRoot(path string) *RelaxedSystemRoot {
	return &RelaxedSystemRoot{path: path}
}

func (r *RelaxedSystemRoot) realPath(name string) (path string, err error) {
	// taken from https://github.com/spf13/afero/blob/master/basepath.go

	if err := validateBasePathName(name); err != nil {
		return name, err
	}

	bpath := filepath.Clean(r.path)
	path = filepath.Join(bpath, name)

	// plain prefix check is not enough: "/srv/ps3_private" has prefix "/srv/ps3"
	rel, err := filepath.Rel(bpath, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return name, os.ErrNotExist
	}

	return path, nil
}

func validateBasePathName(name string) error {
	if runtime.GOOS != "windows" {
		// Not much to do here;
		// the virtual file paths all look absolute on *nix.
		return nil
	}

	// On Windows a common mistake would be to provide an absolute OS path
	// We could strip out the base part, but that would not be very portable.
	if filepath.IsAbs(name) {
		return os.ErrNotExist
	}

	return nil
}

func (r *RelaxedSystemRoot) Open(path string) (handler.File, error) {
	realPath, err := r.realPath(path)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(realPath)
	if err != nil {
		return nil, err
	}

	return NewOSFile(f, path), nil
}

func (r *RelaxedSystemRoot) Create(path string) (handler.WritableFile, error) {
	realPath, err := r.realPath(path)
	if err != nil {
		return nil, err
	}

	f, err := os.Create(realPath)
	if err != nil {
		return nil, err
	}

	return NewOSFile(f, path), nil
}

func (r *RelaxedSystemRoot) Stat(path string) (fs.FileInfo, error) {
	realPath, err := r.realPath(path)
	if err != nil {
		return nil, err
	}

	return os.Stat(realPath)
}

func (r *RelaxedSystemRoot) Remove(path string) error {
	realPath, err := r.realPath(path)
	if err != nil {
		return err
	}

	return os.Remove(realPath)
}

func (r *RelaxedSystemRoot) Mkdir(path string, mode os.FileMode) error {
	realPath, err := r.realPath(path)
	if err != nil {
		return err
	}

	return os.Mkdir(realPath, mode)
}
