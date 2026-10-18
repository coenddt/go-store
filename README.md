# go-store

**`rust-store-core` 的 Go 宿主绑定** —— `nodejs-store` / `py-store` / `rust-store(host)` 的 Go 孪生。用一份纯 JSON schema 定义模型、用 MongoDB 风格的 GQL 树形语法查询，内置 RBAC、计算列与软删除归档。当前 **v0：后端仅 SQLite**。

> **发布状态**：本仓库为纯 Go 实现，**未发布、无 tag、不参与 release-all 发版**（无 CI、无版本声明）。
> 语义只有一份（Rust core）；Go 宿主只做 IO 与命令序列编排，与 Node / Python 行为不漂移。

---

## 1. 状态与范围矩阵（v0）

| 能力 | v0 | 说明 |
|---|---|---|
| **后端：SQLite** | ✅ | DSN：`sqlite://path.db` / `sqlite::memory:`（另兼容驱动原生 `path.db` / `:memory:` / `file:...`） |
| 后端：MySQL / PostgreSQL / MongoDB / local | ❌ | 属后续阶段（与 `rust-store/host` v0 同策略） |
| `Open` / `Close` / `Register` / `SchemaFieldsKeys` / `ListSchemas` | ✅ | schema 注册与元数据 |
| 读：`Query` / `QueryOne` / `QueryWithCount` | ✅ | 含两阶段（关系展开）、聚合（core 规划） |
| 写：`Insert` / `Update` / `Remove` | ✅ | `Update` 探针重入；`Remove` 归档编排（返回计数） |
| 计算列 `fn`（同步回调桥） | ✅ | `RegisterSyncCompute` |
| 计算列 `asyncFn`（批量回调桥） | ✅ | `RegisterCompute`（推荐形态） |
| 计算列 `agg`（引擎内联） | ✅ | 由 core 内联求值，无需宿主回调（声明式，见 §10） |
| RBAC 配置注入与查询面 | ✅ | `SetRbac` / `SetExemptRoles` / `SetDenyWriteRoles` / `SetUnconfiguredPolicy` / `RbacCan` / `RbacReadableFields` / `RbacWritableFields` / `RbacRowCondition` |
| DDL：标量列 + timestamps + `__present` + 归档表 | ✅ | `EnsureTables` / `CreateTableStatements` |
| DDL：索引 / autoincrement 主键 / MySQL·PG 方言 | ❌ | 后续阶段（建表可由调用方脚本替代） |
| **事务 API** | ❌ | v0 无事务 API，详见 §11 |
| 跨平台（非 Windows 构建） | ❌ | 当前源码仅在 **Windows** 可编译（见 §6） |
| 触发器链 / 联邦 / mutation 步骤编排 | ❌ | 未纳入 v0（宿主层无相关编排代码） |

> 本表是**诚实声明**，不是路线图承诺。凡标 ❌ 者，调用会以显式错误或「功能不存在」的方式体现，绝不静默降级。

---

## 2. 目录（TOC）

