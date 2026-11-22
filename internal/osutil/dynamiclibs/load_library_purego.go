//go:build linux && !nopurego && (amd64 || arm64 || 386 || arm || loong64 || ppc64le || riscv64 || s390x)

package dynamiclibs

import (
	"path/filepath"

	"github.com/ebitengine/purego"
)

type CDecl = purego.CDecl

func LoadLibrary(name string) (handle uintptr, err error) {
	if !filepath.IsAbs(name) && filepath.Ext(name) != ".so" {
		name += ".so"
	}

	// RTLD_NOW to gather as much errors as possible right now
	return purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_LOCAL)
}

func LoadSystemLibrary(name string) (handle uintptr, err error) {
	return purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}

func UnloadLibrary(handle uintptr) error {
	return purego.Dlclose(handle)
}

func RegisterLibFunc(fptr any, handle uintptr, name string) {
	purego.RegisterLibFunc(fptr, handle, name)
}

func NewCallback(fptr any) uintptr {
	return purego.NewCallback(fptr)
}
