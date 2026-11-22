package filesystem

import (
	"os"

	"github.com/xakep666/ps3netsrv-go/internal/handler"
	pkgfs "github.com/xakep666/ps3netsrv-go/pkg/fs"
)

// StrictSystemRoot wraps *os.Root to make it usable on 32-bit platforms.
//
// os.Root opens files via a raw openat that does not set O_LARGEFILE, so on a
// 32-bit kernel any file larger than 2 GiB fails to open with EOVERFLOW. Every
// PS3 ISO exceeds that, which makes strict-root mode unusable for PS3 streaming
// on legacy 32-bit NAS hardware. This wrapper forwards O_LARGEFILE through
// os.Root.OpenFile (which passes the flag verbatim to openat) while keeping all
// of os.Root's symlink-safe, path-traversal-safe traversal.
//
// It satisfies pkg/fs.SystemRoot: Open/Create are overridden here, and the
// embedded *os.Root supplies Stat, Remove and Mkdir.
type StrictSystemRoot struct {
	*os.Root
}

// NewStrictSystemRoot wraps an already-opened *os.Root.
func NewStrictSystemRoot(root *os.Root) StrictSystemRoot {
	return StrictSystemRoot{Root: root}
}

func (r StrictSystemRoot) Open(path string) (handler.File, error) {
	f, err := r.Root.OpenFile(path, os.O_RDONLY|openLargeFile, 0)
	if err != nil {
		return nil, err
	}

	return pkgfs.NewOSFile(f, path), nil
}

func (r StrictSystemRoot) Create(path string) (handler.WritableFile, error) {
	f, err := r.Root.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC|openLargeFile, 0666)
	if err != nil {
		return nil, err
	}

	return pkgfs.NewOSFile(f, path), nil
}
