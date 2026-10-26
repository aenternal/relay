package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"relay/internal/domain"
	"relay/internal/project"
	"relay/internal/storage"
)

func (s *Service) ExportProject(ctx context.Context, workspaceID string, includeBodies bool) (domain.ProjectExport, error) {
	work, done, err := acquireProtoWork(ctx, s.ctx)
	if err != nil {
		return domain.ProjectExport{}, err
	}
	defer done()
	state, err := s.LoadState(work)
	if err != nil {
		return domain.ProjectExport{}, err
	}
	doc, warnings, err := project.Export(work, state, workspaceID, project.Options{IncludeBodies: includeBodies})
	if err != nil {
		return domain.ProjectExport{}, err
	}
	if err = storage.ValidateProjectState(project.State(doc)); err != nil {
		return domain.ProjectExport{}, errors.New("portable project failed validation")
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil || len(data) > project.MaxBytes {
		return domain.ProjectExport{}, errors.New("project exceeds the 32 MiB transfer limit")
	}
	if err = work.Err(); err != nil {
		return domain.ProjectExport{}, err
	}
	return domain.ProjectExport{Filename: "relay-project.json", Data: string(data), Warnings: warnings, Stats: project.Statistics(doc)}, nil
}

func parseProject(ctx context.Context, format, data string) (project.Document, []project.Warning, error) {
	if len(data) > project.MaxBytes {
		return project.Document{}, nil, errors.New("project exceeds the 32 MiB transfer limit")
	}
	switch strings.ToLower(format) {
	case "native":
		return project.ParseNative(ctx, []byte(data))
	case "postman":
		return project.ParsePostman([]byte(data))
	case "insomnia":
		return project.ParseInsomnia([]byte(data))
	default:
		return project.Document{}, nil, errors.New("select native, Postman v2.1, or Insomnia v4 import format")
	}
}

func projectWarnings(first, second []project.Warning) []project.Warning {
	result := append([]project.Warning{}, first...)
	for _, warning := range second {
		found := false
		for _, old := range result {
			if old.Code == warning.Code && old.Path == warning.Path && old.Message == warning.Message {
				found = true
				break
			}
		}
		if !found && len(result) < project.MaxWarnings {
			result = append(result, warning)
		}
	}
	return result
}

func (s *Service) PreviewProjectImport(ctx context.Context, format, data string) (domain.ProjectPreview, error) {
	work, done, err := acquireProtoWork(ctx, s.ctx)
	if err != nil {
		return domain.ProjectPreview{}, err
	}
	defer done()
	doc, warnings, err := parseProject(work, format, data)
	if err != nil {
		return domain.ProjectPreview{}, err
	}
	doc, policyWarnings, err := project.Prepare(work, doc, project.Options{IncludeBodies: true})
	if err != nil {
		return domain.ProjectPreview{}, err
	}
	// Reidentification also checks ID uniqueness and dangling references. The
	// preview never persists these temporary IDs or touches the secret provider.
	checked, err := project.Reidentify(doc)
	if err != nil {
		return domain.ProjectPreview{}, err
	}
	if err = storage.ValidateProjectState(project.State(checked)); err != nil {
		return domain.ProjectPreview{}, errors.New("imported project has invalid settings or record ownership")
	}
	if err = work.Err(); err != nil {
		return domain.ProjectPreview{}, err
	}
	return domain.ProjectPreview{Format: strings.ToLower(format), WorkspaceName: doc.Workspace.Name, Warnings: projectWarnings(warnings, policyWarnings), Stats: project.Statistics(doc)}, nil
}

func (s *Service) ImportProject(ctx context.Context, format, data string, includeBodies bool) (domain.ProjectImportResult, error) {
	work, done, err := acquireProtoWork(ctx, s.ctx)
	if err != nil {
		return domain.ProjectImportResult{}, err
	}
	defer done()
	doc, warnings, err := parseProject(work, format, data)
	if err != nil {
		return domain.ProjectImportResult{}, err
	}
	doc, policyWarnings, err := project.Prepare(work, doc, project.Options{IncludeBodies: includeBodies})
	if err != nil {
		return domain.ProjectImportResult{}, err
	}
	doc, err = project.Reidentify(doc)
	if err != nil {
		return domain.ProjectImportResult{}, err
	}
	incoming := project.State(doc)
	if err = storage.ValidateProjectState(incoming); err != nil {
		return domain.ProjectImportResult{}, errors.New("imported project has invalid settings or record ownership")
	}
	// mutate loads the current revision under the service mutex; Save commits
	// the appended workspace and every descriptor artifact in one CAS transaction.
	// Existing preferences, tabs, environments and vault references are preserved.
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.repo.Load(work)
	if err != nil {
		return domain.ProjectImportResult{}, err
	}
	state.Workspaces = append(state.Workspaces, incoming.Workspaces...)
	state.Collections = append(state.Collections, incoming.Collections...)
	state.Folders = append(state.Folders, incoming.Folders...)
	state.Requests = append(state.Requests, incoming.Requests...)
	state.Environments = append(state.Environments, incoming.Environments...)
	state.ProtoSchemas = append(state.ProtoSchemas, incoming.ProtoSchemas...)
	state, err = s.repo.Save(work, state, state.Revision)
	if err != nil {
		return domain.ProjectImportResult{}, err
	}
	return domain.ProjectImportResult{State: state, WorkspaceID: doc.Workspace.ID, Warnings: projectWarnings(warnings, policyWarnings), Stats: project.Statistics(doc)}, nil
}
