// compute.go —— fn / asyncFn 计算列的 Go 回调桥。
//
// core 契约（core/src/computes/registry.rs）：schema 里以 `"fn": true` /
// `"asyncFn": true`（fnRef 指定回调标识，缺省 key 名）声明，执行体由宿主实现
// FnRegistry 后传入。本文件即 Go 侧执行体注册表 + C 回调 trampoline。
package gostore

import (
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Context 权限上下文（与 core permission::Context 的 JSON 契约一致）。
type Context struct {
	UserID   string   `json:"userId,omitempty"`
	Roles    []string `json:"roles,omitempty"`
	Role     string   `json:"role,omitempty"`
	Internal bool     `json:"internal,omitempty"`
}

// AsyncComputeFunc 批量 asyncFn 计算列（推荐形态：一次 FFI 往返处理一批文档，
// docs 原地改写）。返回 error 时整个查询显式失败。
type AsyncComputeFunc func(ctx *Context, docs []map[string]any) error

// SyncComputeFunc 逐文档同步 fn 计算列（返回该计算列的值）。
type SyncComputeFunc func(ctx *Context, doc map[string]any) (any, error)

type computeTable struct {
	mu    sync.RWMutex
	async map[string]AsyncComputeFunc
	syncF map[string]SyncComputeFunc
}

func (t *computeTable) getAsync(fnRef string) (AsyncComputeFunc, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	f, ok := t.async[fnRef]
	return f, ok
}

func (t *computeTable) getSync(fnRef string) (SyncComputeFunc, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	f, ok := t.syncF[fnRef]
	return f, ok
}

// handle -> table（core Registry 句柄与计算列执行体的关联）
var computeRegistries sync.Map

// installComputeBridge 把 Go trampoline 注册到 core-ffi（进程级一次性）。
func installComputeBridge() {
	cb := purego.NewCallback(computeTrampoline)
	freeFP := purego.NewCallback(freeTrampoline)
	rcoreSetComputeCB(cb, freeFP)
}

// computeTrampoline 对应 core-ffi 的 GoComputeFn：
// `fn(fn_ref: *const c_char, kind: c_int, payload: *const c_char) -> *mut c_char`
//   - kind 0：同步 fn，payload `{"doc":...,"ctx":...}` → 返回 `{"value":...}` 或 `{"error":"..."}`
//   - kind 1：批量 asyncFn，payload `{"docs":[...],"ctx":...}` → 返回 `{"docs":[...]}` 或 `{"error":"..."}`
//
// 返回值为非托管堆缓冲区（Rust 复制后调 freeTrampoline 释放）。
// 参数定长 uintptr：Windows 的 syscall.NewCallback 不支持变参函数（变参 slice > uintptr）。
func computeTrampoline(fnRefPtr, kind, payloadPtr uintptr) uintptr {
	fnRef := cToGoString(unsafe.Pointer(fnRefPtr))
	payload := cToGoString(unsafe.Pointer(payloadPtr))

	resp, err := dispatchCompute(fnRef, kind, payload)
	if err != nil {
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		resp = string(b)
	}
	p, allocErr := allocCBytes(resp)
	if allocErr != nil {
		// 分配失败无法上报，只能返回空指针；core 侧将其转为显式错误
		return 0
	}
	return uintptr(p)
}

func freeTrampoline(p uintptr) uintptr {
	globalFree(p)
	return 0
}

// dispatchCompute 按句柄表路由到注册的 Go 执行体；未注册/参数不一致一律显式报错
//（错误文本进入 `{"error":...}`，由 core 的 FnRegistry 桥转成查询失败）。
func dispatchCompute(fnRef string, kind uintptr, payload string) (string, error) {
	var pl struct {
		Handle uint64           `json:"handle"`
		Doc    map[string]any   `json:"doc"`
		Docs   []map[string]any `json:"docs"`
		Ctx    *Context         `json:"ctx"`
	}
	if err := json.Unmarshal([]byte(payload), &pl); err != nil {
		return "", fmt.Errorf("计算列 %s 回调 payload 非法: %w", fnRef, err)
	}
	var table *computeTable
	if v, ok := computeRegistries.Load(pl.Handle); ok {
		table = v.(*computeTable)
	}
	if table == nil {
		return "", fmt.Errorf("计算列 %s 无对应的 Go 注册表（句柄 %d）", fnRef, pl.Handle)
	}

	switch kind {
	case 0:
		f, ok := table.getSync(fnRef)
		if !ok {
			return "", fmt.Errorf("计算列 %s 未注册同步实现（RegisterSyncCompute）", fnRef)
		}
		v, err := f(pl.Ctx, pl.Doc)
		if err != nil {
			return "", fmt.Errorf("计算列 %s 执行失败: %w", fnRef, err)
		}
		b, mErr := json.Marshal(map[string]any{"value": v})
		if mErr != nil {
			return "", fmt.Errorf("计算列 %s 返回值序列化失败: %w", fnRef, mErr)
		}
		return string(b), nil
	case 1:
		f, ok := table.getAsync(fnRef)
		if !ok {
			return "", fmt.Errorf("计算列 %s 未注册批量实现（RegisterCompute）", fnRef)
		}
		if err := f(pl.Ctx, pl.Docs); err != nil {
			return "", fmt.Errorf("计算列 %s 执行失败: %w", fnRef, err)
		}
		b, mErr := json.Marshal(map[string]any{"docs": pl.Docs})
		if mErr != nil {
			return "", fmt.Errorf("计算列 %s 结果序列化失败: %w", fnRef, mErr)
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("计算列 %s 回调 kind 非法: %d", fnRef, kind)
	}
}
