# go-store

**The Go host binding for `rust-store-core`** — the Go twin of `nodejs-store`, `py-store` and `rust-store(host)`.
Define models in plain JSON schema, query them with MongoDB-style GQL tree syntax, and get RBAC, computed columns
and soft-delete archiving built in. Current line: **v0, SQLite only**, released as **0.1.0**.

中文（权威版本）: [README.md](./README.md) · Behaviour changes: [CHANGELOG.md](./CHANGELOG.md)

> The Chinese README is the authoritative document; this file is its English companion and tracks the same contract.
> Semantics live in exactly one place (Rust core). The Go host only performs IO and command-sequence orchestration,
> so it does not drift from Node / Python.

## Release status

- Version **0.1.0** (first release; no tag cut yet; the release pipeline owns the bump).
- Pure Go, does **not** join `release-all`'s protocol-skin batch release.
- **Upgrade notes for 0.1.0**: physical DDL names are snake_case (`created_at`, `updated_at`, `deleted_at`,
  `created_by`), archive tables are `<collection>_deleted`, and `vector` columns are `TEXT`. Existing databases are
  not migrated automatically — see the `Migration` section of the changelog. The `rust_store_ffi.dll` must ship in
  the same batch as the binary (it must export `rcore_translate_name`).

## What it is

| You declare in JSON schema | go-store gives you |
|---|---|
| model `user` | `Register` into the core registry, `EnsureTables` for main + archive table |
| fields with types | scalar columns; `object` / `array` / `vector` become `TEXT` (JSON text, restored by core on read) |
| `timestamps !== false` | `created_at` / `updated_at` (INTEGER ms) plus the `__present` sentinel column |
| RBAC config | `SetRbac` / `SetExemptRoles` / `SetDenyWriteRoles` / `SetUnconfiguredPolicy` + read-only query helpers |
| computed columns | `RegisterCompute` (async batch bridge, recommended) / `RegisterSyncCompute` (sync) / inline `agg` |

The host is a thin shell: no permission logic of its own, no dialect logic of its own. Both come from core.

## Scope (v0)

Supported: SQLite backend (`sqlite://path.db`, `sqlite::memory:`), `Open` / `Close` / `Register` /
`SchemaFieldsKeys` / `SchemaFields` / `ListSchemas`, `Query` / `QueryOne` / `QueryWithCount`,
`Insert` / `Update` / `Remove`, computed columns (`fn` / `asyncFn` / `agg`), RBAC injection,
DDL for scalar columns + timestamps + `__present` + archive tables.

Not in v0: MySQL / PostgreSQL / MongoDB / local backends, index and autoincrement DDL, transactions,
triggers / federation / mutation-step orchestration, non-Windows builds. Unsupported calls fail explicitly;
nothing degrades silently.

## Installation and prerequisites

```go
module github.com/coenddt/go-store   // go.mod declares go 1.26.0
package gostore                       // package name: gostore
```

```go
import gostore "github.com/coenddt/go-store"
```

| Requirement | Detail |
|---|---|
| Go | `go 1.26.0` (declared in `go.mod`) |
| C toolchain / cgo | **not needed** — `ffi.go` loads the library at runtime via purego |
| SQLite driver | pure-Go `modernc.org/sqlite`, registered as `"sqlite"` |
| core-ffi artifact | cdylib `rust_store_ffi.dll` (crate `rust-store-ffi`), **must export `rcore_translate_name`** |
| OS | **Windows only** (`load_windows.go` + `kernel32.dll`) |

Build the artifact in `rust-store/`:

```bash
cargo build -p rust-store-ffi --release   # → target/release/rust_store_ffi.dll
```

Library path resolution (`defaultLibraryPath`): `RUST_STORE_FFI` env var → `../rust-store/target/{release,debug}` →
`../../rust-store/target/{release,debug}` → `./rust_store_ffi.dll`. You can also set `gostore.LibraryPath` before
first use. Loading is process-wide idempotent (`sync.Once`): the first failure is returned forever, never silently
downgraded.

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"log"

	gostore "github.com/coenddt/go-store"
)

func main() {
	store, err := gostore.Open("sqlite::memory:") // or "sqlite://app.db"
	if err != nil {
		log.Fatal(err) // a failed cdylib load surfaces here
	}
	defer store.Close()

	userSchema := map[string]any{
		"name":       "user",
		"collection": "user",
		"idPrefix":   "u",
		"fields": map[string]any{
			"name": map[string]any{"type": "string"},
			"age":  map[string]any{"type": "number"},
		},
	}
	if err := store.Register(userSchema); err != nil {
		log.Fatal(err)
	}
	if err := store.EnsureTables(userSchema); err != nil { // main + archive table
		log.Fatal(err)
	}

	ctx := context.Background()
	doc, err := store.Insert(ctx, "user", map[string]any{"name": "alice", "age": int64(30)}, nil)
	if err != nil {
		log.Fatal(err)
	}

	rows, err := store.Query(ctx, "user($condition: @c0) { name, age }",
		map[string]any{"c0": map[string]any{"age": map[string]any{"$gte": 18}}}, nil)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(doc["_id"])
	fmt.Println(rows)
}
```

The fourth argument is the permission context `*gostore.Context` (`nil` = no RBAC trimming). Do not confuse it with
the method's `ctx context.Context` (cancellation/timeout) — different types, different jobs.

## Physical names and migration

DDL goes through `core::naming` via the FFI `rcore_translate_name` call. Table names (including the archive table
`<collection>_deleted`) and every column name are snake_case. `EnsureTables` uses `CREATE TABLE IF NOT EXISTS`, so it
does **not** migrate existing tables: after upgrading, runtime SQL against an old camelCase database fails with
`no such table` / `no such column`. v0 ships no migration engine — either delete disposable databases and let
`EnsureTables` rebuild, or migrate during a downtime window (rename tables and columns, `<collection>Deleted` →
`<collection>_deleted`), or export → rebuild → re-import. Back up first.

Detection: `SELECT name FROM sqlite_master WHERE type='table'` shows tables containing uppercase letters, or
`pragma_table_info('<table>')` shows `createdAt` / `updatedAt` / `createdBy` / `deletedAt`.

## Testing

```powershell
$env:RUST_STORE_FFI="…\rust-store\target\debug\rust_store_ffi.dll"
go test ./... -count=1
```

Inject the dll explicitly instead of relying on path probing. CI does the same on `windows-latest`
(checkout of the sibling `rust-store` / `go-store` directories → `cargo build -p rust-store-ffi --locked` →
print the dll SHA256 → run the suite).

## Related projects

`rust-store` (core + host), `nodejs-store`, `py-store`, `store-api` / `store-graphql` / `store-grpc` / `store-mcp`
(the protocol skins that consume this host and the other runtimes).

## License

MIT — see [LICENSE](./LICENSE).
