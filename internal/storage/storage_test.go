package storage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"relay/internal/domain"
)

func fixture() domain.State {
	return domain.State{
		Workspaces:  []domain.Workspace{{ID: "w", Name: "Workspace", ActiveEnvironmentID: "e"}},
		Collections: []domain.Collection{{ID: "c", WorkspaceID: "w", Name: "Collection"}},
		// Child before parent also exercises deferred parent foreign keys.
		Folders:      []domain.Folder{{ID: "child", WorkspaceID: "w", CollectionID: "c", ParentID: "parent", Name: "Child"}, {ID: "parent", WorkspaceID: "w", CollectionID: "c", Name: "Parent"}},
		Requests:     []domain.RequestDefinition{{ID: "r", WorkspaceID: "w", CollectionID: "c", FolderID: "child", Name: "Request", Protocol: "http", HTTP: &domain.HTTPDefinition{Method: "POST", URL: "https://{{host}}/path?access_token={{token}}", Version: "auto", Headers: []domain.Header{{Name: "Authorization", Value: "Bearer {{token}}", Enabled: true}}, Body: "transient request body"}}},
		Environments: []domain.Environment{{ID: "e", WorkspaceID: "w", Name: "Local", Variables: []domain.Variable{{ID: "v", Name: "host", Value: "example.test"}, {ID: "secret", Name: "token", Secret: true, SecretRef: "keyring-reference"}}}},
		Settings:     domain.DefaultSettings(),
		UI:           domain.UIState{WorkspaceID: "w", OpenRequestIDs: []string{"r"}, ActiveRequestID: "r"},
	}
}

func openTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "private", "relay.sqlite")
	db, err := Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, filename
}

func TestMigrateReopenAndCRUD(t *testing.T) {
	ctx := context.Background()
	db, filename := openTestDB(t)
	empty, err := db.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(empty, emptyState()) {
		t.Fatalf("empty state = %#v", empty)
	}
	source := fixture()
	saved, err := db.Save(ctx, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 || saved.Requests[0].HTTP.Body != "" {
		t.Fatalf("unexpected saved revision/body")
	}
	if source.Requests[0].HTTP.Body == "" {
		t.Fatal("Save modified caller draft")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reopened, err := db.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved, reopened) {
		t.Fatalf("state changed after reopen\nsaved %#v\nloaded %#v", saved, reopened)
	}
	reopened.Collections[0].Name = "Renamed"
	reopened.Folders = nil
	reopened.Requests[0].FolderID = ""
	reopened.Settings.Theme = "dark"
	reopened.Settings.HistoryLimit = 500
	reopened, err = db.Save(ctx, reopened, reopened.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Revision != 2 || reopened.Collections[0].Name != "Renamed" {
		t.Fatal("update failed")
	}
	if _, err = db.Save(ctx, source, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale save = %v", err)
	}
	deleted, err := db.Save(ctx, domain.State{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Revision != 3 {
		t.Fatal("delete did not increment revision")
	}
	for _, table := range []string{"workspaces", "collections", "folders", "requests", "environments", "variables", "ui_open_requests"} {
		var n int
		if err = db.sql.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s left rows=%d error=%v", table, n, err)
		}
	}
	var version, foreignKeys int
	var journal string
	if err = db.sql.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 4 {
		t.Fatalf("schema version %d: %v", version, err)
	}
	if err = db.sql.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign keys %d: %v", foreignKeys, err)
	}
	if err = db.sql.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal %q: %v", journal, err)
	}
	if info, err := os.Stat(filename); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("database mode: %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Dir(filename)); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("directory mode: %v %v", info, err)
	}
}

func TestSaveRollsBackRevisionAndSnapshotOnWriteFailure(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	before, err := db.Save(ctx, fixture(), 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.sql.ExecContext(ctx, "CREATE TRIGGER reject_request BEFORE INSERT ON requests BEGIN SELECT RAISE(ABORT, 'PRIVATE_SQL_VALUE'); END")
	if err != nil {
		t.Fatal(err)
	}
	changed := before
	changed.Collections = append([]domain.Collection{}, before.Collections...)
	changed.Collections[0].Name = "Must roll back"
	if _, err = db.Save(ctx, changed, 1); err == nil || strings.Contains(err.Error(), "PRIVATE_SQL_VALUE") {
		t.Fatalf("error must be sanitized: %v", err)
	}
	after, err := db.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed save changed snapshot or revision")
	}
}

func TestForeignKeysEnforceOwnershipAndCascade(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	if _, err := db.Save(ctx, fixture(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx, "INSERT INTO requests(id,workspace_id,collection_id,payload) VALUES('broken','w','missing','{}')"); err == nil {
		t.Fatal("missing collection accepted")
	}
	if _, err := db.sql.ExecContext(ctx, "DELETE FROM collections WHERE id='c'"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"folders", "requests", "ui_open_requests"} {
		var n int
		if err := db.sql.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("cascade %s rows=%d: %v", table, n, err)
		}
	}
}

func TestConcurrentSavesHaveSingleWinner(t *testing.T) {
	ctx := context.Background()
	first, filename := openTestDB(t)
	second, err := Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var wg sync.WaitGroup
	results := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db := first
			if i%2 != 0 {
				db = second
			}
			_, e := db.Save(ctx, fixture(), 0)
			results <- e
		}(i)
	}
	wg.Wait()
	close(results)
	winners := 0
	for e := range results {
		if e == nil {
			winners++
		} else if !errors.Is(e, ErrConflict) {
			t.Fatal(e)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d", winners)
	}
	saved, err := first.Load(ctx)
	if err != nil || saved.Revision != 1 {
		t.Fatalf("revision = %d, err %v", saved.Revision, err)
	}
}