1. [状态与范围矩阵（v0）](#1-状态与范围矩阵v0)
2. [What it is（是什么、三层架构中的位置）](#3-what-it-is是什么三层架构中的位置)
3. [When to use / When NOT to use](#4-when-to-use--when-not-to-use)
4. [Installation / 前置条件](#5-installation--前置条件)
5. [Quick start](#6-quick-start)
6. [API 参考](#7-api-参考)
7. [GQL 查询示例](#8-gql-查询示例)
8. [计算列与 DDL](#9-计算列与-ddl)
9. [事务边界](#10-事务边界)
10. [权限（RBAC 透传）](#11-权限rbac-透传)
11. [场景事例（6 个）](#12-场景事例)
12. [边界与非目标](#13-边界与非目标)
13. [FAQ](#14-faq)
14. [Development](#15-development)
15. [Related projects](#16-related-projects)
16. [License](#17-license)

---

## 3. What it is（是什么、三层架构中的位置）

go-store 是 common-store 家族的 **Go 宿主**。它不实现任何 GQL / 权限 / 计算列 / SQL 方言逻辑——这些全部位于 **rust-store/core**（Rust，无 IO、无时钟、无随机）。Go 侧只做三件事：

1. **驱动 IO**：用纯 Go SQLite 驱动执行 core 规划出的参数化 SQL；
2. **供给 `now` 与 `new_id`**：core 无时钟无随机源，插入时间戳与主键一律由宿主给出（`id.go`）；
3. **命令序列编排**：两阶段查询、探针重入、归档+删除等由宿主按 core 契约串联。

```
   go-store（本仓库）              rust-store/core-ffi            rust-store/core
 ┌────────────────────┐        ┌──────────────────────┐      ┌──────────────────────┐
 │ Go 宿主             │        │ C ABI（cdylib）       │      │ 语言无关核心           │
 │ IO + 编排            │ ─────▶ │ rcore_* 符号集        │ ───▶ │ GQL / 权限 / 计算列    │
 │ purego 运行时加载    │        │ JSON 信封            │      │ 命令规划 / SQL 方言    │
 │ （无 cgo）           │        │ {ok, data, error}    │      │ 无 IO / 无时钟 / 无随机 │
 └────────────────────┘        └──────────────────────┘      └──────────────────────┘
```

**绑定方式**：Go ↔ core-ffi 之间是 **purego 运行时加载**（`github.com/ebitengine/purego`），**不使用 cgo**——因此**不需要任何 C 工具链**。所有 `rcore_*` 出口都返回统一 JSON 信封 `{ok, data, error}`（见 `ffi.go` 的 `ffiEnvelope`）：`ok=false` 时把 `error` 字段**原样上浮为 Go error**，绝不静默失守；`ok=true` 时 `data` 以原始 JSON 文本保真（`json.Number` 不丢 int64 精度）。

与之对齐的家族成员：`rust-store/host`（纯 Rust 宿主，行为基准）、`nodejs-store`（Node 宿主）、`py-store`（Python 宿主）。四者共享同一套 GQL 与权限语义。

---

## 4. When to use / When NOT to use

### ✅ When to use

- **在 Go 服务里嵌入一个 schema 驱动的数据层**：模型用纯 JSON 声明，查询用 GQL，附带 RBAC、计算列、软删除归档，而不引入 ORM 与迁移引擎。
- **单文件 / 边缘部署**：SQLite + 纯 Go 驱动 + 单个动态库，无外部数据库服务。
- **需要与 Node / Python 端「同一套语义」**：同一份 schema、同一串 GQL 在 `nodejs-store` / `py-store` / `go-store` 上行为一致（语义只实现在 Rust core 一处）。
- **把权限 / 计算列逻辑从业务代码里挪出来**：写在 schema 里，由 core 在规划期执行与裁决。
- **本地开发与集成测试**：`sqlite::memory:` 起一个内存库，注册 → 建表 → 读写，零外部依赖。

### ❌ When NOT to use

- **需要 MySQL / PostgreSQL / MongoDB**：v0 只有 SQLite，换后端会得到显式报错（见 §12 事例 6）。
- **需要完整事务 API**：v0 未提供，见 §10。
- **需要非 Windows 构建**：当前源码仅在 Windows 可编译，见 §5。
- **需要编译期类型安全 / 迁移引擎**：schema 是运行时 JSON map，不是 Go 结构体；go-store 也不是迁移工具。
- **只用单一数据库且几乎不做关系查询**：直接用 `database/sql` + 驱动更简单。

---

## 5. Installation / 前置条件

### 模块路径与引入

```
module github.com/coenddt/go-store   // go.mod 声明 go 1.26.0
package gostore                       // 包名 gostore
```

```go
import gostore "github.com/coenddt/go-store"
```

### 前置条件

| 前置 | 要求 | 来源 |
|---|---|---|
| Go | `go 1.26.0`（`go.mod` 声明） | `go.mod` |
| **C 工具链 / cgo** | **不需要** | `ffi.go` 用 purego 运行时加载 |
| SQLite 驱动 | 纯 Go 的 `modernc.org/sqlite`（注册驱动名 `"sqlite"`），无 C 依赖 | `exec.go` |
| core-ffi 产物 | cdylib `rust_store_ffi.dll`（crate `rust-store-ffi` 0.1.0，`crate-type=["cdylib"]`） | `rust-store/core-ffi` |
| 运行 OS | **Windows**（见下） | `load_windows.go` / `util.go` |

**构建 core-ffi 产物**（在 `rust-store/` 目录）：

```bash
cargo build -p rust-store-ffi --release   # → target/release/rust_store_ffi.dll
# 或 debug：cargo build -p rust-store-ffi  → target/debug/rust_store_ffi.dll
```

**库路径探测顺序**（`ffi.go` 的 `defaultLibraryPath`）：

1. 环境变量 `RUST_STORE_FFI`；
2. `../rust-store/target/release/rust_store_ffi.dll`
3. `../rust-store/target/debug/rust_store_ffi.dll`
4. `../../rust-store/target/release/rust_store_ffi.dll`
5. `../../rust-store/target/debug/rust_store_ffi.dll`
6. 当前目录下的 `rust_store_ffi.dll`

也可在**首次使用前**直接赋值包级变量 `gostore.LibraryPath` 覆盖路径。加载是**进程级幂等**的（`load()` 用 `sync.Once`，首次失败后每次调用返回同一错误）。

**Windows 加载机制**（`load_windows.go` / `util.go`）：purego 的 `Dlopen` 不支持 Windows，故改用 `windows.LoadLibrary` + `windows.GetProcAddress` 解析符号，再用 `purego.RegisterFunc` 绑定；跨 FFI 的字符串缓冲区分配在 Windows 非托管堆（`GlobalAlloc`/`GlobalFree`），保证地址固定、不经 Go GC。

> ⚠️ **跨平台边界（已核实）**：源码只有 `load_windows.go`（无 unix 加载器），且 `util.go` 依赖 `kernel32.dll` 与 `syscall.NewLazyDLL`。因此**当前 go-store 只能在 Windows 上编译运行**；Linux / macOS 支持属后续阶段（需补一个非 Windows 的加载器实现）。

---

## 6. Quick start

最小可运行示例：打开内存库 → 注册 schema → 建表 → 插入 → GQL 查询 → 读结果。

```go
package main

import (
	"context"
	"fmt"
	"log"

	gostore "github.com/coenddt/go-store"
)

func main() {
	// 打开 SQLite（内存库；落盘用 "sqlite://app.db"）
	store, err := gostore.Open("sqlite::memory:")
	if err != nil {
		log.Fatal(err) // 加载 cdylib 失败也会在此暴露
	}
	defer store.Close()

	// 纯 JSON schema（与 nodejs-store 同一契约）
	userSchema := map[string]any{
		"name":       "user",
		"collection": "user",
		"idPrefix":   "u",
		"fields": map[string]any{
			"name": map[string]any{"type": "string"},
			"age":  map[string]any{"type": "number"},
		},
	}
	if err := store.Register(userSchema); err != nil { // 注册进 core registry
		log.Fatal(err)
	}
	if err := store.EnsureTables(userSchema); err != nil { // 建主表 + 归档表
		log.Fatal(err)
	}

	ctx := context.Background()

	// 插入：返回 core 产出的 returns 文档（含 _id / createdAt）
	doc, err := store.Insert(ctx, "user", map[string]any{"name": "alice", "age": int64(30)}, nil)
	if err != nil {
		log.Fatal(err)
	}

	// GQL 查询：值经命名参数 @c0 从 params 取
	rows, err := store.Query(ctx, "user($condition: @c0) { name, age }",
		map[string]any{"c0": map[string]any{"age": map[string]any{"$gte": 18}}}, nil)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(doc["_id"]) // 例：uXXXXXXXXXXXXXX（u + base36 毫秒 + 8 位随机）
	fmt.Println(rows)       // [map[age:30 name:alice]]
}
```

> 第四个参数 `nil` 是权限上下文 `*gostore.Context`（无上下文 = 不做权限裁剪）。
> 注意不要把方法入参 `ctx context.Context`（用于取消/超时）与 `*gostore.Context`（权限上下文）混淆——两者是不同类型、不同用途。

---

## 7. API 参考

以下均为 **`go-store` 真实导出**（`store.go` / `exec.go` / `ddl.go` / `compute.go` / `id.go` / `ffi.go`）。签名逐字取自源码。

### 7.1 类型与包级变量

| 名称 | 定义 | 用途 |
|---|---|---|
| `Store` | `type Store struct{ ... }` | 宿主句柄。**零值不可用**，须经 `Open` 创建 |
| `Context` | `type Context struct { UserID string; Roles []string; Role string; Internal bool }` | 权限上下文；JSON 契约 `userId` / `roles` / `role` / `internal`（对应 core `permission::Context`） |
| `AsyncComputeFunc` | `func(ctx *Context, docs []map[string]any) error` | 批量 `asyncFn` 计算列执行体（`docs` 原地改写） |
| `SyncComputeFunc` | `func(ctx *Context, doc map[string]any) (any, error)` | 逐文档同步 `fn` 计算列执行体（返回该列的值） |
| `LibraryPath` | `var LibraryPath string` | 首次使用前覆盖 cdylib 路径 |

### 7.2 生命周期与 schema

| 方法 | 签名 | 用途 / 返回值 |
|---|---|---|
| `Open` | `func Open(dsn string) (*Store, error)` | 连接 SQLite。内部 `MaxOpenConns(1)`（SQLite 单写者模型）；空 DSN 显式报错 `SQLite 连接串为空` |
| `Close` | `func (s *Store) Close() error` | 释放 core Registry 句柄与连接池 |
| `Register` | `func (s *Store) Register(defn map[string]any) error` | 注册 schema（与 `nodejs-store` 同 JSON 契约），core 自动派生 `<Name>Deleted` 归档表；`idPrefix` 与 fields 键同步缓存到宿主侧 |
| `SchemaFieldsKeys` | `func (s *Store) SchemaFieldsKeys(schema string) []string` | 已注册 schema 的 fields 键列表（**字母序**；未注册返回 `nil`）。供适配层拼 GQL 投影段 |
| `ListSchemas` | `func (s *Store) ListSchemas() ([]string, error)` | 已注册 schema 名列表 |

### 7.3 读路径

| 方法 | 签名 | 用途 / 返回值 |
|---|---|---|
| `Query` | `func (s *Store) Query(ctx context.Context, gql string, params map[string]any, actx *Context) ([]map[string]any, error)` | 执行 GQL 查询，返回文档列表（含两阶段关系展开、聚合） |
| `QueryOne` | `func (s *Store) QueryOne(ctx context.Context, gql string, params map[string]any, actx *Context) (map[string]any, error)` | 取第一条（core 自动注入 `$limit(1)`）；**未命中返回 `(nil, nil)`** |
| `QueryWithCount` | `func (s *Store) QueryWithCount(ctx context.Context, gql string, params map[string]any, actx *Context) (map[string]any, error)` | 查询 + 计数，返回 `{items, total, hasMore, page, pageSize}`。分页参数经 `params` 传 `page`/`pageSize`（`page` 默认 0，`pageSize` 默认 50、上限 5000），或由 GQL 的 `$skip`/`$limit` 反推；`total` 忽略分页但含权限注入的 owner 条件 |

### 7.4 写路径

| 方法 | 签名 | 用途 / 返回值 |
|---|---|---|
| `Insert` | `func (s *Store) Insert(ctx context.Context, schema string, data map[string]any, actx *Context) (map[string]any, error)` | 插入一条，返回 core 产出的 `returns` 文档；autoincrement 主键回读后补入 `_id` |
| `Update` | `func (s *Store) Update(ctx context.Context, schema string, condition, data map[string]any, actx *Context) (map[string]any, error)` | 部分更新（探针重入编排）；**条件命中返回更新后文档，未命中返回 `(nil, nil)`** |
| `Remove` | `func (s *Store) Remove(ctx context.Context, schema string, condition map[string]any, actx *Context) (map[string]any, error)` | 删除 + 归档编排（探针 → 关系谓词 `preCommand` → find 源文档 → 归档 → 删除）；返回 `{deletedCount, archivedCount}`（均为 `int64`） |

### 7.5 DDL

| 方法 / 函数 | 签名 | 用途 / 返回值 |
|---|---|---|
| `EnsureTables` | `func (s *Store) EnsureTables(defn map[string]any) error` | 为 schema 建主表与归档表（`IF NOT EXISTS`，幂等） |
| `CreateTableStatements` | `func CreateTableStatements(defn map[string]any) ([]string, error)` | **纯函数**：生成主表 + 归档表的 `CREATE TABLE` 语句，不连库、不回写 |

### 7.6 计算列注册

| 方法 | 签名 | 用途 |
|---|---|---|
| `RegisterCompute` | `func (s *Store) RegisterCompute(fnRef string, fn AsyncComputeFunc)` | 注册**批量 `asyncFn`** 执行体（推荐：一次 FFI 往返处理一批） |
| `RegisterSyncCompute` | `func (s *Store) RegisterSyncCompute(fnRef string, fn SyncComputeFunc)` | 注册**逐文档同步 `fn`** 执行体（跨 FFI 回调桥） |

### 7.7 RBAC（透传配置与查询面；**判决唯一在 core，本层零判决**）

| 方法 | 签名 | 用途 / 返回值 |
|---|---|---|
| `SetRbac` | `func (s *Store) SetRbac(policy map[string]any) error` | 注入 RBAC 策略（`nil` = 清除关闭）；解析失败显式报错（fail-fast） |
| `SetExemptRoles` | `func (s *Store) SetExemptRoles(roles []string) error` | 注入豁免角色清单（命中者一切判决放行；默认空 = 无豁免） |
| `SetDenyWriteRoles` | `func (s *Store) SetDenyWriteRoles(roles []string) error` | 注入拒写角色清单（命中者一切写路径拒绝，读不受影响） |
| `SetUnconfiguredPolicy` | `func (s *Store) SetUnconfiguredPolicy(policy string) error` | 未配置姿态（`"open"` / `"closed"`）；未知值由 core 显式报错 |
| `RbacEnabled` | `func (s *Store) RbacEnabled() (bool, error)` | 策略是否已注入 |
| `RbacCan` | `func (s *Store) RbacCan(model, action string, actx *Context) (bool, error)` | 动作判决，`action ∈ {read, insert, update, remove}`；策略未注入 / RBAC 不介入 → `true` |
| `RbacReadableFields` | `func (s *Store) RbacReadableFields(model string, actx *Context) ([]string, error)` | RBAC 叠加后可读字段集（静态 ∩ readFields；空 ctx → `nil` 不裁剪） |
| `RbacWritableFields` | `func (s *Store) RbacWritableFields(model string, actx *Context) ([]string, error)` | RBAC 叠加后可写字段集（静态 ∩ writeFields；空 ctx → `nil` 不裁剪） |
| `RbacRowCondition` | `func (s *Store) RbacRowCondition(model, action string, actx *Context) (map[string]any, error)` | 行级条件（ownerOnly/condition 的 OR 合并体）；`action ∈ {read, update, remove}`（insert 无行级语义）；`nil` = 无行级收紧 |

> **家族对齐命名**：`QueryWithCount` 对应 Node 的 `queryWithCount` / Python 的 `query_with_count`（出参契约一致）。
> 顶层 API/类型仅以上述为准——**不存在** `Exists` / `Count` / `InsertMany` / `UpdateMany` / `Ask` / `Transaction` / `SyncSchema` 等方法（在 Go 侧未导出）。

---

## 8. GQL 查询示例

GQL 是 MongoDB 风格的树形语法：值通过 `@key` 从 `params` 引用；关系在 schema 的 `relations` 中声明，由 core 编译为单条原生查询。以下 GQL 与 Go 调用均可在 v0（SQLite）下运行。

### 8.1 条件 + 命名参数 + 投影

```go
rows, _ := store.Query(ctx, "user($condition: @c0) { name, age }",
    map[string]any{"c0": map[string]any{"age": map[string]any{"$gte": 18}}}, nil)
// → []map[string]any{{"name":"alice","age":30}}
```

### 8.2 精确取一条

```go
one, _ := store.QueryOne(ctx, "user($condition: @c0) { name, age }",
    map[string]any{"c0": map[string]any{"_id": uid}}, nil)
// 命中 → map；未命中 → nil（err 仍为 nil）
```

### 8.3 排序 / 跳过 / 取前 N

```go
rows, _ := store.Query(ctx, "user($sort: @s0, $skip: @sk, $limit: @l0) { name, age }",
    map[string]any{"s0": map[string]any{"age": -1}, "sk": 0, "l0": 20}, nil)
```

### 8.4 分页 + 总数（`QueryWithCount`）

```go
page, _ := store.QueryWithCount(ctx, "user", map[string]any{"page": 1, "pageSize": 2}, nil)
// → {"items":[...2 条...], "total":5, "hasMore":true, "page":1, "pageSize":2}
page, _ = store.QueryWithCount(ctx, "user", map[string]any{"page": 2, "pageSize": 2}, nil)
// → hasMore=false（(page+1)*pageSize >= total）
```

### 8.5 关系展开（两阶段）

先声明关系（与 `nodejs-store` 同契约），再在投影里引用关系名：

```go
orderSchema := map[string]any{
    "name":       "order",
    "collection": "orders",
    "idPrefix":   "o",
    "fields":     map[string]any{"status": map[string]any{"type": "string"}},
    "relations": map[string]any{
        "items": map[string]any{
            "model": "order_item", "type": "many",
            "localField": "_id", "foreignField": "orderId",
        },
    },
}
// 被引用的关系表同样需 Register + EnsureTables（各自 schema）
rows, _ := store.Query(ctx, "order($condition: @c0) { _id, status, items { title } }",
    map[string]any{"c0": map[string]any{"status": "paid"}}, nil)
```

> core 会把「关系展开」规划为两阶段（two_phase）：宿主执行第一段取 `_id`，替换占位符 `{{phase1.ids}}` 后执行第二段，再调 core 还原原始排序（`executePlan` / `restoreSortOrder`）。**无需手写 `$lookup` 或 JOIN**。

### 8.6 分组聚合（core 语义）

```go
rows, _ := store.Query(
    "order($condition: @c0, $group: @g0, $having: @h0, $sort: @s0, $limit: @l0) { status, n, total }",
    map[string]any{
        "c0": map[string]any{"status": map[string]any{"$ne": "deleted"}},
        "g0": map[string]any{"by": []any{"status"},
            "agg": map[string]any{"n": map[string]any{"$count": "*"}, "total": map[string]any{"$sum": "amount"}}},
        "h0": map[string]any{"n": map[string]any{"$gt": 1}},
        "s0": map[string]any{"total": -1},
        "l0": 20,
    }, nil)
```

固定执行顺序：`$condition`(WHERE) → `$group`(GROUP BY) → `$having`(HAVING) → `$sort` → `$skip`/`$limit` → 投影。白名单算子：`$count` / `$sum` / `$avg` / `$min` / `$max`。语义细节以 rust-store 为准（见 §16）。

### 8.7 不可下推 → 显式报错（不静默降级）

当命令包含无法安全翻译的组合时，`translateCommand` **拒绝执行**并返回错误（`exec.go`）：

```
命令包含无法安全下推的组合（unsupported = [...]）：拒绝执行，请改写查询或换用 MongoDB 源
```

查询未注册的 schema 同样显式报错（见测试 `TestUnsupportedExplicitError`）。

---

## 9. 计算列与 DDL

### 9.1 计算列（`fn` / `asyncFn`）

在 schema 的 `computes` 中声明，执行体由宿主实现（`compute.go` 的回调桥）：

- `{"fn": true, "fnRef": "<name>"}` → `RegisterSyncCompute`（逐文档同步，回调 kind=0）
- `{"asyncFn": true, "fnRef": "<name>"}` → `RegisterCompute`（批量异步，回调 kind=1，**推荐**）
- `fnRef` 缺省为计算列 key 名。

```go
schema := map[string]any{
    "name": "order", "collection": "orders", "idPrefix": "o",
    "fields": map[string]any{
        "status": map[string]any{"type": "string"},
        "amount": map[string]any{"type": "number"},
    },
    "computes": map[string]any{
        "statusUpper": map[string]any{"type": "string", "fn": true, "fnRef": "goUpper"},
        "amountAud":   map[string]any{"type": "number", "asyncFn": true, "fnRef": "goAud"},
    },
}
_ = store.Register(schema)
_ = store.EnsureTables(schema)

// 同步 fn：逐文档，返回该列的值
store.RegisterSyncCompute("goUpper", func(ctx *gostore.Context, doc map[string]any) (any, error) {
    v, _ := doc["status"].(string)
    return v + "!", nil
})
// asyncFn：一次处理一批，原地改写 docs
store.RegisterCompute("goAud", func(ctx *gostore.Context, docs []map[string]any) error {
    for _, d := range docs {
        if amt, ok := d["amount"].(float64); ok {
            d["amountAud"] = amt * 15000
        }
    }
    return nil
})

// 查询时按名请求即可
rows, _ := store.Query(ctx, "order { status, statusUpper, amount, amountAud }", nil, nil)
// status="paid", amount=2.5 → statusUpper="paid!", amountAud=37500
```

**铁律**：读路径后处理分三段——① `prepare`（core 内联同步 fn + 默认值 + 权限裁剪，返回待宿主执行的 `asyncFn` 的 `fnRef` 列表）；② 宿主逐 `fnRef` 执行 Go 批量回调；③ `strip`（剥离依赖注入字段）。**回调失败即整个查询失败**，错误带上 `fnRef` 语义上浮，**绝不静默置空**：

- asyncFn 未注册 → `计算列 <fnRef> 未注册异步实现（RegisterCompute）`
- sync fn 未注册 → `计算列 <fnRef> 未注册同步实现（RegisterSyncCompute）`

> `agg` 类型（关系滚动聚合，如 `{"$count": "<relation>"}`）由 **core 内联求值**，无需宿主回调；声明方式见 rust-store README（§16）。

### 9.2 DDL（`EnsureTables` / `CreateTableStatements`）

`CreateTableStatements(defn)` 是**纯函数**（不连库、不回写），生成两条语句：主表 + 归档表 `<collection>_deleted`。`EnsureTables` 只是逐条执行它们。

**类型映射**（`ddl.go`，与 `nodejs-store/src/ddl.js` 的 SQLite 分支一致）：

| schema 声明 `type` | SQLite 列类型 |
|---|---|
| `string` | `TEXT` |
| `int` / `long` / `number` | `INTEGER` |
| `float` / `double` | `REAL` |
| `bool` / `boolean` | `INTEGER` |
| `datetime` / `date` | `INTEGER` |
| `object` / `array` | `TEXT`（JSON 文本列，读侧由 core 还原） |

规则：

- 主键固定为 `"_id" TEXT PRIMARY KEY`（autoincrement 主键属后续阶段；字段名为 `_id` 时跳过重复建列）。
- 每表必建 `"__present" TEXT` 哨兵列（区分「显式 null」与「字段缺失」，core dialect 物理契约）。
- `timestamps !== false`（默认 `true`）→ 追加 `createdAt` / `updatedAt`（`INTEGER` 毫秒）；归档表额外带 `deletedAt`。
- 字段名排序后输出，保证 DDL 文本确定。
- 未在类型表内的 `type` → 显式报错 `字段 <f> 类型 <t> 不在 SQLite v0 类型表内`。

```go
stmts, err := gostore.CreateTableStatements(schema) // []string{主表, 归档表}
// 例：
// CREATE TABLE IF NOT EXISTS "orders" ("_id" TEXT PRIMARY KEY, "amount" INTEGER, "status" TEXT, "createdAt" INTEGER, "updatedAt" INTEGER, "__present" TEXT)
// CREATE TABLE IF NOT EXISTS "orders_deleted" (..., "deletedAt" INTEGER)
```

**v0 未含**：`schema.indexes` 索引 DDL、autoincrement 主键、MySQL/PG 方言。建表也可由调用方脚本替代。

---

## 10. 事务边界

**v0 无事务 API**（源码中不存在 `Transaction` / `Session` / `Begin` 等方法或符号）。

- `Insert` / `Update` / `Remove` 各自是**单条语句**级别的操作。
- `Remove` 的**归档 + 删除**编排（`preCommand` → find 源文档 → `plan_archive_docs` → delete）在同一 `*sql.Conn` 上**顺序执行**，但**未包裹在显式数据库事务里**——源码中没有 `BEGIN`/`COMMIT`。因此它与 `rust-store/host`（用 sqlx 原生事务把「归档+删除」做成原子段）**不同**：go-store v0 不保证「已归档但未删除」等中途失败的回滚。

> 若业务需要「归档+删除」原子，需自行在调用方包一层事务（当前 API 未暴露连接，故这属 v0 已知缺口）；或改用 `rust-store/host`。**待核实**：是否需要、以及如何为 go-store 补齐事务能力（未在本仓库任何文档中给出结论）。

---

## 11. 权限（RBAC 透传）

**判决定律在 core，Go 宿主零判决**：`store.go` 的 RBAC 方法只做「配置注入 + 查询面透传」，是否放行、可读/可写哪些字段、行级条件如何合并，全部由 core 裁决。注入后，`plan` 链路的拦截自动生效。

```go
// 1) 注入策略（null 清除关闭；解析失败显式报错）
_ = store.SetRbac(map[string]any{
    "mode":  "enforce",
    "roles": map[string]any{"editor": map[string]any{}},
    "grants": []any{
        map[string]any{"role": "editor", "model": "user", "actions": []any{"read", "insert"}},
    },
})
enabled, _ := store.RbacEnabled() // true

// 2) 动作判决（策略未注入 / 不介入 → true）
ctxE := &gostore.Context{UserID: "u1", Roles: []string{"editor"}}
ok, _ := store.RbacCan("user", "insert", ctxE) // true
ok, _ = store.RbacCan("user", "remove", ctxE)  // false（未授予）

// 3) 可读/可写字段集（空 ctx → nil 不裁剪）
fields, _ := store.RbacReadableFields("user", ctxE)
writable, _ := store.RbacWritableFields("user", ctxE)

// 4) 行级条件（ownerOnly → {createdBy: userId}）
cond, _ := store.RbacRowCondition("user", "read",
    &gostore.Context{UserID: "u9", Roles: []string{"reader"}}) // {"createdBy":"u9"}

// 5) 拒写清单（命中者写路径拒绝，读不受影响）
_ = store.SetDenyWriteRoles([]string{"guest"})
guest := &gostore.Context{UserID: "g1", Roles: []string{"guest"}}
_, err := store.Update(ctx, "user", map[string]any{"_id": uid}, map[string]any{"age": 1}, guest)
// err != nil（core 稳定前缀原样上浮，例如 ERR_PERMISSION / ERR_NO_WRITE）
```

**错误呈现**：core 采用稳定错误前缀（`ERR_PERMISSION` / `ERR_NO_WRITE` / `ERR_NO_BATCH_WRITE` / `ERR_NO_CONTEXT` 等，见 rust-store README）。go-store 的 `callFFI` 把信封 `error` **原样上浮为 Go `error`**，**不做重映射**；由调用方或适配层决定是否映射为状态码。

---

## 12. 场景事例

> 每个事例固定四段：**业务背景 → Go 调用代码 → 结果 → 优势所在**。

### 事例 1：嵌入式 Go 服务直查 SQLite（单文件部署）

**业务背景**：一个单机 CLI / 边缘采集服务，只需要一个本地文件数据库，不想引入外部 DB 服务，也不想为 ORM 与迁移额外接线。

**Go 调用代码**：

```go
store, err := gostore.Open("sqlite://./data/app.db") // 落盘单文件
if err != nil { log.Fatal(err) }
defer store.Close()

_ = store.Register(userSchema)
_ = store.EnsureTables(userSchema) // 幂等建表

doc, err := store.Insert(ctx, "user", map[string]any{"name": "alice", "age": int64(30)}, nil)
rows, err := store.Query(ctx, "user($condition: @c0) { name, age }",
    map[string]any{"c0": map[string]any{"age": map[string]any{"$gte": 18}}}, nil)
```

**结果**：`doc["_id"]` 形如 `u<base36毫秒><8位随机>`，`doc["createdAt"]` 为毫秒时间戳；`rows` 为命中文档数组。

**优势所在**：
- **零外部服务**：SQLite 驱动是纯 Go（`modernc.org/sqlite`），加一个 cdylib 即可，整个服务可打成单个可执行文件 + 一个 `.db`。
- **schema 即代码**：模型用 JSON map 声明，改字段不动业务结构；建表由 `EnsureTables` 幂等完成。
- **`_id` 与时间戳由宿主供给**：core 无时钟无随机源，插入结果的 `_id`/`createdAt` 在返回文档里直接可见（`Insert` 回读补 `_id`）。

---

### 事例 2：与 rust-store 同一套 GQL / 权限语义（多语言一致）

**业务背景**：同一产品有 Node 后端与 Go 边缘服务，两端要读同一份数据的同一套查询语义，不希望在 Go 侧重新实现一遍 GQL 解析、权限与方言翻译。

**Go 调用代码**：

```go
// 与 nodejs-store / py-store 完全相同的 schema JSON 与 GQL 串
_ = store.Register(userSchema)
rows, err := store.Query(ctx, "user($condition: @c0) { name, age }",
    map[string]any{"c0": map[string]any{"age": map[string]any{"$gte": 18}}}, ctxPerm)
```

**结果**：GQL 解析、权限注入、命令规划、SQL 方言翻译全部在 rust-store/core 完成；Go 侧只是把规划结果翻译并执行。同一串 GQL 在 Node / Python / Go 上得到一致语义。

**优势所在**：
- **语义只实现一次**：宿主之间「不能漂移」不是靠约定，而是因为**没有第二份实现**。
- **同一份 schema 与同一串 GQL 跨语言复用**：迁移语言或做多端时，业务查询语句不必重写。
- **权限行为与 Rust 宿主逐段对齐**：`store.go` 的 query/insert/update/remove 编排注释明确标注「逐段对齐 `rust-store/host/src/lib.rs`」。

---

### 事例 3：计算列在 Go 侧零成本复用（一次 FFI 往返处理一批）

**业务背景**：列表接口需要为每条记录算一个派生字段（如外币换算、展示标签），希望计算逻辑写在 Go 里，但不想为每条记录做一次跨语言调用。

**Go 调用代码**：

```go
// schema 中声明 asyncFn（批量，推荐）
"computes": map[string]any{
    "amountAud": map[string]any{"type": "number", "asyncFn": true, "fnRef": "goAud"},
}

store.RegisterCompute("goAud", func(ctx *gostore.Context, docs []map[string]any) error {
    for _, d := range docs { // 一批文档一次处理
        if amt, ok := d["amount"].(float64); ok {
            d["amountAud"] = amt * 15000
        }
    }
    return nil
})

rows, _ := store.Query(ctx, "order { status, amount, amountAud }", nil, nil)
```

**结果**：`amount=2.5` → `amountAud=37500`。同步 `fn`（`RegisterSyncCompute`）则逐文档返回值，如 `statusUpper="paid!"`。

**优势所在**：
- **批量形态把跨 FFI 成本摊薄为「一次往返一批」**（`AsyncComputeFunc` 接收 `[]map[string]any` 原地改写），适合列表页。
- **计算列声明在 schema、执行体在 Go**：core 负责「该算哪些列、需要哪些依赖字段」，Go 只写纯函数。
- **失败即失败**：回调返回 error 会让整个查询显式失败上报，不会把计算列静默留空。

---

### 事例 4：本地开发 / 边缘节点（内存库，零依赖）

**业务背景**：CI 与本地开发要快速起一套完整的数据读写链路做集成测试，不希望依赖任何外部数据库或容器。

**Go 调用代码**：

```go
store, _ := gostore.Open("sqlite::memory:") // 内存库
defer store.Close()
_ = store.Register(userSchema)
_ = store.EnsureTables(userSchema)
// ……插入与查询，用完即弃
```

**结果**：测试启动即得到一个可用 SQLite；`go-store` 自带测试即用真实 SQLite（内存库）跑全链路（register → DDL → insert → GQL → 计算列 → update → remove）。

**优势所在**：
- **零外部依赖**：不需要 DB 服务、不需要 C 工具链（purego + 纯 Go 驱动）。
- **贴近生产语义**：内存库与落盘库走的是同一套 core 规划与 SQL 执行路径，测试结论可作为行为基准。
- **行为基准对齐**：本仓库测试场景与断言对齐 `rust-store/host/tests/sqlite_e2e.rs`。

---

### 事例 5：与 nodejs-store / py-store 的同 schema 一致性

**业务背景**：一个多语言团队希望「同一份 schema 定义」在 Node、Python、Go 上产生一致的数据形状与结果，减少联调与对账成本。

**Go 调用代码**：

```go
// 同一 JSON 契约：name / collection / idPrefix / fields / computes / relations ...
_ = store.Register(orderSchema)
doc, _ := store.Insert(ctx, "order", map[string]any{"status": "paid", "amount": 2.5}, nil)
out, _ := store.Remove(ctx, "order", map[string]any{"_id": doc["_id"]}, nil)
```

**结果**：
- 插入返回的 `returns` 文档带 `_id`（`idPrefix` 生效）与 `createdAt`（`timestamps` 生效）；
- 删除返回 `{"deletedCount":1, "archivedCount":1}`，源表命中清零，归档表 `<collection>_deleted` 留下记录（与 Node/Python 语义一致）。

**优势所在**：
- **同一 JSON 契约**：`Register` 的 defn 与 `nodejs-store` 的 `store.register(defn)` 同契约，core 自动派生 `<Name>Deleted` 归档表。
- **出参形状对齐**：`QueryWithCount` 的 `{items,total,hasMore,page,pageSize}` 对齐 Node `queryWithCount` / Python `query_with_count`。
- **软删除语义统一**：`Remove` 先归档后删除，归档表结构（含 `deletedAt`）由 `EnsureTables` 统一生成。

---

### 事例 6：v0 边界场景（非 SQLite 后端 / 不可下推组合 → 明确报错）

**业务背景**：团队想直接切到 MySQL 或 Postgres，或写了一条 SQL 侧无法安全翻译的查询，需要清楚「会得到什么」。

**Go 调用代码**：

```go
// (a) 非 SQLite 后端：v0 不支持
store, err := gostore.Open("mysql://user:pw@host/db")
// err != nil；且 store 为 nil —— 加载/连接阶段即失败，不会得到半可用句柄

// (b) 查询未注册 schema
_, err = store.Query(ctx, "ghost { name }", nil, nil) // err != nil

// (c) 不可安全下推的组合
_, err = store.Query(ctx, /* 某含 unsupported 组合的 GQL */ "…", params, nil)
// err: 命令包含无法安全下推的组合（unsupported = [...]）：拒绝执行，请改写查询或换用 MongoDB 源
```

**结果**：全部以**显式 error** 返回，不产生部分结果、不静默降级。

**优势所在**：
- **诚实边界**：v0 只做 SQLite，遇到其它后端就是失败，而不是「看起来能跑、实际丢条件」。
- **不静默降级**（铁律）：`translateCommand` 一旦发现 `unsupported` 非空，**绝不执行残缺 SQL**，直接报错并提示改写或换源。
- **未注册 schema、未注册计算列回调** 同样是显式错误，便于尽早暴露接线遗漏。

---

## 13. 边界与非目标

**v0 边界（如实声明，均以源码为准）：**

1. **后端**：仅 SQLite。MySQL / PostgreSQL / MongoDB / local 属后续阶段。
2. **平台**：当前仅 **Windows** 可编译运行（只有 `load_windows.go`，无 unix 加载器；`util.go` 依赖 kernel32）。
3. **事务**：无事务 API；`Remove` 的归档+删除未包在显式事务里（见 §10）。
4. **DDL**：`EnsureTables` 覆盖标量列 + timestamps + `__present` + 归档表；`object`/`array` 以 `TEXT`（JSON 文本列）建列，读侧由 core 还原。**索引 DDL、autoincrement 主键、MySQL/PG 方言未含**。
5. **触发器 / 联邦 / mutation 步骤编排**：未纳入 v0（宿主层无相关编排代码）。
6. **顶层 API 面有限**：无 `Exists` / `Count` / `InsertMany` / `UpdateMany` / `Ask` 等（Go 侧未导出）。
7. **SQLite 并发**：`Open` 强制 `MaxOpenConns(1)`（SQLite 单写者模型；modernc 驱动并发写会 `SQLITE_BUSY`，单连接 + 排队最稳）。

**非目标：**

- 不是 ORM：不做结构体映射、不做编译期类型生成。
- 不是迁移引擎：不做 DDL diff / migration。
- 不重映射错误码：core 稳定前缀原样上浮，映射交给调用方。

---

## 14. FAQ

**Q1. 需要 cgo 吗？**
不需要。绑定层用 `purego` **运行时加载** cdylib（`ffi.go`），因此**不需要任何 C 工具链**，也无需 `CGO_ENABLED=1`。这与「传统 cgo 绑定」不同。

**Q2. 需要先构建 rust-store 的 C ABI 产物吗？**
需要。go-store 运行时要加载 `rust_store_ffi.dll`（crate `rust-store-ffi`）。构建命令：`cargo build -p rust-store-ffi --release`（在 `rust-store/`）。也可用环境变量 `RUST_STORE_FFI` 或在首次使用前赋值 `gostore.LibraryPath` 指定路径（见 §5 探测顺序）。

**Q3. 支持哪些后端？**
v0 **仅 SQLite**。`Open` 只接受 SQLite DSN（`sqlite://path`、`sqlite::memory:`，或驱动原生 `path.db` / `:memory:` / `file:...`）。传入 MySQL/PG 等会失败。

**Q4. 能跨平台吗？**
当前**只能 Windows**——源码只有 `load_windows.go`（`windows.LoadLibrary` + `GetProcAddress`），且 `util.go` 依赖 `kernel32.dll`。Linux/macOS 需补一个对应加载器实现，属后续阶段。

**Q5. 它和 GORM / database/sql 是什么关系？**
go-store **内部就是基于 `database/sql`**（配合纯 Go 的 `modernc.org/sqlite` 驱动）执行 core 规划出的参数化 SQL；但它**对外不是 ORM**：没有结构体映射，schema 是运行时 JSON map，返回的是 `map[string]any` 文档。它更接近「schema 驱动的查询/写入门面」，而非「模型→表的映射框架」。

**Q6. 性能如何？能不能扛高并发？**
源码层面可确认的是：SQLite 走**单连接串行**（`MaxOpenConns(1)`），且 `asyncFn` 计算列采用**批量一次往返**以摊薄跨 FFI 成本。**未提供任何 benchmark / 压测数据**——具体吞吐请以你自己场景的实测为准（**待核实**）。

**Q7. 有事务吗？怎么写原子操作？**
v0 **没有事务 API**，且 `Remove` 的归档+删除未包在显式事务中（见 §10）。需要「多步原子」时，当前 API 无法直接满足——这是 v0 已知缺口。**待核实**：后续是否补齐事务能力。

**Q8. 某个 schema 有哪些字段？**
用 `SchemaFieldsKeys(schema)` 取该 schema 的 fields 键列表（**字母序**；未注册返回 `nil`）。它用于适配层拼 GQL 投影段。

**Q9. 计算列的 fnRef 忘了注册会怎样？**
不会静默置空。查询时会显式报错：asyncFn 报 `计算列 <fnRef> 未注册异步实现（RegisterCompute）`，同步 fn 报 `... 未注册同步实现（RegisterSyncCompute）`。

**Q10. 权限被拒时返回什么？**
返回非 `nil` 的 `error`，内容是 core 的稳定前缀原文（如 `ERR_PERMISSION` / `ERR_NO_WRITE`），go-store **不重映射**。可用 `strings.HasPrefix(err.Error(), "ERR_PERMISSION")` 之类判断。

**Q11. `Query` 的第四个参数是什么？**
是权限上下文 `*gostore.Context`（对应 core `permission::Context`）；传 `nil` 表示无上下文（不裁剪）。它**不是** `context.Context`——后者是第一个参数，用于取消/超时。

**Q12. 查询未注册的 schema 会怎样？**
显式报错（不返回空数组）。见测试 `TestUnsupportedExplicitError`。

---

## 15. Development

```bash
# 构建
go build ./...

# 运行测试（真实 SQLite 内存库全链路；需先构建 core-ffi 产物，见 §5）
go test ./...
go test -run TestInsertQueryUpdateRemove -v
```

测试文件 `store_test.go` 覆盖：`Insert/Query/QueryOne/Update/Remove` 全链路、计算列（sync `fn` + 批量 `asyncFn`）、`QueryWithCount` 分页、`unsupported` 显式报错、RBAC（注入/判决/可读字段/行级条件/拒写）。测试与断言对齐 `rust-store/host/tests/sqlite_e2e.rs`（家族行为基准）。

> **前置**：测试会触发 `Open` → 加载 cdylib，因此运行前必须先构建 `rust_store_ffi.dll`，或设置 `RUST_STORE_FFI` / `gostore.LibraryPath`。

---

## 16. Related projects

- `../rust-store`：Rust 核心与各语言绑定。其中：
  - `rust-store/core`：语言无关核心（GQL / 权限 / 计算列 / 命令规划 / SQL 方言，**无 IO**）——**GQL 语义、权限模型、计算列、命令规划以它为准**；
  - `rust-store/core-ffi`（crate `rust-store-ffi`，`cdylib`）：go-store 依赖的 **C ABI 面**（`rcore_*` 符号 + JSON 信封）；
  - `rust-store/host`（crate `rust-store`）：纯 Rust 宿主，go-store 的**行为基准**。
- `../nodejs-store` / `../py-store`：Node / Python 宿主（同一 schema 与 GQL 语义）。
- `../store-api`：REST 皮（GQL → HTTP，按 `store-api/spec` 映射）。

> **GQL 语法、权限模型、计算列（含 `agg`）、聚合语义**：go-store 无自己的 spec 目录，一律以 `../rust-store/README.md` 及其 core 文档为准。

---

## 17. License

MIT License

Copyright (c) 2026 丁滔

Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the "Software"), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
