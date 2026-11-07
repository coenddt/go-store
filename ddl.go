// ddl.go —— 简版 SQLite 建表器（schema def → CREATE TABLE 文本；不连库、不回写）。
//
// 对齐 nodejs-store/src/ddl.js 与 py-store/src/py_store/ddl.py 的 SQLite 分支语义：
//   - 物理列名 = core::naming 的 snake_case（经 FFI，宿主不自研转换）；
//     执行期 SQL 由 core dialect 生成，两侧必须同源，否则 `no column named created_at`；
//   - 标量字段按声明类型建列；object/array → TEXT（JSON 文本列，读侧由 core row 还原）；
//   - 每表必建 __present 哨兵列（存「显式存在字段集合」，core dialect 物理契约）；
//   - timestamps !== false → 追加 created_at / updated_at（INTEGER 毫秒）；
//   - creator 权限（read/write 含 creator）→ 补归属键列（ownerKey，缺省 createdBy，
//     与 core registry::add_owner_field 同规则）；
//   - 归档表 <collection>_deleted 同结构 + deleted_at。
//
// v0 范围诚实声明：覆盖标量列、时间戳与归属键；schema.indexes 索引 DDL、autoincrement 主键、
// MySQL/PG 方言属后续阶段（与 rust-store/host v0 同策略，建表可由调用方脚本替代）。
package gostore

import (
	"fmt"
	"sort"
	"strings"
)

// 内建列逻辑名（与 core schema/registry.rs 同源）
const (
	defaultOwnerKey = "createdBy"
	deletedAtKey    = "deletedAt"
)

// pname 逻辑名 → SQLite 物理标识符。家族唯一算法在 core::naming（经 FFI），
// 宿主不得自研 snake_case 转换；`_id` 与 `__` 前缀保留原名（core dialect::physical_of）。
func pname(logical string) (string, error) {
	if logical == "" || logical == "_id" || strings.HasPrefix(logical, "__") {
		return logical, nil
	}
	if err := load(); err != nil {
		return "", err
	}
	lp, err := rawCFor(logical)
	if err != nil {
		return "", err
	}
	defer freeCArg(lp)
	bp, err := rawCFor("sqlite")
	if err != nil {
		return "", err
	}
	defer freeCArg(bp)
	var out string
	if err := callFFIInto(rcoreTranslateName(lp, bp), &out); err != nil {
		return "", fmt.Errorf("逻辑名 %q 的物理名翻译失败: %w", logical, err)
	}
	return out, nil
}

// hasCreatorRole defn.read / defn.write 是否含 creator（accept []any / []string）
func hasCreatorRole(v any) bool {
	switch list := v.(type) {
	case []any:
		for _, r := range list {
			if s, ok := r.(string); ok && s == "creator" {
				return true
			}
		}
	case []string:
		for _, r := range list {
			if r == "creator" {
				return true
			}
		}
	}
	return false
}

// ownerColumn creator 权限隐含的归属键；返回 (逻辑名, 是否需要补列)。
func ownerColumn(defn map[string]any) (string, bool) {
	if !hasCreatorRole(defn["read"]) && !hasCreatorRole(defn["write"]) {
		return "", false
	}
	if k, ok := defn["ownerKey"].(string); ok && k != "" {
		return k, true
	}
	return defaultOwnerKey, true
}

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
	"vector":   "TEXT",
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
	declared := map[string]bool{"_id": true}
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
		col, err := pname(f)
		if err != nil {
			return nil, err
		}
		declared[col] = true
		cols = append(cols, quoteIdentSQLite(col)+" "+typ)
	}
	// 内建列：timestamps → 归属键（与 py implicit_columns 同序；已声明则沿用声明列，不重复）
	if timestamps {
		for _, logical := range []string{"createdAt", "updatedAt"} {
			col, err := pname(logical)
			if err != nil {
				return nil, err
			}
			if !declared[col] {
				declared[col] = true
				cols = append(cols, quoteIdentSQLite(col)+" INTEGER")
			}
		}
	}
	if owner, ok := ownerColumn(defn); ok {
		col, err := pname(owner)
		if err != nil {
			return nil, err
		}
		if !declared[col] {
			declared[col] = true
			cols = append(cols, quoteIdentSQLite(col)+" TEXT")
		}
	}
	cols = append(cols, `"__present" TEXT`)

	deletedCol, err := pname(deletedAtKey)
	if err != nil {
		return nil, err
	}
	colsDeleted := append([]string{}, cols...)
	colsDeleted = append(colsDeleted, quoteIdentSQLite(deletedCol)+" INTEGER")

	mainTable, err := pname(collection)
	if err != nil {
		return nil, err
	}
	deletedTable, err := pname(collection + "_deleted")
	if err != nil {
		return nil, err
	}
	main := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)",
		quoteIdentSQLite(mainTable), strings.Join(cols, ", "))
	deleted := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)",
		quoteIdentSQLite(deletedTable), strings.Join(colsDeleted, ", "))
	return []string{main, deleted}, nil
}
