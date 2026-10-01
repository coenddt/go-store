// util.go —— Windows 非托管堆工具（GlobalAlloc/GlobalFree，回调返回缓冲区的生命周期锚点）。
package gostore

import (
	"syscall"
)

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procGlobalAlloc  = modkernel32.NewProc("GlobalAlloc")
	procGlobalFree   = modkernel32.NewProc("GlobalFree")
)

// gmemFixed：固定地址分配（返回值即内存地址，不可被系统移动）。
const gmemFixed = 0x0000
