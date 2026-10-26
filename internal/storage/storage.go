// Package storage persists application snapshots in a private, versioned SQLite database.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
	"relay/internal/domain"
)

var (
	ErrConflict = errors.New("storage: state revision conflict")
	ErrInvalid  = errors.New("storage: invalid state")
)

type DB struct{ sql *sql.DB }

// Open creates a database and migrates it before returning. Newly created parent
// directories are private; callers should use a dedicated application directory.
func Open(filename string) (*DB, error) {
	if strings.TrimSpace(filename) == "" || filename == ":memory:" {
		return nil, fmt.Errorf("storage: database path required")
	}
	abs, err := filepath.Abs(filename)
	if err != nil {
		return nil, fmt.Errorf("storage: invalid database path")
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, fmt.Errorf("storage: create database directory failed")
	}
	if info, e := os.Lstat(abs); e == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return nil, fmt.Errorf("storage: database must be a regular file")
	} else if e != nil && !os.IsNotExist(e) {
		return nil, fmt.Errorf("storage: inspect database failed")
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("storage: open database file failed")
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return nil, fmt.Errorf("storage: protect database file failed")
	}
	if err = f.Close(); err != nil {
		return nil, fmt.Errorf("storage: close database file failed")
	}
	uriPath := filepath.ToSlash(abs)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsn := &url.URL{Scheme: "file", Path: uriPath}
	q := dsn.Query()
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	dsn.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("storage: open SQLite failed")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err == nil {
		err = migrate(ctx, db)
	}
	if err != nil {
		db.Close()
		return nil, databaseError(ctx, "initialize database", err)
	}
	return &DB{sql: db}, nil
}

func (db *DB) Close() error {
	if err := db.sql.Close(); err != nil {
		return fmt.Errorf("storage: close database failed")
	}
	return nil
}

// databaseError deliberately omits driver error text: SQL diagnostics may contain
// user-supplied values. Context cancellation remains detectable with errors.Is.
func databaseError(ctx context.Context, operation string, err error) error {
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalid) {
		return err
	}
	if ctx.Err() != nil {
		return fmt.Errorf("storage: %s: %w", operation, ctx.Err())
	}
	return fmt.Errorf("storage: %s failed", operation)
}

func emptyState() domain.State {
	return domain.State{Workspaces: []domain.Workspace{}, Collections: []domain.Collection{}, Folders: []domain.Folder{}, Requests: []domain.RequestDefinition{}, Environments: []domain.Environment{}, ProtoSchemas: []domain.ProtoSchema{}, Settings: domain.DefaultSettings(), UI: domain.UIState{OpenRequestIDs: []string{}}}
}

func (db *DB) Load(ctx context.Context) (domain.State, error) {
	s := emptyState()
	tx, err := db.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return s, databaseError(ctx, "begin read", err)
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM state_metadata WHERE singleton=1").Scan(&s.Revision); err != nil {
		return emptyState(), databaseError(ctx, "read revision", err)
	}
	if err = readPayloads(ctx, tx, "workspaces", func(data []byte) error {
		var v domain.Workspace
		if e := json.Unmarshal(data, &v); e != nil {
			return e
		}
		s.Workspaces = append(s.Workspaces, v)
		return nil
	}); err != nil {
		return emptyState(), err
	}
	if err = readPayloads(ctx, tx, "collections", func(data []byte) error {
		var v domain.Collection
		if e := json.Unmarshal(data, &v); e != nil {
			return e
		}
		s.Collections = append(s.Collections, v)
		return nil
	}); err != nil {
		return emptyState(), err
	}
	if err = readPayloads(ctx, tx, "folders", func(data []byte) error {
		var v domain.Folder
		if e := json.Unmarshal(data, &v); e != nil {
			return e
		}
		s.Folders = append(s.Folders, v)
		return nil
	}); err != nil {
		return emptyState(), err
	}
	if err = readPayloads(ctx, tx, "requests", func(data []byte) error {
		var v domain.RequestDefinition
		if e := json.Unmarshal(data, &v); e != nil {
			return e
		}
		s.Requests = append(s.Requests, v)
		return nil
	}); err != nil {
		return emptyState(), err
	}
	if err = readPayloads(ctx, tx, "environments", func(data []byte) error {
		var v domain.Environment
		if e := json.Unmarshal(data, &v); e != nil {
			return e
		}
		v.Variables = []domain.Variable{}
		s.Environments = append(s.Environments, v)
		return nil
	}); err != nil {
		return emptyState(), err
	}
	if err = readProtoSchemas(ctx, tx, &s); err != nil {
		return emptyState(), err
	}
	envPositions := make(map[string]int, len(s.Environments))
	for i, env := range s.Environments {
		envPositions[env.ID] = i
	}
	rows, err := tx.QueryContext(ctx, "SELECT environment_id,payload FROM variables ORDER BY rowid")
	if err != nil {
		return emptyState(), databaseError(ctx, "read variables", err)
	}
	for rows.Next() {
		var env string
		var data []byte
		var v domain.Variable
		if err = rows.Scan(&env, &data); err != nil {
			break
		}
		if err = json.Unmarshal(data, &v); err != nil {
			break
		}
		i, ok := envPositions[env]
		if !ok {
			err = ErrInvalid
			break
		}
		s.Environments[i].Variables = append(s.Environments[i].Variables, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return emptyState(), databaseError(ctx, "read variables", err)
	}
	for _, item := range []struct {
		table string
		dst   any
	}{{"settings", &s.Settings}, {"ui_state", &s.UI}} {
		var data []byte
		err = tx.QueryRowContext(ctx, "SELECT payload FROM "+item.table+" WHERE singleton=1").Scan(&data)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return emptyState(), databaseError(ctx, "read preferences", err)
		}
		if json.Unmarshal(data, item.dst) != nil {
			return emptyState(), fmt.Errorf("storage: invalid stored preferences")
		}
	}
	if err = tx.Commit(); err != nil {
		return emptyState(), databaseError(ctx, "finish read", err)
	}
	clean, err := sanitize(s)
	if err != nil {
		return emptyState(), fmt.Errorf("storage: stored state failed validation")
	}
	return clean, nil
}

