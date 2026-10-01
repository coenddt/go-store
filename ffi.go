// ffi.go —— rust-store/core-ffi 的 purego 绑定与 FFI 信封解析。
package gostore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// LibraryPath 允许在首次使用前覆盖 cdylib 路径（默认自动探测构建产物，
// 或环境变量 RUST_STORE_FFI）。
var LibraryPath string

var (
	rcoreFree              func(uintptr)
	rcoreSetComputeCB      func(fp, freeFP uintptr) int32
	rcoreRegistryNew       func(requireContext int32) uint64
	rcoreRegistryDrop      func(handle uint64)
	rcoreRegistryRegister  func(handle uint64, defn unsafe.Pointer) unsafe.Pointer
	rcoreRegistryList      func(handle uint64) unsafe.Pointer
	rcorePlanQuery         func(handle uint64, gql, params, ctx unsafe.Pointer) unsafe.Pointer
	rcorePlanQueryOne      func(handle uint64, gql, params, ctx unsafe.Pointer) unsafe.Pointer
	rcorePlanQueryWithCnt  func(handle uint64, gql, params, ctx unsafe.Pointer) unsafe.Pointer
	rcorePlanInsert        func(handle uint64, schema, ctx, data unsafe.Pointer, now int64, newID unsafe.Pointer) unsafe.Pointer
	rcorePlanUpdate        func(handle uint64, schema, ctx, cond, data unsafe.Pointer, now int64, probe unsafe.Pointer) unsafe.Pointer
	rcorePlanRemove        func(handle uint64, schema, ctx, cond, probe unsafe.Pointer) unsafe.Pointer
	rcorePlanArchiveDocs   func(handle uint64, schema, docs unsafe.Pointer, now int64) unsafe.Pointer
	rcoreTranslate         func(handle uint64, backend, cmd unsafe.Pointer) unsafe.Pointer
	rcoreFinalizePrepare   func(handle uint64, post, items, ctx unsafe.Pointer) unsafe.Pointer
	rcoreFinalizeStrip     func(post, items unsafe.Pointer) unsafe.Pointer
	rcoreRestoreSortOrder  func(items, ids, sort unsafe.Pointer) unsafe.Pointer
	rcoreRestoreRowsJSON   func(rowShape, rows unsafe.Pointer) unsafe.Pointer
)

var initOnce sync.Once
var initErr error

// load 初始化 cdylib（幂等；首次失败后续调用返回同一错误）。
func load() error {
	initOnce.Do(func() {
		path := LibraryPath
		if path == "" {
			path = defaultLibraryPath()
		}
		h, err := loadLibraryAt(path)
		if err != nil {
			initErr = fmt.Errorf("加载 rust-store cdylib 失败（路径 %q；应先构建 core-ffi，见 README）: %w", path, err)
			return
		}
		bind := func(dst any, name string) {
			addr, err := symbolAt(h, name)
			if err != nil {
				initErr = fmt.Errorf("cdylib 缺少导出符号 %s: %w", name, err)
				return
			}
			purego.RegisterFunc(dst, addr)
		}
		bind(&rcoreFree, "rcore_free")
		bind(&rcoreSetComputeCB, "rcore_set_compute_callback")
		bind(&rcoreRegistryNew, "rcore_registry_new")
		bind(&rcoreRegistryDrop, "rcore_registry_drop")
		bind(&rcoreRegistryRegister, "rcore_registry_register")
		bind(&rcoreRegistryList, "rcore_registry_list")
		bind(&rcorePlanQuery, "rcore_plan_query")
		bind(&rcorePlanQueryOne, "rcore_plan_query_one")
		bind(&rcorePlanQueryWithCnt, "rcore_plan_query_with_count")
		bind(&rcorePlanInsert, "rcore_plan_insert")
		bind(&rcorePlanUpdate, "rcore_plan_update")
		bind(&rcorePlanRemove, "rcore_plan_remove")
		bind(&rcorePlanArchiveDocs, "rcore_plan_archive_docs")
		bind(&rcoreTranslate, "rcore_translate")
		bind(&rcoreFinalizePrepare, "rcore_finalize_prepare")
		bind(&rcoreFinalizeStrip, "rcore_finalize_strip")
		bind(&rcoreRestoreSortOrder, "rcore_restore_sort_order")
		bind(&rcoreRestoreRowsJSON, "rcore_restore_rows_json")
		if initErr != nil {
			return
		}
		installComputeBridge()
	})
	return initErr
}

