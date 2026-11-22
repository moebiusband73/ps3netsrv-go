package s3root

import (
	"context"
	"io"
	"io/fs"
	"iter"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/minio/minio-go/v7"
)

// dir emulates directory using "/"-delimited listing of key prefix.
type dir struct {
	root   *Root
	name   string
	info   objInfo
	prefix string // listing prefix

	ctx    context.Context
	cancel context.CancelFunc

	next          func() (minio.ObjectInfo, bool)
	stop          func()
	listingExpiry time.Time
	done          bool
}

func newDir(r *Root, name string, info objInfo) *dir {
	ctx, cancel := context.WithCancel(r.ctx)
	return &dir{
		root:   r,
		name:   name,
		info:   info,
		prefix: r.dirPrefix(info.key),
		ctx:    ctx,
		cancel: cancel,
	}
}

func (d *dir) Name() string { return d.name }

func (d *dir) Stat() (fs.FileInfo, error) { return d.info.fileInfo(), nil }

func (d *dir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: syscall.EISDIR}
}

func (d *dir) Seek(int64, int) (int64, error) {
	return 0, &fs.PathError{Op: "seek", Path: d.name, Err: syscall.EISDIR}
}

func (d *dir) Close() error {
	if d.stop != nil {
		d.stop()
	}
	d.cancel()
	return nil
}

// ReadDir follows [os.File.ReadDir] semantics.
func (d *dir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.next == nil && !d.done {
		d.listingExpiry = d.root.meta.beginListing()
		var seq iter.Seq[minio.ObjectInfo] = d.root.client.ListObjectsIter(d.ctx, d.root.bucket, minio.ListObjectsOptions{
			Prefix: d.prefix,
		})
		d.next, d.stop = iter.Pull(seq)
	}

	var entries []fs.DirEntry
	for !d.done && (n <= 0 || len(entries) < n) {
		obj, ok := d.next()
		if !ok {
			d.done = true
			d.stop()
			d.root.meta.completeListing(d.prefix, d.listingExpiry)
			break
		}

		if obj.Err != nil {
			d.done = true
			d.stop()
			return entries, &fs.PathError{Op: "readdir", Path: d.name, Err: mapError(obj.Err)}
		}

		entry, ok := d.entry(obj)
		if !ok {
			continue
		}

		d.root.meta.put(entry.info.key, entry.info)
		entries = append(entries, entry)
	}

	if n > 0 && len(entries) == 0 {
		return nil, io.EOF
	}

	return entries, nil
}

// entry converts listing item to directory entry. Items which can't be addressed by path are skipped.
func (d *dir) entry(obj minio.ObjectInfo) (dirEntry, bool) {
	name, isDir := strings.CutSuffix(strings.TrimPrefix(obj.Key, d.prefix), "/")
	if !validName(name) {
		return dirEntry{}, false
	}

	key := d.prefix + name
	if isDir {
		return dirEntry{info: objInfo{key: key, name: name, isDir: true, modTime: d.root.created}}, true
	}

	info := objInfoFromObject(key, obj)
	info.name = name
	return dirEntry{info: info}, true
}

// validName reports whether directory entry name can be opened again by path.
func validName(name string) bool {
	switch {
	case name == "", name == ".", name == "..":
		return false // i.e. directory marker or "a//b" keys
	case strings.Contains(name, "/"):
		return false
	case runtime.GOOS == "windows" && strings.ContainsAny(name, `\:`):
		return false
	default:
		return true
	}
}

type dirEntry struct {
	info objInfo
}

func (e dirEntry) Name() string               { return e.info.name }
func (e dirEntry) IsDir() bool                { return e.info.isDir }
func (e dirEntry) Type() fs.FileMode          { return e.info.fileInfo().Mode().Type() }
func (e dirEntry) Info() (fs.FileInfo, error) { return e.info.fileInfo(), nil }
func (e dirEntry) String() string             { return fs.FormatDirEntry(e) }
