// store.go —— go-store 宿主主体：连接管理、schema 注册、query/insert/update/remove 编排。
//
// 编排逻辑逐段对齐 rust-store/host/src/lib.rs（家族行为基准）：
//   - query：plan → 执行 commands[0] → TwoPhase 时取 ids 替换占位符执行第二段 →
//     restore_sort_order → finalize 两段式（prepare → Host 执行 asyncFn → strip）
//   - insert：now/new_id 宿主供给 → plan_insert → 执行 → autoincrement 回读补 _id
//   - update：探针重入（NotProbed → needsProbe → Found/NoResult 重入）
//   - remove：探针 → 关系谓词 preCommand 取 ids → 归档 → 删除
package gostore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unsafe"
)

// Store 是 go-store 的宿主句柄。零值不可用，须经 Open 创建。
type Store struct {
	handle  uint64
	db      *sql.DB
	backend string // "sqlite"（v0 范围，与 rust-store/host 一致）

	mu       sync.RWMutex
	prefixes map[string]string   // schema 名 → idPrefix（register 时缓存，供 generate_id）
	fields   map[string][]string // schema 名 → fields 键列表（register 时缓存，供 REST 投影）

	rng      xorshift
	computes *computeTable
}

// Open 连接一个 SQLite 数据库。dsn 支持 rust 风格（`sqlite://path.db`、
// `sqlite::memory:`）与驱动原生形式（`path.db`、`:memory:`、`file:...`）。
func Open(dsn string) (*Store, error) {
	if err := load(); err != nil {
		return nil, err
	}
	native, err := toNativeDSN(dsn)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", native)
	if err != nil {
		return nil, fmt.Errorf("SQLite 连接失败: %w", err)
	}
	// SQLite 单写者模型；modernc 驱动并发写会 SQLITE_BUSY，单连接 + 排队最稳
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("SQLite 连接失败: %w", err)
	}
	s := &Store{
		handle:   rcoreRegistryNew(0),
		db:       db,
		backend:  "sqlite",
		prefixes: map[string]string{},
		fields:   map[string][]string{},
		computes: &computeTable{async: map[string]AsyncComputeFunc{}, syncF: map[string]SyncComputeFunc{}},
	}
	s.rng.s = uint64(nowMS()) ^ 0x9E3779B97F4A7C15
	if s.handle == 0 {
		db.Close()
		return nil, fmt.Errorf("创建 core Registry 失败")
	}
	computeRegistries.Store(s.handle, s.computes)
	return s, nil
}

func toNativeDSN(dsn string) (string, error) {
	switch {
	case dsn == "sqlite::memory:":
		return ":memory:", nil
	case strings.HasPrefix(dsn, "sqlite://"):
		return strings.TrimPrefix(dsn, "sqlite://"), nil
	case dsn == "":
		return "", fmt.Errorf("SQLite 连接串为空")
	default:
		return dsn, nil
	}
}

// Close 释放 core Registry 句柄与连接池。
func (s *Store) Close() error {
	if s.handle != 0 {
		computeRegistries.Delete(s.handle)
		rcoreRegistryDrop(s.handle)
		s.handle = 0
	}
	return s.db.Close()
}

