// store_test.go —— 端到端集成测试：真实 SQLite（内存库）全链路。
// 场景与断言对齐 rust-store/host/tests/sqlite_e2e.rs（家族行为基准）：
// register → DDL → insert → GQL 查询 → 计算列（Go 回调桥）→ update（探针重入）→ remove（归档）。
package gostore

import (
	"context"
	"testing"
)

func freshStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("sqlite::memory:")
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	userSchema := map[string]any{
		"name":       "user",
		"collection": "user",
		"idPrefix":   "u",
		"fields": map[string]any{
			"name": map[string]any{"type": "string"},
			"age":  map[string]any{"type": "number"},
		},
	}
	if err := s.Register(userSchema); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := s.EnsureTables(userSchema); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return s
}

func TestInsertQueryUpdateRemove(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()

	// insert
	doc, err := s.Insert(ctx, "user", map[string]any{"name": "alice", "age": int64(30)}, nil)
	if err != nil {
		t.Fatalf("insert 失败: %v", err)
	}
	if doc["name"] != "alice" {
		t.Fatalf("insert 返回 name = %v", doc["name"])
	}
	uid, _ := doc["_id"].(string)
	if len(uid) == 0 || uid[0] != 'u' {
		t.Fatalf("idPrefix 应生效, got %q", uid)
	}
	if _, ok := doc["createdAt"].(float64); !ok {
		t.Fatalf("timestamps 应生效, got %T", doc["createdAt"])
	}

	// query（GQL 条件 + 命名参数）
	rows, err := s.Query(ctx, "user($condition: @c0) { name, age }",
		map[string]any{"c0": map[string]any{"age": map[string]any{"$gte": 18}}}, nil)
	if err != nil {
		t.Fatalf("query 失败: %v", err)
	}
	if len(rows) != 1 || rows[0]["name"] != "alice" {
		t.Fatalf("query 结果 = %v", rows)
	}

	// query_one：命中
	one, err := s.QueryOne(ctx, "user($condition: @c0)",
		map[string]any{"c0": map[string]any{"_id": uid}}, nil)
	if err != nil || one == nil {
		t.Fatalf("query_one 应命中: %v %v", one, err)
	}

	// query_one：未命中
	none, err := s.QueryOne(ctx, "user($condition: @c0)",
		map[string]any{"c0": map[string]any{"_id": "nope"}}, nil)
	if err != nil || none != nil {
		t.Fatalf("query_one 未命中应返回 nil: %v %v", none, err)
	}

	// update（探针重入路径）
	updated, err := s.Update(ctx, "user", map[string]any{"_id": uid},
		map[string]any{"age": int64(31)}, nil)
	if err != nil {
		t.Fatalf("update 失败: %v", err)
	}
	if updated == nil || updated["age"] != float64(31) {
		t.Fatalf("update 结果 = %v", updated)
	}

	// 权限：guest 写入拒绝（core ERR_NO_WRITE，原样透传）
	guest := &Context{UserID: "g1", Roles: []string{"guest"}}
	_, denied := s.Update(ctx, "user", map[string]any{"_id": uid}, map[string]any{"age": 1}, guest)
	if denied == nil {
		t.Fatalf("guest 写入应被拒绝")
	}

	// remove：归档 + 删除
	out, err := s.Remove(ctx, "user", map[string]any{"_id": uid}, nil)
	if err != nil {
		t.Fatalf("remove 失败: %v", err)
	}
	if out["deletedCount"] != int64(1) || out["archivedCount"] != int64(1) {
		t.Fatalf("remove 计数 = %v", out)
	}

	// 归档后源表为空
	rows, err = s.Query(ctx, "user { name }", nil, nil)
	if err != nil || len(rows) != 0 {
		t.Fatalf("删除后应无数据: %v %v", rows, err)
	}
}

