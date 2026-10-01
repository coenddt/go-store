# go-store

**rust-store-core 的 Go 宿主** —— nodejs-store / py-store / rust-store(host) 的 Go 孪生。统一数据层，用纯 JSON 定义模型、MongoDB 风格的 GQL 树形语法查询，内置 RBAC、计算列与软删除归档；SQLite 落库（v0）。

## 架构

```
go-store（本仓库）          rust-store/core-ffi           rust-store/core
Go 宿主（IO + 编排）  ──▶  C ABI 绑定（JSON 边界）  ──▶  语言无关核心
purego 免 cgo 加载        FnRegistry 回调桥              GQL/权限/计算列/方言
```

语义只有一份（Rust core），宿主只做三件事：驱动 IO、供给 now/new_id、命令序列编排 —— 与 `rust-store/host` 逐段对齐，Python/Node/Go 行为不漂移。

## 依赖与安装

- Go 1.25+
- rust-store 的 C ABI 产物 `rust_store_ffi.dll`（构建：`cargo build -p rust-store-ffi`，位于 `rust-store/target/debug|release/`）
- 无 cgo：绑定层用 [purego](https://github.com/ebitengine/purego) 运行时加载，**不需要任何 C 工具链**
- SQLite 驱动为纯 Go 的 `modernc.org/sqlite`，同样无 C 依赖

库路径探测顺序：环境变量 `RUST_STORE_FFI` → `../rust-store/target/release` → `../rust-store/target/debug` → 当前目录。也可直接赋值 `gostore.LibraryPath`。

## 快速上手

```go
package main

import (
	"context"
	"fmt"

	gostore "github.com/coenddt/go-store"
)

func main() {
	store, _ := gostore.Open("sqlite://app.db")
	defer store.Close()

	userSchema := map[string]any{
		"name": "user", "collection": "user", "idPrefix": "u",
		"fields": map[string]any{
			"name": map[string]any{"type": "string"},
			"age":  map[string]any{"type": "number"},
		},
	}
	_ = store.Register(userSchema)
	_ = store.EnsureTables(userSchema)

	ctx := context.Background()
	doc, _ := store.Insert(ctx, "user", map[string]any{"name": "alice", "age": 30}, nil)

	rows, _ := store.Query(ctx, "user($condition: @c0) { name, age }",
		map[string]any{"c0": map[string]any{"age": map[string]any{"$gte": 18}}}, nil)
	fmt.Println(doc["_id"], rows)
}
```

## API 面（v0）

| 方法 | 说明 |
|---|---|
| `Open(dsn)` | 连接 SQLite（`sqlite://path` / `sqlite::memory:`） |
| `Register(defn)` | 注册 schema（与 nodejs-store 同一 JSON 契约，自动派生 `<Name>Deleted` 归档表） |
| `EnsureTables(defn)` | 简版建表（主表 + 归档表 + `__present` 哨兵列） |
| `Query(ctx, gql, params, ctxOpt)` | GQL 查询（含关系展开 / 两阶段 / 聚合） |
| `QueryOne(...)` | 单条（core 自动注入 `$limit(1)`） |
| `Insert(ctx, schema, data, ctxOpt)` | 插入（autoincrement 主键回读补 `_id`） |
| `Update(ctx, schema, cond, data, ctxOpt)` | 部分更新（探针重入编排） |
| `Remove(ctx, schema, cond, ctxOpt)` | 删除 + 归档编排（返回 `{deletedCount, archivedCount}`） |
| `RegisterCompute(fnRef, fn)` | **批量 asyncFn 计算列**（Go 闭包，一次 FFI 往返处理一批，推荐） |
| `RegisterSyncCompute(fnRef, fn)` | 逐文档同步 fn 计算列（Go 闭包，跨 FFI 回调桥） |

### Go 定义计算列

```go
// schema 声明：{"computes": {"amountAud": {"type":"number","asyncFn":true,"fnRef":"goAud"}}}
store.RegisterCompute("goAud", func(ctx *gostore.Context, docs []map[string]any) error {
	for _, d := range docs {
		if amt, ok := d["amount"].(float64); ok {
			d["amountAud"] = amt * 15000
		}
	}
	return nil
})
```

回调失败即整个查询失败（错误带上 fnRef 语义上浮），绝不静默置空。

## v0 范围诚实声明

- **后端**：仅 SQLite（与 `rust-store/host` v0 同策略）；MySQL / PostgreSQL / MongoDB 属后续阶段。
- **DDL**：`EnsureTables` 覆盖标量列 + timestamps + `__present` + 归档表；索引 DDL、autoincrement、object/array JSON 列、MySQL/PG 方言未含。
- **权限错误**：沿 core 稳定前缀（`ERR_PERMISSION:` 等）原样上浮，由调用方或适配器映射状态码。
- 行为基准对齐 `rust-store/host/tests/sqlite_e2e.rs`；本仓库测试为真实 SQLite（内存库）全链路。

## 相关仓库

- `../rust-store`：核心与各语言绑定（core / core-ffi / core-node / core-py / host）
- `../store-api-go`：REST 壳（GQL → HTTP，按 `../store-api/spec` 映射）
- `../nodejs-store` / `../py-store`：Node / Python 宿主
