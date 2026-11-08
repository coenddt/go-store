# Changelog — go-store

本文件记录 go-store（`rust-store/core` 的 Go 宿主绑定）的行为变更。
格式遵循 Keep a Changelog；版本号由发布流程统一 bump。

## 0.1.0 (2026-11-08)

> 首个发布版本定为 **0.1.0**（未发布）。首发即包含下列两项行为变化，部署前请按 `Migration` 一节核对旧库。

### Breaking

#### 1. DDL 物理名改走 `core::naming`（经 FFI `rcore_translate_name`），宿主不再自研转换

- 表名（含归档表 `<collection>_deleted`）与全部列名统一透出 snake_case：
  `createdAt` → `created_at`、`updatedAt` → `updated_at`、`deletedAt` → `deleted_at`、`createdBy` → `created_by`。
- **下游要动哪里**：
  1. 依赖旧 camelCase 表名 / 列名执行手写 SQL、报表或 BI 直连的，全部改为 snake_case；
  2. 旧库不会自动迁移，按 `Migration` 一节重建或单独迁移。
- **绑定版本要求**：`rust_store_ffi.dll` 必须导出 `rcore_translate_name`
  （即 `rust-store/core-ffi/src/lib.rs` 含该导出之后的构建产物；core-ffi 未单列 semver，以仓库 HEAD 为准）。
  旧 dll 会在 `Open` 首次调用时**直接失败**（`load()` 进程级幂等），不会静默降级：
  - 缺符号实测原文：`Open 失败: cdylib 缺少导出符号 rcore_translate_name: GetProcAddress(rcore_translate_name): The specified procedure could not be found.`
  - dll 缺失实测原文：`Open 失败: 加载 rust-store cdylib 失败（路径 "C:\\nonexistent\\rust_store_ffi.dll"；应先构建 core-ffi，见 README）: The specified module could not be found.`
- 部署要求：go-store 二进制与 `rust_store_ffi.dll` **必须同批升级**；混用版本会在启动/首次 `Open` 即报错。

#### 2. `vector` 字段的列类型固定为 `TEXT`

- **改了什么**：此前 `vector` 不在 `sqliteTypes` 映射表内，`EnsureTables` 对含 `vector` 字段的 schema 直接报错
  `字段 <f> 类型 vector 不在 SQLite v0 类型表内`，根本无法建表；现显式映射为 `TEXT`（JSON 文本，与
  `object` / `array` 同档），读侧由 core row 还原为向量。
- **下游要动哪里**：这不是把可用能力改坏，而是把「建表被拒」放行——若此前用手写建表绕过并选了别的列类型，
  需把该列对齐为 `TEXT` 并保证存放 JSON 文本，否则读取时 core 无法还原。

### Added

- `SchemaFields(schema string) map[string]any`：返回已注册 schema 的 `fields` 定义树（只读，供协议皮按
  `store-api/spec/02` 生成上传可写点路径白名单；宿主本身不做任何白名单判决）。

### Fixed

- **DDL 列名与执行期 SQL 列名不一致**：旧实现建表写 `"createdAt"` / `"updatedAt"`，执行期 SQL 用
  `created_at` / `updated_at`，导致 `table user has no column named created_at`（并连带打挂 store-api/go 与
  store-graphql/go）。现两端同源于 `core::naming::physical_of`。
- **同类不一致一并收口**：
  - 表名与归档表名物理化（此前建 `orderItem`、查 `order_item`）；
  - creator 归属键列补齐（`ownerKey`，缺省 `createdBy` → `created_by`），与 core `registry::add_owner_field` 同规则；
  - 归档表 `deletedAt` → `deleted_at`；
  - `_id` 与 `__` 前缀列保持原样（core 保留名契约）。

### Migration

- **`CREATE TABLE IF NOT EXISTS` 不会迁移已存在的旧表**：表名与列名都变了，升级后对旧库执行期 SQL 会报
  `no such table` / `no such column`。v0 无迁移引擎，处理方式：
  1. 可丢弃的库（本地 / 测试 / 边缘缓存）：删除旧库文件，由 `EnsureTables` 重建；
  2. 需保留数据的库：停机窗口内单独迁移（表重命名 + 列重命名 + `<collection>Deleted` → `<collection>_deleted`），
     或「导出 → 重建 → 回灌」；迁移前先备份。
- 受影响判定：`SELECT name FROM sqlite_master WHERE type='table'` 出现含大写字母的表名，或
  `pragma_table_info('<table>')` 出现 `createdAt` / `updatedAt` / `createdBy` / `deletedAt` 等 camelCase 列。
- **`vector` 列**：若旧库由手写建表生成且列类型不是 `TEXT`，读取会失败；迁移时把该列改为 `TEXT` 并确保存量值是
  JSON 数组文本（v0 无列类型迁移引擎，同样走「重建 / 导出回灌」）。

### CI（已落地）

- 新增 `.github/workflows/ci.yml`：`windows-latest` + 兄弟目录 checkout（`rust-store` / `go-store`）
  → `cargo build -p rust-store-ffi --locked` → 校验 dll 存在并打印 SHA256 → `RUST_STORE_FFI` 注入
  → `go test ./... -count=1`。
- GitHub 可运行性：设计上可跑；前提是 `coenddt/rust-store` 公开或配置 `RUST_STORE_TOKEN`，
  且 `rust-store/Cargo.lock` 可 `--locked` 解析。本机网络无法访问 github.com，连通性未实测，
  属「待核实」；首次触发后需核对 dll SHA256 与测试输出。