// Register 注册一个 schema 定义（与 nodejs-store `store.register(defn)` 同一 JSON 契约；
// core 侧自动派生 `<Name>Deleted` 归档表）。应先注册全部 schema 再发起查询。
// idPrefix 同步缓存到宿主侧（insert 时 generate_id 用，对齐 host 的 reg.get(...).id_prefix）。
func (s *Store) Register(defn map[string]any) error {
	ret := rcoreRegistryRegister(s.handle, mustCArg(defn))
	if err := callFFIInto(ret, nil); err != nil {
		return err
	}
	name, _ := defn["name"].(string)
	prefix, _ := defn["idPrefix"].(string)
	if name != "" {
		keys := []string{}
		if fm, ok := defn["fields"].(map[string]any); ok {
			for k := range fm {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		s.mu.Lock()
		s.prefixes[name] = prefix
		s.fields[name] = keys
		s.mu.Unlock()
	}
	return nil
}

// SchemaFieldsKeys 返回已注册 schema 的 fields 键列表（字母序；未注册返回 nil）。
// 供适配层拼 GQL 投影段（store-api/spec/01：投影取自 schema 元数据的 fields 键列表）。
func (s *Store) SchemaFieldsKeys(schema string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fields[schema]
}

// ListSchemas 返回已注册 schema 名。
func (s *Store) ListSchemas() ([]string, error) {
	ret := rcoreRegistryList(s.handle)
	var names []string
	err := callFFIInto(ret, &names)
	return names, err
}

// RegisterCompute 注册批量 asyncFn 计算列执行体（推荐形态：一次 FFI 往返处理一批）。
// schema 中以 `{"asyncFn": true, "fnRef": "<name>"}` 声明（fnRef 缺省为计算列 key 名）。
func (s *Store) RegisterCompute(fnRef string, fn AsyncComputeFunc) {
	s.computes.mu.Lock()
	defer s.computes.mu.Unlock()
	s.computes.async[fnRef] = fn
}

// RegisterSyncCompute 注册逐文档同步 fn 计算列执行体。
// schema 中以 `{"fn": true, "fnRef": "<name>"}` 声明。
func (s *Store) RegisterSyncCompute(fnRef string, fn SyncComputeFunc) {
	s.computes.mu.Lock()
	defer s.computes.mu.Unlock()
	s.computes.syncF[fnRef] = fn
}

// queryPlan 对应 core QueryPlan::to_value。
type queryPlan struct {
	Collection  string            `json:"collection"`
	Mode        string            `json:"mode"`
	Commands    []json.RawMessage `json:"commands"`
	Postprocess json.RawMessage   `json:"postprocess"`
	Sort        json.RawMessage   `json:"sort"`
}

// Query 执行 GQL 查询，返回文档列表。
func (s *Store) Query(ctx context.Context, gql string, params map[string]any, actx *Context) ([]map[string]any, error) {
	plan, err := s.plan(rcorePlanQuery, gql, params, actx)
	if err != nil {
		return nil, err
	}
	items, err := s.executePlan(ctx, plan)
	if err != nil {
		return nil, err
	}
	return s.finalize(ctx, plan.Postprocess, items, actx)
}

// QueryOne 执行 GQL 查询取第一条（core 自动注入 $limit(1)）。
func (s *Store) QueryOne(ctx context.Context, gql string, params map[string]any, actx *Context) (map[string]any, error) {
	plan, err := s.plan(rcorePlanQueryOne, gql, params, actx)
	if err != nil {
		return nil, err
	}
	items, err := s.executePlan(ctx, plan)
	if err != nil {
		return nil, err
	}
	items, err = s.finalize(ctx, plan.Postprocess, items, actx)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	return items[0], nil
}

// QueryWithCount 执行 GQL 查询返回 items + total + 分页元数据（对齐 node queryWithCount /
// py query_with_count 的出参契约：{items, total, hasMore, page, pageSize}）。
// 分页参数：params 传 page/pageSize（page 默认 0，pageSize 默认 50，上限 5000），
// 或 GQL 显式 $skip/$limit 反推；total 统计忽略分页但含权限注入的 owner 条件。
func (s *Store) QueryWithCount(ctx context.Context, gql string, params map[string]any, actx *Context) (map[string]any, error) {
	gqlP, err := rawCFor(gql) // gql 是裸字符串通道（Rust cstr 裸读）
	if err != nil {
		return nil, err
	}
	defer freeCArg(gqlP)
	paramsP := mustCArg(orEmptyParams(params))
	defer freeCArg(paramsP)
	ctxP := mustCArg(actx)
	defer freeCArg(ctxP)

	ret := rcorePlanQueryWithCnt(s.handle, gqlP, paramsP, ctxP)
	// CountQueryPlan.to_value 把 query 字段打平 + 追加 countCommand/page/pageSize，
	// 故一次解析同时覆盖 queryPlan 与分页扩展字段
	var plan struct {
		queryPlan
		CountCommand json.RawMessage `json:"countCommand"`
		Page         float64         `json:"page"`
		PageSize     float64         `json:"pageSize"`
	}
	if err := callFFIInto(ret, &plan); err != nil {
		return nil, err
	}

	items, err := s.executePlan(ctx, &plan.queryPlan)
	if err != nil {
		return nil, err
	}
	items, err = s.finalize(ctx, plan.Postprocess, items, actx)
	if err != nil {
		return nil, err
	}

	total, err := s.execCountScalar(ctx, plan.CountCommand)
	if err != nil {
		return nil, err
	}
	hasMore := (plan.Page+1.0)*plan.PageSize < total
	return map[string]any{
		"items":    items,
		"total":    int64(total),
		"hasMore":  hasMore,
		"page":     int64(plan.Page),
		"pageSize": int64(plan.PageSize),
	}, nil
}

// execCountScalar 执行 countDocuments 翻译结果，取第一行第一列标量
//（对齐 node 执行器 shapeResult 的 `case 'countDocuments': _scalar(out.rows)`；
// count 的 RowShape 为 empty，行还原产出空文档，必须取原始标量）。
func (s *Store) execCountScalar(ctx context.Context, cmd json.RawMessage) (float64, error) {
	if len(cmd) == 0 || string(cmd) == "null" {
		return 0, fmt.Errorf("CountQueryPlan 缺少 countCommand")
	}
	tr, err := s.translateCommand(cmd)
	if err != nil {
		return 0, err
	}
	if len(tr.Stmts) != 1 {
		return 0, fmt.Errorf("countDocuments 应翻译为单条语句，实际 %d 条", len(tr.Stmts))
	}
	stmt := &tr.Stmts[0]
	args, err := bindParams(stmt.Params)
	if err != nil {
		return 0, err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	var v any
	if err := conn.QueryRowContext(ctx, stmt.Text, args...).Scan(&v); err != nil {
		return 0, fmt.Errorf("count 执行失败: %w", err)
	}
	n := normalizeCell(v)
	switch t := n.(type) {
	case int64:
		return float64(t), nil
	case float64:
		return t, nil
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, fmt.Errorf("count 结果非数值: %v", t)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("count 结果非数值（%T）", v)
	}
}

func (s *Store) plan(
	f func(uint64, unsafe.Pointer, unsafe.Pointer, unsafe.Pointer) unsafe.Pointer,
	gql string, params map[string]any, actx *Context,
) (*queryPlan, error) {
	gqlP, err := rawCFor(gql) // gql 是裸字符串通道（Rust cstr 裸读）
	if err != nil {
		return nil, err
	}
	defer freeCArg(gqlP)
	paramsP := mustCArg(orEmptyParams(params))
	defer freeCArg(paramsP)
	ctxP := mustCArg(actx) // *Context 为 nil 时序列化为 null，core 视为无上下文
	defer freeCArg(ctxP)
	var plan queryPlan
	if err := callFFIInto(f(s.handle, gqlP, paramsP, ctxP), &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

func orEmptyParams(p map[string]any) map[string]any {
	if p == nil {
		return map[string]any{}
	}
	return p
}

// executePlan 执行命令序列（两阶段编排对齐 host/lib.rs query）。
func (s *Store) executePlan(ctx context.Context, plan *queryPlan) ([]map[string]any, error) {
	if len(plan.Commands) == 0 {
		return nil, fmt.Errorf("QueryPlan 缺少命令序列")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()

	t0, err := s.translateCommand(plan.Commands[0])
	if err != nil {
		return nil, err
	}
	outcome, err := s.execTranslated(ctx, conn, t0)
	if err != nil {
		return nil, err
	}

	if plan.Mode == "two_phase" {
		if len(plan.Commands) < 2 {
			return nil, fmt.Errorf("TwoPhase 计划缺少第二条命令")
		}
		ids := idArray(outcome.Docs)
		var second map[string]any
		if err := json.Unmarshal(plan.Commands[1], &second); err != nil {
			return nil, fmt.Errorf("第二段命令解析失败: %w", err)
		}
		substituted := substituteIDs(second, phase1IDsPlaceholder, outcome.Docs)
		b, err := json.Marshal(substituted)
		if err != nil {
			return nil, err
		}
		t1, err := s.translateCommand(b)
		if err != nil {
			return nil, err
		}
		o1, err := s.execTranslated(ctx, conn, t1)
		if err != nil {
			return nil, err
		}
		return s.restoreSortOrder(o1.Docs, ids, plan.Sort)
	}
	return outcome.Docs, nil
}

// restoreSortOrder 调 core 还原两阶段结果的原始排序。
func (s *Store) restoreSortOrder(items []map[string]any, ids []any, sort json.RawMessage) ([]map[string]any, error) {
	if len(sort) == 0 {
		sort = json.RawMessage("null")
	}
	sortP := mustCArg(json.RawMessage(sort))
	defer freeCArg(sortP)
	ret := rcoreRestoreSortOrder(mustCArg(items), mustCArg(ids), sortP)
	var restored []map[string]any
	if err := callFFIInto(ret, &restored); err != nil {
		return nil, err
	}
	return restored, nil
}

// finalize 读路径后处理两段式（core finalize.rs 钦定的 FFI 分工）：
// ① prepare：同步 fn 内联（走回调桥）+ 默认值 + 权限裁剪，返回待宿主执行的 asyncFn fnRef；
// ② 宿主逐 fnRef 执行 Go 批量回调（原地改写 items）；
// ③ strip：剥离依赖注入字段。
func (s *Store) finalize(ctx context.Context, postprocess json.RawMessage, items []map[string]any, actx *Context) ([]map[string]any, error) {
	if len(postprocess) == 0 || string(postprocess) == "null" {
		return items, nil
	}
	postP := mustCArg(json.RawMessage(postprocess))
	defer freeCArg(postP)
	ctxP := mustCArg(actx)
	defer freeCArg(ctxP)

	// ① prepare
	prepareP := mustCArg(items)
	var prep struct {
		Items       []map[string]any `json:"items"`
		AsyncFnRefs []string         `json:"asyncFnRefs"`
	}
	ret := rcoreFinalizePrepare(s.handle, postP, prepareP, ctxP)
	if err := callFFIInto(ret, &prep); err != nil {
		return nil, err
	}

	// ② 宿主批量执行 asyncFn（fnRef 缺失注册 → 显式报错，经 `{"error":...}` 上浮）
	if len(prep.AsyncFnRefs) > 0 && len(prep.Items) > 0 {
		for _, fnRef := range prep.AsyncFnRefs {
			f, ok := s.computes.getAsync(fnRef)
			if !ok {
				return nil, fmt.Errorf("计算列 %s 未注册异步实现（RegisterCompute）", fnRef)
			}
			if err := f(actx, prep.Items); err != nil {
				return nil, fmt.Errorf("计算列 %s 执行失败: %w", fnRef, err)
			}
		}
	}

	// ③ strip
	itemsP := mustCArg(prep.Items)
	stripRet := rcoreFinalizeStrip(postP, itemsP)
	var stripped []map[string]any
	if err := callFFIInto(stripRet, &stripped); err != nil {
		return nil, err
	}
	return stripped, nil
}

// Insert 插入一条，返回 core 产出的 returns 文档（autoincrement 主键回读后补入）。
func (s *Store) Insert(ctx context.Context, schema string, data map[string]any, actx *Context) (map[string]any, error) {
	now := nowMS()
	newID := s.rng.generateID(s.idPrefix(schema))
	schemaP, err := rawCFor(schema) // 裸字符串通道（Rust cstr 裸读）
	if err != nil {
		return nil, err
	}
	defer freeCArg(schemaP)
	dataP := mustCArg(data)
	defer freeCArg(dataP)
	ctxP := mustCArg(actx)
	defer freeCArg(ctxP)
	idP, err := rawCFor(newID) // 裸字符串通道
	if err != nil {
		return nil, err
	}
	defer freeCArg(idP)

	ret := rcorePlanInsert(s.handle, schemaP, ctxP, dataP, now, idP)
	var plan struct {
		Command json.RawMessage `json:"command"`
		Returns map[string]any  `json:"returns"`
	}
	if err := callFFIInto(ret, &plan); err != nil {
		return nil, err
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	t, err := s.translateCommand(plan.Command)
	if err != nil {
		return nil, err
	}
	outcome, err := s.execTranslated(ctx, conn, t)
	if err != nil {
		return nil, err
	}

	// autoincrement：INSERT ... RETURNING _id → 回读补入 returns（对齐 host insert）
	if len(outcome.Docs) == 1 {
		if newPK, ok := outcome.Docs[0]["_id"]; ok {
			plan.Returns["_id"] = newPK
		}
	}
	return plan.Returns, nil
}

// idPrefix 从已注册 schema 取 idPrefix（缺省 ""，与 core registry 语义一致）。
// v0：宿主在 register 时同步保存（见 Register），此处读缓存。
func (s *Store) idPrefix(schema string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.prefixes[schema]
}

// Update 更新（探针重入编排；条件命中返回更新后文档，未命中返回 nil）。
func (s *Store) Update(ctx context.Context, schema string, condition, data map[string]any, actx *Context) (map[string]any, error) {
	now := nowMS()
	schemaP, err := rawCFor(schema) // 裸字符串通道
	if err != nil {
		return nil, err
	}
	defer freeCArg(schemaP)
	condP := mustCArg(orEmpty(condition))
	defer freeCArg(condP)
	dataP := mustCArg(data)
	defer freeCArg(dataP)
	ctxP := mustCArg(actx)
	defer freeCArg(ctxP)
	notProbedP := mustCArg("not_probed")
	defer freeCArg(notProbedP)

	// ① 第一次规划（NotProbed）
	ret := rcorePlanUpdate(s.handle, schemaP, ctxP, condP, dataP, now, notProbedP)
	var first struct {
		Command    json.RawMessage `json:"command"`
		NeedsProbe json.RawMessage `json:"needsProbe"`
	}
	if err := callFFIInto(ret, &first); err != nil {
		return nil, err
	}

	// ② 探针重入
	var command json.RawMessage
	if len(first.NeedsProbe) > 0 && string(first.NeedsProbe) != "null" {
		found, err := s.runProbe(ctx, first.NeedsProbe)
		if err != nil {
			return nil, err
		}
		probeP := mustCArg(probeArg(found))
		defer freeCArg(probeP)
		ret2 := rcorePlanUpdate(s.handle, schemaP, ctxP, condP, dataP, now, probeP)
		var second struct {
			Command json.RawMessage `json:"command"`
		}
		if err := callFFIInto(ret2, &second); err != nil {
			return nil, err
		}
		command = second.Command
	} else {
		command = first.Command
	}
	if len(command) == 0 || string(command) == "null" {
		return nil, fmt.Errorf("plan_update 未产出命令")
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	t, err := s.translateCommand(command)
	if err != nil {
		return nil, err
	}
	outcome, err := s.execTranslated(ctx, conn, t)
	if err != nil {
		return nil, err
	}
	if len(outcome.Docs) == 0 {
		return nil, nil
	}
	return outcome.Docs[0], nil
}

// Remove 删除（归档编排：探针 → 关系谓词 preCommand → find 源文档 → 归档 → 删除）。
// 返回 {deletedCount, archivedCount}。
func (s *Store) Remove(ctx context.Context, schema string, condition map[string]any, actx *Context) (map[string]any, error) {
	schemaP, err := rawCFor(schema) // 裸字符串通道
	if err != nil {
		return nil, err
	}
	defer freeCArg(schemaP)
	condP := mustCArg(orEmpty(condition))
	defer freeCArg(condP)
	ctxP := mustCArg(actx)
	defer freeCArg(ctxP)

	notProbedP := mustCArg("not_probed")
	defer freeCArg(notProbedP)
	ret := rcorePlanRemove(s.handle, schemaP, ctxP, condP, notProbedP)
	var plan map[string]any
	if err := callFFIInto(ret, &plan); err != nil {
		return nil, err
	}

	// ① 探针重入
	if needs, ok := plan["needsProbe"]; ok && needs != nil {
		found, err := s.runProbeRaw(ctx, needs)
		if err != nil {
			return nil, err
		}
		probeP := mustCArg(probeArg(found))
		defer freeCArg(probeP)
		ret2 := rcorePlanRemove(s.handle, schemaP, ctxP, condP, probeP)
		plan = nil
		if err := callFFIInto(ret2, &plan); err != nil {
			return nil, err
		}
	}

	deleteCommand, ok := plan["deleteCommand"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("plan_remove 缺少 deleteCommand")
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()

	// ② 关系谓词：preCommand（aggregate 取 _id）→ 替换 filter 中的 __REL_PRED_IDS__
	if pre, ok := deleteCommand["preCommand"]; ok && pre != nil {
		preB, err := json.Marshal(pre)
		if err != nil {
			return nil, err
		}
		t, err := s.translateCommand(preB)
		if err != nil {
			return nil, err
		}
		outcome, err := s.execTranslated(ctx, conn, t)
		if err != nil {
			return nil, err
		}
		if filter, ok := deleteCommand["filter"]; ok {
			deleteCommand["filter"] = substituteIDs(filter, relPredIDsPlaceholder, outcome.Docs)
		}
	}

	var archivedCount int64
	// ③ 归档：findCommand 取源文档 → plan_archive_docs → 执行
	if find, ok := plan["findCommand"]; ok && find != nil {
		findB, err := json.Marshal(find)
		if err != nil {
			return nil, err
		}
		t, err := s.translateCommand(findB)
		if err != nil {
			return nil, err
		}
		outcome, err := s.execTranslated(ctx, conn, t)
		if err != nil {
			return nil, err
		}
		if len(outcome.Docs) > 0 {
			archP := mustCArg(outcome.Docs)
			defer freeCArg(archP)
			ret := rcorePlanArchiveDocs(s.handle, schemaP, archP, nowMS())
			var archivePlan map[string]any
			if err := callFFIInto(ret, &archivePlan); err != nil {
				return nil, err
			}
			if cmd, ok := archivePlan["command"]; ok && cmd != nil {
				archB, err := json.Marshal(cmd)
				if err != nil {
					return nil, err
				}
				t, err := s.translateCommand(archB)
				if err != nil {
					return nil, err
				}
				arch, err := s.execTranslated(ctx, conn, t)
				if err != nil {
					return nil, err
				}
				archivedCount = arch.Changes
			}
		}
	}

	// ④ 删除
	deleteB, err := json.Marshal(deleteCommand)
	if err != nil {
		return nil, err
	}
	t, err := s.translateCommand(deleteB)
	if err != nil {
		return nil, err
	}
	outcome, err := s.execTranslated(ctx, conn, t)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"deletedCount":  outcome.Changes,
		"archivedCount": archivedCount,
	}, nil
}

// probeArg 把探针结果编码为 core Probe 契约（"no_result" / {"found":{...}}）。
func probeArg(found map[string]any) any {
	if found == nil {
		return "no_result"
	}
	return map[string]any{"found": found}
}

// runProbe 执行探针命令（cmd_find_one）取条件命中文档。
func (s *Store) runProbe(ctx context.Context, probeCmd json.RawMessage) (map[string]any, error) {
	return s.runProbeRaw(ctx, json.RawMessage(probeCmd))
}

func (s *Store) runProbeRaw(ctx context.Context, probeCmd any) (map[string]any, error) {
	b, err := json.Marshal(probeCmd)
	if err != nil {
		return nil, err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	t, err := s.translateCommand(b)
	if err != nil {
		return nil, err
	}
	outcome, err := s.execTranslated(ctx, conn, t)
	if err != nil {
		return nil, err
	}
	if len(outcome.Docs) == 0 {
		return nil, nil
	}
	return outcome.Docs[0], nil
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
