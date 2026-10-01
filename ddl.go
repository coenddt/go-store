// ddl.go —— 简版 SQLite 建表器（schema def → CREATE TABLE 文本；纯函数，不连库、不回写）。
//
// 对齐 nodejs-store/src/ddl.js 的 SQLite 分支语义（类型映射逐项一致）：
//   - 标量字段按声明类型建列；object/array → TEXT（JSON 文本列，读侧由 core row 还原）；
//   - 每表必建 __present 哨兵列（存「显式存在字段集合」，core dialect 物理契约）；
//   - timestamps !== false → 追加 createdAt / updatedAt（INTEGER 毫秒）；
//   - 归档表 <collection>_deleted 同结构 + deletedAt。
//
// v0 范围诚实声明：覆盖标量列与时间戳；schema.indexes 索引 DDL、autoincrement 主键、
// MySQL/PG 方言属后续阶段（与 rust-store/host v0 同策略，建表可由调用方脚本替代）。
package gostore

import (
	"fmt"
	"sort"
	"strings"
)

// sqliteTypes schema 声明类型 → SQLite 列类型（对齐 ddl.js TYPES 的 sqlite 分支）
var sqliteTypes = map[string]string{
	"string":   "TEXT",
	"int":      "INTEGER",
	"long":     "INTEGER",
	"number":   "INTEGER",
	"float":    "REAL",
	"double":   "REAL",
	"bool":     "INTEGER",
	"boolean":  "INTEGER",
	"datetime": "INTEGER",
	"date":     "INTEGER",
	"object":   "TEXT",
	"array":    "TEXT",
}

func quoteIdentSQLite(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// EnsureTables 为 schema 建主表与归档表（IF NOT EXISTS，幂等）。
func (s *Store) EnsureTables(defn map[string]any) error {
	stmts, err := CreateTableStatements(defn)
	if err != nil {
		return err
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("DDL 执行失败: %w\n语句: %s", err, stmt)
		}
	}
	return nil
}

// CreateTableStatements 生成主表 + 归档表的 CREATE TABLE 语句。
func CreateTableStatements(defn map[string]any) ([]string, error) {
	name, _ := defn["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("schema 缺少 name")
	}
	collection, _ := defn["collection"].(string)
	if collection == "" {
		collection = name
	}
	timestamps := true
	if v, ok := defn["timestamps"].(bool); ok {
		timestamps = v
	}

	fields, _ := defn["fields"].(map[string]any)
	if fields == nil {
		fields = map[string]any{}
	}

	// 列定义（按字段名排序保证输出确定）
	names := make([]string, 0, len(fields))
	for f := range fields {
		names = append(names, f)
	}
	sort.Strings(names)

	cols := []string{`"_id" TEXT PRIMARY KEY`}
	for _, f := range names {
		if f == "_id" {
			continue // 主键已内置（autoincrement 属后续阶段，见文件头声明）
		}
		spec, _ := fields[f].(map[string]any)
		typ := "TEXT"
		if spec != nil {
			if t, ok := spec["type"].(string); ok {
				if ct, ok := sqliteTypes[t]; ok {
					typ = ct
				} else {
					return nil, fmt.Errorf("字段 %s 类型 %s 不在 SQLite v0 类型表内", f, t)
				}
			}
		}
		cols = append(cols, quoteIdentSQLite(f)+" "+typ)
	}
	if timestamps {
		cols = append(cols, `"createdAt" INTEGER`, `"updatedAt" INTEGER`)
	}
	cols = append(cols, `"__present" TEXT`)

	colsDeleted := append([]string{}, cols...)
	if timestamps {
		colsDeleted = append(colsDeleted, `"deletedAt" INTEGER`)
	} else {
		colsDeleted = append(colsDeleted, `"deletedAt" INTEGER`)
	}

	main := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)",
		quoteIdentSQLite(collection), strings.Join(cols, ", "))
	deleted := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)",
		quoteIdentSQLite(collection+"_deleted"), strings.Join(colsDeleted, ", "))
	return []string{main, deleted}, nil
}
