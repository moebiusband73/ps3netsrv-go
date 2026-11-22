package ioutil

import (
	"errors"
	"fmt"
	"io"
)

// FillBuffer fills a provided buffer from ReadSeeker starting from pos and restore position back.
// It tries to use [io.ReaderAt] if possible.
// It returns error [io.ErrUnexpectedEOF] if buffer didn't filled completely.
func FillBuffer(f io.ReadSeeker, pos int64, buf []byte) (err error) {
	if rdAt, ok := f.(io.ReaderAt); ok {
		n, err := rdAt.ReadAt(buf, pos)
		if n == len(buf) {
			// ReadAt may return io.EOF together with full buffer at the end of file
			return nil
		}
		if err == nil || errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return fmt.Errorf("read at: %w", err)
	}

	currOffset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("get current position: %w", err)
	}
	defer func() {
		_, restoreErr := f.Seek(currOffset, io.SeekStart)
		if restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore position: %w", restoreErr))
		}
	}()

	_, err = f.Seek(pos, io.SeekStart)
	if err != nil {
		return fmt.Errorf("seek to pos: %w", err)
	}

	_, err = io.ReadFull(f, buf)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	return nil
}
