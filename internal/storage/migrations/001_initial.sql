CREATE TABLE state_metadata (
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
    revision INTEGER NOT NULL CHECK(revision >= 0)
);
INSERT INTO state_metadata(singleton, revision) VALUES (1, 0);

CREATE TABLE workspaces (
    id TEXT PRIMARY KEY NOT NULL,
    active_environment_id TEXT,
    payload TEXT NOT NULL,
    FOREIGN KEY(active_environment_id, id) REFERENCES environments(id, workspace_id)
        DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE collections (
    id TEXT PRIMARY KEY NOT NULL,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    payload TEXT NOT NULL,
    UNIQUE(id, workspace_id)
);
CREATE TABLE folders (
    id TEXT PRIMARY KEY NOT NULL,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    collection_id TEXT NOT NULL,
    parent_id TEXT,
    payload TEXT NOT NULL,
    UNIQUE(id, workspace_id, collection_id),
    FOREIGN KEY(collection_id, workspace_id) REFERENCES collections(id, workspace_id) ON DELETE CASCADE,
    FOREIGN KEY(parent_id, workspace_id, collection_id) REFERENCES folders(id, workspace_id, collection_id)
        ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE requests (
    id TEXT PRIMARY KEY NOT NULL,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    collection_id TEXT NOT NULL,
    folder_id TEXT,
    payload TEXT NOT NULL,
    UNIQUE(id, workspace_id),
    FOREIGN KEY(collection_id, workspace_id) REFERENCES collections(id, workspace_id) ON DELETE CASCADE,
    FOREIGN KEY(folder_id, workspace_id, collection_id) REFERENCES folders(id, workspace_id, collection_id)
        ON DELETE CASCADE
);
CREATE TABLE environments (
    id TEXT PRIMARY KEY NOT NULL,
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    payload TEXT NOT NULL,
    UNIQUE(id, workspace_id)
);
CREATE TABLE variables (
    id TEXT PRIMARY KEY NOT NULL,
    environment_id TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    payload TEXT NOT NULL,
    UNIQUE(environment_id, name)
);
CREATE TABLE settings (
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
    payload TEXT NOT NULL
);
CREATE TABLE ui_state (
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
    workspace_id TEXT REFERENCES workspaces(id) ON DELETE CASCADE,
    active_request_id TEXT,
    payload TEXT NOT NULL,
    FOREIGN KEY(active_request_id, workspace_id) REFERENCES requests(id, workspace_id) ON DELETE CASCADE
);
CREATE TABLE ui_open_requests (
    position INTEGER PRIMARY KEY CHECK(position >= 0),
    request_id TEXT NOT NULL UNIQUE,
    workspace_id TEXT NOT NULL,
    FOREIGN KEY(request_id, workspace_id) REFERENCES requests(id, workspace_id) ON DELETE CASCADE
);
