//go:build !linux || nopurego || !(amd64 || arm64 || 386 || arm || loong64 || ppc64le || riscv64 || s390x)

package dynamiclibs

import "errors"

type CDecl struct{}

func LoadLibrary(name string) (uintptr, error) {
	return 0, errors.ErrUnsupported
}

func LoadSystemLibrary(name string) (uintptr, error) {
	return 0, errors.ErrUnsupported
}

func UnloadLibrary(uintptr) error {
	return errors.ErrUnsupported
}

func RegisterLibFunc(fptr any, handle uintptr, name string) {
	panic(errors.ErrUnsupported)
}

func NewCallback(fptr any) uintptr {
	panic(errors.ErrUnsupported)
}
