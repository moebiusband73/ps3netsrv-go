package chd

import (
	"errors"
	"io"
	"log/slog"
	"math"
	"structs"
	"syscall"
	"unsafe"

	"github.com/xakep666/ps3netsrv-go/internal/handler"
	"github.com/xakep666/ps3netsrv-go/internal/logutil"
	"github.com/xakep666/ps3netsrv-go/internal/osutil/dynamiclibs"
)

// fileCallbacks are called by libchdr ituserdata to outsource i/o operations
type fileCallbacks struct {
	_ structs.HostLayout

	fsize  uintptr
	fread  uintptr
	fclose uintptr
	fseek  uintptr
}

func newFileCallbacks(log *slog.Logger) *fileCallbacks {
	// dynamiclibs.Handle used to propagate a Go value through C code without it's being garbage-collected
	return &fileCallbacks{
		// signature: uint64_t fsize(void* userdata);
		fsize: dynamiclibs.NewCallback(func(_ dynamiclibs.CDecl, userdata dynamiclibs.Handle) uint64 {
			f := userdata.Value().(handler.File)
			ret, err := f.Seek(0, io.SeekEnd)
			if err != nil {
				log.Error("chd: seek/fsize failed", logutil.ErrorAttr(err), slog.String("name", f.Name()))
				return math.MaxUint64
			}
			return uint64(ret)
		}),
		// signature: size_t fread(void *target, size_t size, size_t count, void* userdata);
		// Go's analog for size_t is uint.
		fread: dynamiclibs.NewCallback(func(_ dynamiclibs.CDecl, target *byte, size, count uint, userdata dynamiclibs.Handle) uint {
			f := userdata.Value().(handler.File)
			if size == 0 || count == 0 {
				return 0
			}

			// fread semantics: fill the buffer unless EOF or error occurs and return number of complete items.
			// Single Read is not enough for files which may return short reads (i.e. network-backed ones).
			n, err := io.ReadFull(f, unsafe.Slice(target, count*size))
			switch {
			case errors.Is(err, nil), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
				return uint(n) / size
			default:
				log.Error("chd: read failed", logutil.ErrorAttr(err), slog.String("name", f.Name()))
				return uint(n) / size // short count signals error
			}
		}),
		// signatrue: int fclose(void *userdata);
		fclose: dynamiclibs.NewCallback(func(_ dynamiclibs.CDecl, userdata dynamiclibs.Handle) int {
			defer userdata.Delete()

			f := userdata.Value().(handler.File)
			err := f.Close()
			if err != nil {
				log.Error("chd: close failed", logutil.ErrorAttr(err), slog.String("name", f.Name()))
				return extractSysErrCode(err)
			}
			return 0
		}),
		// signature: int fseek(void *userdata, int64_t offset, int whence);
		fseek: dynamiclibs.NewCallback(func(_ dynamiclibs.CDecl, userdata dynamiclibs.Handle, offset int64, whence int) int {
			f := userdata.Value().(handler.File)
			_, err := f.Seek(offset, whence)
			if err != nil {
				log.Error("chd: seek failed", logutil.ErrorAttr(err), slog.String("name", f.Name()))
				return extractSysErrCode(err)
			}
			return 0
		}),
	}
}

func extractSysErrCode(err error) int {
	if err == nil {
		return 0
	}

	sysErr, ok := errors.AsType[syscall.Errno](err)
	if ok {
		return -int(sysErr)
	}
	return -1
}