func readPayloads(ctx context.Context, tx *sql.Tx, table string, accept func([]byte) error) error {
	// table names are constants at each callsite, never user input.
	rows, err := tx.QueryContext(ctx, "SELECT payload FROM "+table+" ORDER BY rowid")
	if err != nil {
		return databaseError(ctx, "read records", err)
	}
	defer rows.Close()
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return databaseError(ctx, "read record", err)
		}
		if accept(data) != nil {
			return fmt.Errorf("storage: invalid stored record")
		}
	}
	if err = rows.Err(); err != nil {
		return databaseError(ctx, "read records", err)
	}
	return nil
}

// Save atomically replaces the snapshot if expectedRevision is still current.
// Bodies without explicit persistence consent are removed from both storage and
// the returned snapshot. The caller's input is never modified.
func (db *DB) Save(ctx context.Context, state domain.State, expectedRevision int64) (domain.State, error) {
	clean, err := sanitize(state)
	if err != nil {
		return domain.State{}, err
	}
	if expectedRevision < 0 || expectedRevision == int64(^uint64(0)>>1) {
		return domain.State{}, fmt.Errorf("%w: revision out of range", ErrInvalid)
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return domain.State{}, databaseError(ctx, "begin save", err)
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, "UPDATE state_metadata SET revision=revision+1 WHERE singleton=1 AND revision=?", expectedRevision)
	if err != nil {
		return domain.State{}, databaseError(ctx, "update revision", err)
	}
	n, err := r.RowsAffected()
	if err != nil {
		return domain.State{}, databaseError(ctx, "read updated revision", err)
	}
	if n != 1 {
		return domain.State{}, ErrConflict
	}
	// References become garbage in the same transaction that removes their owning
	// variables. A crash before vault deletion leaves a durable cleanup request.
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO secret_cleanup(ref) SELECT DISTINCT secret_ref FROM variables WHERE secret_ref <> ''"); err != nil {
		return domain.State{}, databaseError(ctx, "queue removed secrets", err)
	}
	for _, table := range []string{"ui_open_requests", "ui_state", "settings", "workspaces"} {
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return domain.State{}, databaseError(ctx, "replace state", err)
		}
	}
	put := func(query string, payload any, args ...any) error {
		data, e := json.Marshal(payload)
		if e != nil {
			return fmt.Errorf("%w: record encoding failed", ErrInvalid)
		}
		args = append(args, string(data))
		_, e = tx.ExecContext(ctx, query, args...)
		if e != nil {
			return databaseError(ctx, "write record", e)
		}
		return nil
	}
	for _, v := range clean.Workspaces {
		if err = put("INSERT INTO workspaces(id,active_environment_id,payload) VALUES(?,?,?)", v, v.ID, nullable(v.ActiveEnvironmentID)); err != nil {
			return domain.State{}, err
		}
	}
	for _, v := range clean.ProtoSchemas {
		if err = put("INSERT INTO proto_schemas(id,workspace_id,descriptor_set_base64,payload) VALUES(?,?,?,?)", v, v.ID, v.WorkspaceID, v.DescriptorSetBase64); err != nil {
			return domain.State{}, err
		}
	}
	for _, v := range clean.Collections {
		if err = put("INSERT INTO collections(id,workspace_id,payload) VALUES(?,?,?)", v, v.ID, v.WorkspaceID); err != nil {
			return domain.State{}, err
		}
	}
	for _, v := range clean.Folders {
		if err = put("INSERT INTO folders(id,workspace_id,collection_id,parent_id,payload) VALUES(?,?,?,?,?)", v, v.ID, v.WorkspaceID, v.CollectionID, nullable(v.ParentID)); err != nil {
			return domain.State{}, err
		}
	}
	for _, v := range clean.Requests {
		if err = put("INSERT INTO requests(id,workspace_id,collection_id,folder_id,payload) VALUES(?,?,?,?,?)", v, v.ID, v.WorkspaceID, v.CollectionID, nullable(v.FolderID)); err != nil {
			return domain.State{}, err
		}
	}
	for _, v := range clean.Environments {
		variables := v.Variables
		v.Variables = nil
		if err = put("INSERT INTO environments(id,workspace_id,payload) VALUES(?,?,?)", v, v.ID, v.WorkspaceID); err != nil {
			return domain.State{}, err
		}
		for _, variable := range variables {
			if err = put("INSERT INTO variables(id,environment_id,name,secret_ref,payload) VALUES(?,?,?,?,?)", variable, variable.ID, v.ID, variable.Name, variable.SecretRef); err != nil {
				return domain.State{}, err
			}
		}
	}
	if err = put("INSERT INTO settings(singleton,payload) VALUES(1,?)", clean.Settings); err != nil {
		return domain.State{}, err
	}
	if err = put("INSERT INTO ui_state(singleton,workspace_id,active_request_id,payload) VALUES(1,?,?,?)", clean.UI, nullable(clean.UI.WorkspaceID), nullable(clean.UI.ActiveRequestID)); err != nil {
		return domain.State{}, err
	}
	for i, id := range clean.UI.OpenRequestIDs {
		if _, err = tx.ExecContext(ctx, "INSERT INTO ui_open_requests(position,request_id,workspace_id) VALUES(?,?,?)", i, id, clean.UI.WorkspaceID); err != nil {
			return domain.State{}, databaseError(ctx, "write open tabs", err)
		}
	}
	// Promotion of a previously queued new reference and preservation of existing
	// variables both cancel cleanup. Never delete a vault entry still in use.
	if _, err = tx.ExecContext(ctx, "DELETE FROM secret_cleanup WHERE ref IN (SELECT secret_ref FROM variables WHERE secret_ref <> '')"); err != nil {
		return domain.State{}, databaseError(ctx, "preserve live secrets", err)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM websocket_sessions WHERE NOT EXISTS (
		SELECT 1 FROM requests WHERE requests.id=websocket_sessions.request_id
		AND requests.workspace_id=websocket_sessions.workspace_id
		AND json_extract(requests.payload,'$.protocol')='websocket')`); err != nil {
		return domain.State{}, databaseError(ctx, "remove orphaned WebSocket sessions", err)
	}
	if err = tx.Commit(); err != nil {
		return domain.State{}, databaseError(ctx, "commit save", err)
	}
	clean.Revision = expectedRevision + 1
	return clean, nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// QueueSecretCleanup persists only an opaque vault reference before a vault
// write. Saving a variable with that reference promotes it and removes the task.
func (db *DB) QueueSecretCleanup(ctx context.Context, ref string) error {
	if !validSecretReference(ref) {
		return invalid("invalid secret reference")
	}
	if _, err := db.sql.ExecContext(ctx, "INSERT OR IGNORE INTO secret_cleanup(ref) VALUES(?)", ref); err != nil {
		return databaseError(ctx, "queue secret cleanup", err)
	}
	return nil
}

func (db *DB) PendingSecretCleanup(ctx context.Context) ([]string, error) {
	refs := []string{}
	rows, err := db.sql.QueryContext(ctx, "SELECT ref FROM secret_cleanup WHERE NOT EXISTS (SELECT 1 FROM variables WHERE variables.secret_ref = secret_cleanup.ref) ORDER BY ref")
	if err != nil {
		return nil, databaseError(ctx, "read pending secret cleanup", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, databaseError(ctx, "read cleanup reference", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(ctx, "read pending secret cleanup", err)
	}
	return refs, nil
}

func (db *DB) AcknowledgeSecretCleanup(ctx context.Context, ref string) error {
	if !validSecretReference(ref) {
		return invalid("invalid secret reference")
	}
	if _, err := db.sql.ExecContext(ctx, "DELETE FROM secret_cleanup WHERE ref=?", ref); err != nil {
		return databaseError(ctx, "acknowledge secret cleanup", err)
	}
	return nil
}
