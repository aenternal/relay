package app

import (
	"relay/internal/domain"
	"relay/internal/protobuf/registry"
)

// ProtoSource is a virtual import path and UTF-8 source, never a filesystem path.
type ProtoSource struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type ProtoImport struct {
	ID                  string        `json:"id"`
	WorkspaceID         string        `json:"workspaceId"`
	Name                string        `json:"name"`
	Kind                string        `json:"kind"`
	RootFiles           []string      `json:"rootFiles"`
	Sources             []ProtoSource `json:"sources"`
	DescriptorSetBase64 string        `json:"descriptorSetBase64"`
}

// ProtoDiagnostic deliberately contains no source excerpt or request value.
type ProtoDiagnostic struct {
	File    string `json:"file"`
	Path    string `json:"path"`
	Message string `json:"message"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
}

type ProtoImportResult struct {
	State       domain.State      `json:"state"`
	SchemaID    string            `json:"schemaId"`
	Diagnostics []ProtoDiagnostic `json:"diagnostics"`
	Warnings    []string          `json:"warnings"`
}

type ProtoSchemaDetails struct {
	Schema   domain.ProtoSchema     `json:"schema"`
	Files    []registry.FileInfo    `json:"files"`
	Services []registry.ServiceInfo `json:"services"`
	Messages []registry.MessageInfo `json:"messages"`
}

type ProtoMethodDetails struct {
	Method   registry.MethodInfo  `json:"method"`
	Input    registry.MessageInfo `json:"input"`
	Output   registry.MessageInfo `json:"output"`
	Template string               `json:"template"`
}

// Validation is an offline editor operation. Environment variables, especially
// keyring secrets, are not resolved into canonical JSON or wire previews.
type ProtoValidationRequest struct {
	WorkspaceID string `json:"workspaceId"`
	SchemaID    string `json:"schemaId"`
	Method      string `json:"method"`
	Body        string `json:"body"`
	Format      string `json:"format"`
}

type ProtoValidationResult struct {
	Valid         bool              `json:"valid"`
	CanonicalJSON string            `json:"canonicalJSON"`
	WireBase64    string            `json:"wireBase64"`
	WireBytes     int               `json:"wireBytes"`
	Diagnostics   []ProtoDiagnostic `json:"diagnostics"`
	Warnings      []string          `json:"warnings"`
}
