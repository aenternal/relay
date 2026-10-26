package domain

// Project transfer exposes only a bounded report until the user applies import.
// Secret values and process-local file permissions are never transferred.
type ProjectWarning struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

type ProjectStats struct {
	Collections     int `json:"collections"`
	Folders         int `json:"folders"`
	Requests        int `json:"requests"`
	Environments    int `json:"environments"`
	Schemas         int `json:"schemas"`
	SecretVariables int `json:"secretVariables"`
}

type ProjectExport struct {
	Filename string           `json:"filename"`
	Data     string           `json:"data"`
	Warnings []ProjectWarning `json:"warnings"`
	Stats    ProjectStats     `json:"stats"`
}

type ProjectPreview struct {
	Format        string           `json:"format"`
	WorkspaceName string           `json:"workspaceName"`
	Warnings      []ProjectWarning `json:"warnings"`
	Stats         ProjectStats     `json:"stats"`
}

type ProjectImportResult struct {
	State       State            `json:"state"`
	WorkspaceID string           `json:"workspaceId"`
	Warnings    []ProjectWarning `json:"warnings"`
	Stats       ProjectStats     `json:"stats"`
}

type SelectedProjectFile struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

type ProjectFileExport struct {
	Saved    bool             `json:"saved"`
	Warnings []ProjectWarning `json:"warnings"`
	Stats    ProjectStats     `json:"stats"`
}
