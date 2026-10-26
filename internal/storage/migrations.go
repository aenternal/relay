package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version             int
	name, sql, checksum string
}

func readMigrations(files fs.FS) ([]migration, error) {
	names, err := fs.Glob(files, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("storage: list migrations failed")
	}
	var all []migration
	seen := map[int]bool{}
	for _, name := range names {
		base := path.Base(name)
		n, _, ok := strings.Cut(base, "_")
		version, e := strconv.Atoi(n)
		if !ok || e != nil || version < 1 || seen[version] {
			return nil, fmt.Errorf("storage: invalid or duplicate migration version")
		}
		seen[version] = true
		data, e := fs.ReadFile(files, name)
		if e != nil {
			return nil, fmt.Errorf("storage: read migration failed")
		}
		all = append(all, migration{version: version, name: base, sql: string(data), checksum: fmt.Sprintf("%x", sha256.Sum256(data))})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].version < all[j].version })
	for i, m := range all {
		if m.version != i+1 {
			return nil, fmt.Errorf("storage: migrations must be consecutive")
		}
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("storage: no migrations")
	}
	return all, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	return migrateFiles(ctx, db, migrationFiles)
}

func migrateFiles(ctx context.Context, db *sql.DB, files fs.FS) error {
	all, err := readMigrations(files)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(ctx, "begin migration", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
        version INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, checksum TEXT NOT NULL,
        applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	if err != nil {
		return databaseError(ctx, "create migration ledger", err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return databaseError(ctx, "read migration ledger", err)
	}
	applied := 0
	for rows.Next() {
		var version int
		var name, checksum string
		if err = rows.Scan(&version, &name, &checksum); err != nil {
			rows.Close()
			return databaseError(ctx, "read migration ledger", err)
		}
		if version != applied+1 || version > len(all) {
			rows.Close()
			return fmt.Errorf("storage: unsupported schema version")
		}
		m := all[version-1]
		if name != m.name || checksum != m.checksum {
			rows.Close()
			return fmt.Errorf("storage: migration checksum mismatch")
		}
		applied++
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return databaseError(ctx, "read migration ledger", err)
	}
	rows.Close()
	var userVersion int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		return databaseError(ctx, "read schema version", err)
	}
	if userVersion != applied {
		return fmt.Errorf("storage: unsupported schema version")
	}
	for _, m := range all[applied:] {
		if _, err = tx.ExecContext(ctx, m.sql); err != nil {
			return databaseError(ctx, "apply migration", err)
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,name,checksum) VALUES(?,?,?)", m.version, m.name, m.checksum); err != nil {
			return databaseError(ctx, "record migration", err)
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			return databaseError(ctx, "write schema version", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return databaseError(ctx, "commit migration", err)
	}
	return nil
}
