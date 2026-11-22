package s3root

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"sync"
	"syscall"
)

// file is a read-only object reader. Data is read through shared page cache.
// Sequential access is detected to download data ahead in large ranged requests.
type file struct {
	root *Root
	name string
	info objInfo

	ctx    context.Context // cancelled on Close, aborts readahead of this file
	cancel context.CancelFunc

	mu      sync.Mutex
	offset  int64
	lastEnd int64 // end of previous read, -1 before first read
	window  int64 // current readahead window in bytes, 0 for random access
	raNext  int64 // first page not yet requested by readahead
	closed  bool
}

func newFile(r *Root, name string, info objInfo) *file {
	ctx, cancel := context.WithCancel(r.ctx)
	return &file{
		root:    r,
		name:    name,
		info:    info,
		ctx:     ctx,
		cancel:  cancel,
		lastEnd: -1,
	}
}

func (f *file) Name() string { return f.name }

func (f *file) Stat() (fs.FileInfo, error) { return f.info.fileInfo(), nil }

func (f *file) ReadDir(int) ([]fs.DirEntry, error) {
	return nil, &fs.PathError{Op: "readdir", Path: f.name, Err: syscall.ENOTDIR}
}

func (f *file) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return fs.ErrClosed
	}

	f.closed = true
	f.cancel()
	return nil
}

// Read fills the buffer completely unless EOF or error occurs.
func (f *file) Read(b []byte) (int, error) {
	f.mu.Lock()
	off := f.offset
	f.mu.Unlock()

	n, err := f.ReadAt(b, off)

	f.mu.Lock()
	f.offset = off + int64(n)
	f.mu.Unlock()

	if n > 0 && errors.Is(err, io.EOF) {
		return n, nil
	}

	return n, err
}

func (f *file) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return 0, fs.ErrClosed
	}

	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.offset
	case io.SeekEnd:
		offset += f.info.size
	default:
		return 0, syscall.EINVAL
	}

	if offset < 0 {
		return 0, syscall.EINVAL
	}

	f.offset = offset
	return offset, nil
}

func (f *file) ReadAt(b []byte, off int64) (int, error) {
	if err := f.planRead(off, len(b)); err != nil {
		return 0, err
	}

	if off >= f.info.size {
		return 0, io.EOF
	}

	pageSize := f.root.cfg.PageSize
	end := min(off+int64(len(b)), f.info.size)

	n := 0
	for pos := off; pos < end; {
		idx := pos / pageSize
		data, err := f.page(idx)
		if err != nil {
			return n, &fs.PathError{Op: "read", Path: f.name, Err: err}
		}

		copied := copy(b[n:end-off], data[pos-idx*pageSize:])
		n += copied
		pos += int64(copied)
	}

	if n < len(b) {
		return n, io.EOF
	}

	return n, nil
}

// planRead validates the read and requests pages: only touched ones for random access
// and also next window of pages for sequential access.
func (f *file) planRead(off int64, length int) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case f.closed:
		return fs.ErrClosed
	case off < 0:
		return syscall.EINVAL
	case off >= f.info.size || length == 0:
		return nil
	}

	cfg := &f.root.cfg
	end := min(off+int64(length), f.info.size)
	first, last := off/cfg.PageSize, (end-1)/cfg.PageSize

	// Small gaps and overlaps are still sequential access, i.e. CD sector reads with skipped subchannel data.
	sequential := f.lastEnd >= 0 && off >= f.lastEnd-cfg.PageSize && off <= f.lastEnd+2*cfg.PageSize
	f.lastEnd = end

	if !sequential {
		f.window = 0
		f.raNext = last + 1
		f.root.fetch(f.ctx, f.info, first, last)
		return nil
	}

	if f.window == 0 {
		f.window = cfg.ReadaheadMin
	} else {
		f.window = min(f.window*2, cfg.ReadaheadMax)
	}

	windowPages := (f.window + cfg.PageSize - 1) / cfg.PageSize
	f.raNext = max(f.raNext, first)

	// request next window when less than half of the current one remains ahead
	if f.raNext-last <= windowPages/2 {
		target := last + windowPages
		f.root.fetch(f.ctx, f.info, f.raNext, target)
		f.raNext = target + 1
	}

	return nil
}

// page returns page data waiting for download if needed.
func (f *file) page(idx int64) ([]byte, error) {
	const maxRetries = 2

	k := f.root.pageKey(f.info, idx)
	for attempt := 0; ; attempt++ {
		data, call, owner := f.root.pages.lookup(k)
		if data != nil {
			return data, nil
		}

		if owner { // not requested yet or evicted before use
			go f.root.download(f.ctx, f.info, idx, []*pageCall{call})
		}

		select {
		case <-call.done:
		case <-f.ctx.Done():
			return nil, f.ctx.Err()
		}

		if call.err == nil {
			return call.data, nil
		}

		// download started by another (closed) file was cancelled, try again by ourselves
		if attempt < maxRetries && f.ctx.Err() == nil && errors.Is(call.err, context.Canceled) {
			continue
		}

		return nil, call.err
	}
}
