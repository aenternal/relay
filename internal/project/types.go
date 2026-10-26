// Package project implements portable, offline project transfers.
package project

import "relay/internal/domain"

const (
	Format      = "relay-project"
	Version     = 1
	MaxBytes    = 32 << 20
	MaxRecords  = 10000
	MaxRequests = 2000
	MaxWarnings = 200
)

type Warning = domain.ProjectWarning
type Stats = domain.ProjectStats

type Options struct{ IncludeBodies bool }

type Schema struct {
	Metadata            domain.ProtoSchema `json:"metadata"`
	DescriptorSetBase64 string             `json:"descriptorSetBase64"`
}

type Document struct {
	Format       string                     `json:"format"`
	Version      int                        `json:"version"`
	Workspace    domain.Workspace           `json:"workspace"`
	Collections  []domain.Collection        `json:"collections"`
	Folders      []domain.Folder            `json:"folders"`
	Requests     []domain.RequestDefinition `json:"requests"`
	Environments []domain.Environment       `json:"environments"`
	Schemas      []Schema                   `json:"schemas"`
}

func Statistics(doc Document) Stats {
	result := Stats{Collections: len(doc.Collections), Folders: len(doc.Folders), Requests: len(doc.Requests), Environments: len(doc.Environments), Schemas: len(doc.Schemas)}
	for _, env := range doc.Environments {
		for _, variable := range env.Variables {
			if variable.Secret {
				result.SecretVariables++
			}
		}
	}
	return result
}