func TestSecretsAndUnapprovedBodiesNeverReachDatabaseOrWAL(t *testing.T) {
	ctx := context.Background()
	db, filename := openTestDB(t)
	s := fixture()
	const secret = "NEVER_PERSIST_THIS_SECRET_39ac12"
	const transient = "NEVER_PERSIST_THIS_BODY_84eb21"
	s.Requests[0].HTTP.Body = transient
	if _, err := db.Save(ctx, s, 0); err != nil {
		t.Fatal(err)
	}
	s.Environments[0].Variables[1].Value = secret
	if _, err := db.Save(ctx, s, 1); !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), secret) {
		t.Fatalf("secret save: %v", err)
	}
	for _, filename := range []string{filename, filename + "-wal", filename + "-shm"} {
		data, err := os.ReadFile(filename)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{secret, transient} {
			if bytes.Contains(data, []byte(private)) {
				t.Fatalf("private data persisted in %s", filepath.Base(filename))
			}
		}
	}
	s = fixture()
	s.Requests[0].HTTP.PersistBody = true
	s.Requests[0].HTTP.Body = "explicitly persisted"
	saved, err := db.Save(ctx, s, 1)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := db.Load(ctx)
	if err != nil || loaded.Requests[0].HTTP.Body != "explicitly persisted" || !reflect.DeepEqual(saved, loaded) {
		t.Fatal("approved body not restored", err)
	}
}

