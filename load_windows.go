// load_windows.go —— Windows 的 cdylib 加载与符号解析。
// purego 的 Dlopen 明确不支持 Windows（见其 dlfcn.go 文档），官方指引用
// windows.LoadLibrary / GetProcAddress + RegisterFunc 组合。
package gostore

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func loadLibraryAt(path string) (uintptr, error) {
	h, err := windows.LoadLibrary(path)
	if err != nil {
		return 0, err
	}
	return uintptr(h), nil
}

func symbolAt(h uintptr, name string) (uintptr, error) {
	p, err := windows.GetProcAddress(windows.Handle(h), name)
	if err != nil || p == 0 {
		return 0, fmt.Errorf("GetProcAddress(%s): %w", name, err)
	}
	return p, nil
}
