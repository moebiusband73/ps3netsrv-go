package kongutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"

	"github.com/alecthomas/kong"
)

var OutputFileMapper = kong.NamedMapper("outputfile", kong.MapperFunc(outputFileMapper))

func outputFileMapper(dctx *kong.DecodeContext, target reflect.Value) error {
	if _, ok := target.Interface().(*os.File); !ok {
		return fmt.Errorf("\"outputfile\" can only be used with *os.File")
	}

	var path string
	err := dctx.Scan.PopValueInto("file", &path)
	if err != nil {
		return err
	}

	if path == "-" {
		target.Set(reflect.ValueOf(os.Stdout))
		return nil
	}

	path = kong.ExpandPath(path)

	// O_EXCL makes existence check and creation atomic, images must not be executable
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("target file already exists")
	}
	if err != nil {
		return err
	}

	target.Set(reflect.ValueOf(f))

	return nil
}