func TestComputedColumns(t *testing.T) {
	s, err := Open("sqlite::memory:")
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer s.Close()

	itemSchema := map[string]any{
		"name":       "item",
		"collection": "item",
		"idPrefix":   "i",
		"fields": map[string]any{
			"title":  map[string]any{"type": "string"},
			"amount": map[string]any{"type": "number"},
		},
		"computes": map[string]any{
			// 关系滚动聚合（纯声明式，无回调）
			"itemCount": map[string]any{"type": "int"},
			// 同步 fn：走回调桥 kind=0
			"titleUpper": map[string]any{"type": "string", "fn": true, "fnRef": "goUpper"},
		},
	}
	_ = itemSchema
	// v0 简版：同步 fn 计算列直接依赖回调桥；关系 agg 需要关系 schema，分开放到 TestRelationCompute。

	schema := map[string]any{
		"name":       "order",
		"collection": "orders",
		"idPrefix":   "o",
		"fields": map[string]any{
			"status": map[string]any{"type": "string"},
			"amount": map[string]any{"type": "number"},
		},
		"computes": map[string]any{
			// 同步 fn 计算列（Go 闭包，跨 FFI 回调桥）
			"statusUpper": map[string]any{"type": "string", "fn": true, "fnRef": "goUpper"},
			// asyncFn 批量计算列（Go 闭包，读时批量改写）
			"amountAud": map[string]any{"type": "number", "asyncFn": true, "fnRef": "goAud"},
		},
	}
	if err := s.Register(schema); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := s.EnsureTables(schema); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	s.RegisterSyncCompute("goUpper", func(ctx *Context, doc map[string]any) (any, error) {
		v, _ := doc["status"].(string)
		return v + "!", nil
	})
	s.RegisterCompute("goAud", func(ctx *Context, docs []map[string]any) error {
		for _, d := range docs {
			if amt, ok := d["amount"].(float64); ok {
				d["amountAud"] = amt * 15000
			}
		}
		return nil
	})

	ctx := context.Background()
	if _, err := s.Insert(ctx, "order", map[string]any{"status": "paid", "amount": 2.5}, nil); err != nil {
		t.Fatalf("insert 失败: %v", err)
	}

	rows, err := s.Query(ctx, "order { status, statusUpper, amount, amountAud }", nil, nil)
	if err != nil {
		t.Fatalf("query 失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应有 1 行, got %d", len(rows))
	}
	r := rows[0]
	if r["statusUpper"] != "paid!" {
		t.Fatalf("同步 fn 计算列 = %v", r["statusUpper"])
	}
	if r["amountAud"] != 37500.0 {
		t.Fatalf("asyncFn 计算列 = %v (%T)", r["amountAud"], r["amountAud"])
	}
}

// TestUnsupportedExplicitError —— unsupported 非空时显式报错（no-error-masking 铁律）。
func TestUnsupportedExplicitError(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	// 触发探针重入以外的合法路径失败：向未注册 schema 查询
	if _, err := s.Query(ctx, "ghost { name }", nil, nil); err == nil {
		t.Fatalf("查询未注册 schema 应报错")
	}
}

// TestQueryWithCount —— 分页计数全链路（对齐 node queryWithCount 出参契约）。
func TestQueryWithCount(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		age := int64(20 + i)
		if _, err := s.Insert(ctx, "user", map[string]any{"name": "n", "age": age}, nil); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	out, err := s.QueryWithCount(ctx, "user", map[string]any{"page": 1, "pageSize": 2}, nil)
	if err != nil {
		t.Fatalf("QueryWithCount: %v", err)
	}
	if out["total"] != int64(5) || out["page"] != int64(1) || out["pageSize"] != int64(2) {
		t.Fatalf("分页元数据 = %v", out)
	}
	if out["hasMore"] != true {
		t.Fatalf("page=1&pageSize=2&total=5 应 hasMore=true: %v", out)
	}
	items, _ := out["items"].([]map[string]any)
	if len(items) != 2 {
		t.Fatalf("items 应 2 行: %d", len(items))
	}

	// 末页 hasMore=false
	out, err = s.QueryWithCount(ctx, "user", map[string]any{"page": 2, "pageSize": 2}, nil)
	if err != nil {
		t.Fatalf("QueryWithCount: %v", err)
	}
	if out["hasMore"] != false {
		t.Fatalf("末页应 hasMore=false: %v", out)
	}
}