func TestInvalidStateRejectedWithoutChangingRevision(t *testing.T) {
	tests := map[string]func(*domain.State){
		"duplicate ID":               func(s *domain.State) { s.Collections[0].ID = "w" },
		"empty ID":                   func(s *domain.State) { s.Workspaces[0].ID = "" },
		"foreign workspace":          func(s *domain.State) { s.Collections[0].WorkspaceID = "missing" },
		"folder cycle":               func(s *domain.State) { s.Folders[1].ParentID = "child" },
		"folder parent missing":      func(s *domain.State) { s.Folders[0].ParentID = "missing" },
		"foreign collection":         func(s *domain.State) { s.Requests[0].CollectionID = "missing" },
		"foreign request folder":     func(s *domain.State) { s.Requests[0].FolderID = "missing" },
		"wrong protocol":             func(s *domain.State) { s.Requests[0].Protocol = "grpc" },
		"multiple protocols":         func(s *domain.State) { s.Requests[0].GRPC = &domain.GRPCDefinition{} },
		"active environment missing": func(s *domain.State) { s.Workspaces[0].ActiveEnvironmentID = "missing" },
		"variable duplicate":         func(s *domain.State) { s.Environments[0].Variables[1].Name = "host" },
		"variable ID duplicate":      func(s *domain.State) { s.Environments[0].Variables[1].ID = "v" },
		"public keyring reference":   func(s *domain.State) { s.Environments[0].Variables[0].SecretRef = "reference" },
		"invalid open tab":           func(s *domain.State) { s.UI.OpenRequestIDs = []string{"missing"} },
		"active closed tab":          func(s *domain.State) { s.UI.OpenRequestIDs = nil },
		"duplicate open tab":         func(s *domain.State) { s.UI.OpenRequestIDs = []string{"r", "r"} },
		"unknown theme":              func(s *domain.State) { s.Settings.Theme = "unknown" },
		"negative history":           func(s *domain.State) { s.Settings.HistoryLimit = -1 },
		"too large body":             func(s *domain.State) { s.Requests[0].HTTP.Body = strings.Repeat("x", maxBodyBytes+1) },
		"literal auth":               func(s *domain.State) { s.Requests[0].HTTP.Headers[0].Value = "Bearer NEVER_LOG_THIS" },
		"literal cookie": func(s *domain.State) {
			s.Requests[0].HTTP.Headers = []domain.Header{{Name: "Cookie", Value: "session=NEVER_LOG_THIS"}}
		},
		"literal API key": func(s *domain.State) {
			s.Requests[0].HTTP.Headers = []domain.Header{{Name: "X-Api-Key", Value: "NEVER_LOG_THIS"}}
		},
		"URL userinfo":        func(s *domain.State) { s.Requests[0].HTTP.URL = "https://user:NEVER_LOG_THIS@example.test" },
		"literal token query": func(s *domain.State) { s.Requests[0].HTTP.URL = "https://example.test?access_token=NEVER_LOG_THIS" },
	}
	db, _ := openTestDB(t)
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s := fixture()
			mutate(&s)
			_, err := db.Save(context.Background(), s, 0)
			if !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "NEVER_LOG_THIS") {
				t.Fatalf("expected sanitized invalid error, got %v", err)
			}
		})
	}
	s, err := db.Load(context.Background())
	if err != nil || s.Revision != 0 {
		t.Fatalf("invalid save changed revision: %d %v", s.Revision, err)
	}
}

func TestMigrationsRejectDuplicateAndGaps(t *testing.T) {
	for name, files := range map[string]fstest.MapFS{
		"duplicate":   {"migrations/001_first.sql": {Data: []byte("SELECT 1;")}, "migrations/1_second.sql": {Data: []byte("SELECT 2;")}},
		"gap":         {"migrations/002_first.sql": {Data: []byte("SELECT 1;")}},
		"non numeric": {"migrations/not_a_version.sql": {Data: []byte("SELECT 1;")}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readMigrations(files); err == nil {
				t.Fatal("invalid migration accepted")
			}
		})
	}
}

func TestMigrationsRejectUnsupportedOrChangedSchema(t *testing.T) {
	for name, query := range map[string]string{
		"future user version": "PRAGMA user_version=99",
		"future ledger":       "INSERT INTO schema_migrations(version,name,checksum) VALUES(99,'099_future.sql','unknown')",
		"checksum mismatch":   "UPDATE schema_migrations SET checksum='changed' WHERE version=1",
	} {
		t.Run(name, func(t *testing.T) {
			db, filename := openTestDB(t)
			if _, err := db.sql.Exec(query); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if reopened, err := Open(filename); err == nil {
				reopened.Close()
				t.Fatal("unsupported schema opened")
			}
		})
	}
}

func TestMigrationFailureRollsBackAllDDLAndLedger(t *testing.T) {
	db, _ := openTestDB(t)
	initial, err := os.ReadFile("migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := os.ReadFile("migrations/002_secret_cleanup.sql")
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := os.ReadFile("migrations/003_proto_schemas.sql")
	if err != nil {
		t.Fatal(err)
	}
	history, err := os.ReadFile("migrations/004_websocket_sessions.sql")
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{
		"migrations/001_initial.sql":            {Data: initial},
		"migrations/002_secret_cleanup.sql":     {Data: cleanup},
		"migrations/003_proto_schemas.sql":      {Data: schemas},
		"migrations/004_websocket_sessions.sql": {Data: history},
		"migrations/005_failure.sql":            {Data: []byte("CREATE TABLE should_rollback(id TEXT); INSERT INTO missing_table VALUES(1);")},
	}
	if err := migrateFiles(context.Background(), db.sql, files); err == nil {
		t.Fatal("broken migration succeeded")
	}
	var n int
	if err := db.sql.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='should_rollback'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("DDL survived rollback: %d %v", n, err)
	}
	if err := db.sql.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&n); err != nil || n != 4 {
		t.Fatalf("ledger survived rollback: %d %v", n, err)
	}
}

