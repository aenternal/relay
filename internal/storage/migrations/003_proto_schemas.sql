CREATE TABLE proto_schemas (
    id TEXT PRIMARY KEY NOT NULL,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    payload TEXT NOT NULL,
    descriptor_set_base64 TEXT NOT NULL,
    UNIQUE(id, workspace_id)
);
