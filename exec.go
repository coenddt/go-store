// exec.go —— 命令执行层：Mongo 风格命令 JSON → core translate → SQLite 执行 → 行还原。
//
// 铁律（对齐 rust-store/host/src/exec.rs 与 no-error-masking）：
// translate 结果的 `unsupported` 非空时绝不执行残缺 SQL —— 显式报错，禁静默降级。
package gostore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动（注册 "sqlite"）
)

// translatedStmt 对应 core translate 输出的每条语句（dialect/ir.rs Stmt JSON 契约）。
type translatedStmt struct {
	Text      string          `json:"text"`
	Params    []any           `json:"params"`
	IsWrite   bool            `json:"isWrite"`
	RowShape  json.RawMessage `json:"rowShape"`
	Returning []string        `json:"returning"`
}

type translated struct {
	Stmts []translatedStmt `json:"stmts"`
}

// translateCommand 翻译一条 Mongo 风格命令；unsupported 非空 → 显式报错。
// 出参用 UseNumber 解析：params 保持 json.Number（int64 无精度损失，bindParams 负责转换）。
func (s *Store) translateCommand(cmd json.RawMessage) (*translated, error) {
	backendP, err := rawCFor(s.backend) // 裸字符串通道（Backend::parse 原文解析）
	if err != nil {
		return nil, err
	}
	defer freeCArg(backendP)
	ret := rcoreTranslate(s.handle, backendP, mustCArg(json.RawMessage(cmd)))
	data, err := callFFI(ret)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var out struct {
		translated
		Unsupported []any `json:"unsupported"`
	}
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("translate 出参解析失败: %w", err)
	}
	if len(out.Unsupported) > 0 {
		b, _ := json.Marshal(out.Unsupported)
		return nil, fmt.Errorf(
			"命令包含无法安全下推的组合（unsupported = %s）：拒绝执行，请改写查询或换用 MongoDB 源", b)
	}
	return &out.translated, nil
}

// execOutcome：docs = 还原后的文档数组；changes = 写语句累计影响行数
type execOutcome struct {
	Docs    []map[string]any
	Changes int64
}

// execTranslated 在给定连接上执行一组翻译后的语句。
// 取数规则（对齐 host/exec.rs）：最后一条产出行的语句为主结果
//（SELECT 带 rowShape 还原；写语句带 RETURNING 直取列）。
func (s *Store) execTranslated(ctx context.Context, conn *sql.Conn, t *translated) (*execOutcome, error) {
	var docs []map[string]any
	var changes int64

	for i := range t.Stmts {
		stmt := &t.Stmts[i]
		args, err := bindParams(stmt.Params)
		if err != nil {
			return nil, err
		}
		// rowShape 的 JSON null（4 字节）与缺省都视为「无行形态」（对齐 host 的 row_shape.is_null()）
		hasRowShape := len(stmt.RowShape) > 0 && string(stmt.RowShape) != "null"
		if stmt.IsWrite && !hasRowShape && len(stmt.Returning) == 0 {
			res, err := conn.ExecContext(ctx, stmt.Text, args...)
			if err != nil {
				return nil, fmt.Errorf("SQL 执行失败: %w", err)
			}
			n, _ := res.RowsAffected()
			changes += n
			docs = nil
		} else {
			rows, err := conn.QueryContext(ctx, stmt.Text, args...)
			if err != nil {
				return nil, fmt.Errorf("SQL 执行失败: %w", err)
			}
			values, err := rowsToValues(rows)
			rows.Close()
			if err != nil {
				return nil, err
			}
			if hasRowShape {
				restored, err := s.restoreRows(stmt.RowShape, values)
				if err != nil {
					return nil, err
				}
				docs = restored
			} else {
				docs = values
			}
			changes += int64(len(docs))
		}
	}
	return &execOutcome{Docs: docs, Changes: changes}, nil
}

// restoreRows 调 core 的 restore_rows_json 把扁平行还原为嵌套文档。
func (s *Store) restoreRows(rowShape json.RawMessage, rows []map[string]any) ([]map[string]any, error) {
	ret := rcoreRestoreRowsJSON(mustCArg(json.RawMessage(rowShape)), mustCArg(rows))
	var docs []map[string]any
	if err := callFFIInto(ret, &docs); err != nil {
		return nil, err
	}
	return docs, nil
}

// bindParams 把 core 产出的 JSON 参数（标量契约）转成 database/sql 的 driver 值。
func bindParams(params []any) ([]any, error) {
	out := make([]any, len(params))
	for i, p := range params {
		switch v := p.(type) {
		case nil, bool, int64, float64, string, []byte:
			out[i] = v
		case json.Number:
			if n, err := v.Int64(); err == nil {
				out[i] = n
			} else {
				f, err := v.Float64()
				if err != nil {
					return nil, fmt.Errorf("参数 %d 非法数字: %w", i, err)
				}
				out[i] = f
			}
		case float32:
			out[i] = float64(v)
		case int:
			out[i] = int64(v)
		default:
			// Binder 契约上全是标量；对象/数组属上游失守，显式报错（禁静默序列化掩盖）
			return nil, fmt.Errorf("参数 %d 类型超出标量契约（%T）；属上游失守，拒绝绑定", i, p)
		}
	}
	return out, nil
}

// rowsToValues 把结果行读成 JSON 值数组（列序即 core 行还原契约的输入顺序）。
func rowsToValues(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("读取列元数据失败: %w", err)
	}
	out := []map[string]any{}
	for rows.Next() {
		slots := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range slots {
			ptrs[i] = &slots[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("行读取失败: %w", err)
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			m[c] = normalizeCell(slots[i])
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("行迭代失败: %w", err)
	}
	return out, nil
}

// normalizeCell 把 driver 返回的动态类型归一为 JSON 值。
func normalizeCell(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(t)
	case int64, float64, bool, string:
		return v
	default:
		// modernc sqlite 的其他数值形态（int 等）——显式转换，不可识别则原样
		switch t := v.(type) {
		case int:
			return int64(t)
		case int32:
			return int64(t)
		case float32:
			return float64(t)
		default:
			return v
		}
	}
}

// substituteIDs 递归替换运行期占位符（对齐 host/exec.rs substitute_ids）：
// `{{phase1.ids}}` → 两阶段第一阶段取回的 _id 数组；`__REL_PRED_IDS__` → 关系谓词命中 id 数组。
// （@c0 类命名参数 core 规划期已消费，宿主只处理这两类。）
func substituteIDs(cmd any, placeholder string, ids []map[string]any) any {
	switch v := cmd.(type) {
	case string:
		if v == placeholder {
			return idArray(ids)
		}
		return v
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = substituteIDs(e, placeholder, ids)
		}
		return out
	case map[string]any:
		for k, e := range v {
			v[k] = substituteIDs(e, placeholder, ids)
		}
		return v
	default:
		return cmd
	}
}

func idArray(docs []map[string]any) []any {
	out := make([]any, len(docs))
	for i, d := range docs {
		if id, ok := d["_id"]; ok {
			out[i] = id
		} else {
			out[i] = nil
		}
	}
	return out
}

// 两阶段 / 关系谓词占位符（与 core PHASE1_IDS 及 core mutate/mod.rs 字面量对齐）
const (
	phase1IDsPlaceholder = "{{phase1.ids}}"
	relPredIDsPlaceholder = "__REL_PRED_IDS__"
)
