package storage

import (
	"errors"
	"reflect"
	"testing"

	"relay/internal/domain"
	"relay/internal/project"
)

func TestProjectAppendCommitsArtifactsAndRollsBackWholeSnapshot(t *testing.T) {
	db, _ := openTestDB(t)
	initial := fixture()
	initial.ProtoSchemas = []domain.ProtoSchema{schemaFixture(t)}
	saved, err := db.Save(t.Context(), initial, 0)
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := project.Export(t.Context(), saved, "w", project.Options{})
	if err != nil {
		t.Fatal(err)
	}
	doc, err = project.Reidentify(doc)
	if err != nil {
		t.Fatal(err)
	}
	incoming := project.State(doc)
	candidate := saved
	candidate.Workspaces = append(append([]domain.Workspace{}, saved.Workspaces...), incoming.Workspaces...)
	candidate.Collections = append(append([]domain.Collection{}, saved.Collections...), incoming.Collections...)
	candidate.Folders = append(append([]domain.Folder{}, saved.Folders...), incoming.Folders...)
	candidate.Requests = append(append([]domain.RequestDefinition{}, saved.Requests...), incoming.Requests...)
	candidate.Environments = append(append([]domain.Environment{}, saved.Environments...), incoming.Environments...)
	candidate.ProtoSchemas = append(append([]domain.ProtoSchema{}, saved.ProtoSchemas...), incoming.ProtoSchemas...)
	if err = ValidateProjectState(candidate); err != nil {
		t.Fatal(err)
	}
	// Fail after workspaces and descriptor artifacts have already been written.
	// SQLite must restore the previous snapshot, revision and secret outbox.
	if _, err = db.sql.Exec(`CREATE TRIGGER fail_project_write BEFORE INSERT ON requests WHEN NEW.id <> 'r' BEGIN SELECT RAISE(ABORT,'project write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Save(t.Context(), candidate, saved.Revision); err == nil {
		t.Fatal("expected transaction failure")
	}
	after, err := db.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, saved) {
		t.Fatal("failed import changed the persisted snapshot")
	}
	refs, err := db.PendingSecretCleanup(t.Context())
	if err != nil || len(refs) != 0 {
		t.Fatal("failed import changed secret cleanup outbox")
	}
	if _, err = db.sql.Exec(`DROP TRIGGER fail_project_write`); err != nil {
		t.Fatal(err)
	}
	committed, err := db.Save(t.Context(), candidate, saved.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(committed.Workspaces) != 2 || len(committed.ProtoSchemas) != 2 || committed.Settings != saved.Settings || !reflect.DeepEqual(committed.UI, saved.UI) {
		t.Fatal("append lost project or existing preferences")
	}
	if _, err = db.Save(t.Context(), candidate, saved.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale append: %v", err)
	}
	after, err = db.Load(t.Context())
	if err != nil || !reflect.DeepEqual(after, committed) {
		t.Fatal("CAS conflict changed state")
	}
}

func TestProjectValidationHasNoPersistenceOrAliasing(t *testing.T) {
	db, _ := openTestDB(t)
	saved, err := db.Save(t.Context(), fixture(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateProjectState(saved); err != nil {
		t.Fatal(err)
	}
	bad := saved
	bad.Requests = append([]domain.RequestDefinition{}, saved.Requests...)
	bad.Requests[0].FolderID = "not-in-project"
	if err = ValidateProjectState(bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid ownership accepted: %v", err)
	}
	after, err := db.Load(t.Context())
	if err != nil || !reflect.DeepEqual(after, saved) {
		t.Fatal("preview validation modified persisted state")
	}
}