func TestVersionOneDatabaseUpgradesAndRetainsData(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "version_one.sqlite")
	legacy, err := sql.Open("sqlite", filename+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	initial, err := os.ReadFile("migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err = migrateFiles(context.Background(), legacy, fstest.MapFS{"migrations/001_initial.sql": {Data: initial}}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO workspaces(id,payload) VALUES('w','{"id":"w","name":"Legacy"}')`,
		`INSERT INTO environments(id,workspace_id,payload) VALUES('e','w','{"id":"e","workspaceId":"w","name":"Legacy environment"}')`,
		`INSERT INTO variables(id,environment_id,name,payload) VALUES('v','e','token','{"id":"v","name":"token","secret":true,"secretRef":"legacy-reference"}')`,
	} {
		if _, err = legacy.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err = legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := db.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Workspaces) != 1 || s.Workspaces[0].Name != "Legacy" || s.Environments[0].Variables[0].SecretRef != "legacy-reference" {
		t.Fatal("upgrade lost state")
	}
	if _, err = db.Save(context.Background(), domain.State{}, s.Revision); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingSecretCleanup(context.Background())
	if err != nil || !reflect.DeepEqual(pending, []string{"legacy-reference"}) {
		t.Fatalf("legacy secret reference not indexed: %v %v", pending, err)
	}
}

func TestSecretCleanupIsDurableAndPromotionPreservesLiveReferences(t *testing.T) {
	ctx := context.Background()
	db, filename := openTestDB(t)
	if err := db.QueueSecretCleanup(ctx, "new-reference"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueueSecretCleanup(ctx, "new-reference"); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingSecretCleanup(ctx)
	if err != nil || !reflect.DeepEqual(pending, []string{"new-reference"}) {
		t.Fatalf("pending %v: %v", pending, err)
	}
	s := fixture()
	s.Environments[0].Variables[1].SecretRef = "new-reference"
	saved, err := db.Save(ctx, s, 0)
	if err != nil {
		t.Fatal(err)
	}
	pending, err = db.PendingSecretCleanup(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("promoted reference pending %v: %v", pending, err)
	}
	// Even explicitly queued live references cannot become cleanup candidates.
	if err = db.QueueSecretCleanup(ctx, "new-reference"); err != nil {
		t.Fatal(err)
	}
	pending, err = db.PendingSecretCleanup(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("live reference pending %v: %v", pending, err)
	}
	saved.Environments[0].Variables = saved.Environments[0].Variables[:1]
	saved, err = db.Save(ctx, saved, saved.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pending, err = db.PendingSecretCleanup(ctx)
	if err != nil || !reflect.DeepEqual(pending, []string{"new-reference"}) {
		t.Fatalf("removed reference not durable %v: %v", pending, err)
	}
	if err = db.AcknowledgeSecretCleanup(ctx, "new-reference"); err != nil {
		t.Fatal(err)
	}
	pending, err = db.PendingSecretCleanup(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("acknowledgement failed %v: %v", pending, err)
	}
	loaded, err := db.Load(ctx)
	if err != nil || loaded.Revision != saved.Revision {
		t.Fatalf("queue operation changed revision: %v", err)
	}
}

func TestSecretCleanupRollsBackWithFailedSnapshot(t *testing.T) {
	ctx := context.Background()
	db, _ := openTestDB(t)
	saved, err := db.Save(ctx, fixture(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.sql.Exec("CREATE TRIGGER reject_request BEFORE INSERT ON requests BEGIN SELECT RAISE(ABORT, 'reject'); END"); err != nil {
		t.Fatal(err)
	}
	saved.Environments[0].Variables = saved.Environments[0].Variables[:1]
	if _, err = db.Save(ctx, saved, saved.Revision); err == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	var n int
	if err = db.sql.QueryRow("SELECT count(*) FROM secret_cleanup").Scan(&n); err != nil || n != 0 {
		t.Fatalf("rolled-back reference remained queued: %d %v", n, err)
	}
}

func TestCanceledOperations(t *testing.T) {
	db, _ := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := db.Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load cancellation = %v", err)
	}
	if _, err := db.Save(ctx, fixture(), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Save cancellation = %v", err)
	}
}