// defaultLibraryPath 探测 core-ffi 构建产物（release 优先于 debug；支持 RUST_STORE_FFI 覆盖）。
func defaultLibraryPath() string {
	if v := os.Getenv("RUST_STORE_FFI"); v != "" {
		return v
	}
	cands := []string{
		"../rust-store/target/release/rust_store_ffi.dll",
		"../rust-store/target/debug/rust_store_ffi.dll",
		"../../rust-store/target/release/rust_store_ffi.dll",
		"../../rust-store/target/debug/rust_store_ffi.dll",
		"rust_store_ffi.dll",
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return cands[0]
}

// ffiEnvelope 是所有 rcore_* 出口的统一信封。
type ffiEnvelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

// callFFI 调用入口返回值：复制 C 字符串 → rcore_free 释放 → 解析信封。
// 返回 data 的原始 JSON 文本（RawMessage 保真，int64 无精度损失）；
// ok=false 时把 error 字段原样上浮为 Go error（禁静默失守）。
func callFFI(ret unsafe.Pointer) (json.RawMessage, error) {
	if ret == nil {
		return nil, errors.New("FFI 入口返回空指针（信封序列化失败）")
	}
	s := cToGoString(ret)
	rcoreFree(uintptr(ret))
	var env ffiEnvelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return nil, fmt.Errorf("FFI 信封非法: %w", err)
	}
	if !env.OK {
		return nil, errors.New(env.Error)
	}
	return env.Data, nil
}

// callFFIInto 解析 data 到 out（out 为 nil 时丢弃）。
func callFFIInto(ret unsafe.Pointer, out any) error {
	data, err := callFFI(ret)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// cToGoString 复制 Rust 侧返回的 NUL 结尾 UTF-8 C 字符串。
func cToGoString(p unsafe.Pointer) string {
	if p == nil {
		return ""
	}
	buf := make([]byte, 0, 64)
	q := uintptr(p)
	for {
		b := *(*byte)(unsafe.Pointer(q))
		if b == 0 {
			return string(buf)
		}
		buf = append(buf, b)
		q++
	}
}

// cArgFor 序列化 v 为分配在 Windows 非托管堆的 NUL 结尾 C 字符串。
// 非托管堆（GlobalAlloc GMEM_FIXED）保证地址固定、不经 Go GC，
// 跨 FFI 边界生命周期最稳；调用方 defer freeCArg 配对释放。
func cArgFor(v any) (unsafe.Pointer, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("FFI 入参序列化失败: %w", err)
	}
	return allocCBytes(string(b))
}

// rawCFor 裸字符串通道：不 JSON 序列化，原文传 C（对应 Rust 侧 cstr() 裸读的参数：
// gql、schema 名、new_id、backend）。与 JSON 通道（cArgFor ↔ json_arg）严格配对，
// 错用会导致接收端多出一对引号（表现为「Schema 未注册: "user"」）。
func rawCFor(s string) (unsafe.Pointer, error) {
	return allocCBytes(s)
}

func mustCArg(v any) unsafe.Pointer {
	p, err := cArgFor(v)
	if err != nil {
		panic(err) // 仅序列化不可失败的基础类型时使用
	}
	return p
}

func freeCArg(p unsafe.Pointer) {
	if p != nil {
		globalFree(uintptr(p))
	}
}

// allocCBytes 分配 NUL 结尾副本到非托管堆。
func allocCBytes(s string) (unsafe.Pointer, error) {
	b := make([]byte, len(s)+1)
	copy(b, s)
	h, _, callErr := procGlobalAlloc.Call(gmemFixed, uintptr(len(b)))
	if h == 0 {
		return nil, fmt.Errorf("GlobalAlloc 失败: %v", callErr)
	}
	for i := range b {
		*(*byte)(unsafe.Pointer(h + uintptr(i))) = b[i]
	}
	return unsafe.Pointer(h), nil
}

func globalFree(p uintptr) {
	if p != 0 {
		procGlobalFree.Call(p)
	}
}
