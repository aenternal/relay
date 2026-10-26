package domain

// ProtoSchema persists a self-contained descriptor set while exposing only
// bounded metadata through the desktop bridge. Storage encodes the artifact
// separately: ordinary JSON snapshots never send it back to the webview.
type ProtoSchema struct {
	ID                  string   `json:"id"`
	WorkspaceID         string   `json:"workspaceId"`
	Name                string   `json:"name"`
	Kind                string   `json:"kind"`
	Checksum            string   `json:"checksum"`
	ImportedAt          string   `json:"importedAt"`
	RootFiles           []string `json:"rootFiles"`
	Files               []string `json:"files"`
	DescriptorSetBase64 string   `json:"-"`
}
