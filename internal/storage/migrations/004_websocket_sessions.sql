-- Snapshot saves replace workspaces/requests inside a transaction. History has
-- logical ownership checked on insert and pruned after the snapshot replacement;
-- a cascading FK would erase sessions on every ordinary autosave.
CREATE TABLE websocket_sessions (
    id TEXT PRIMARY KEY NOT NULL,
    workspace_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    saved_at TEXT NOT NULL,
    bytes INTEGER NOT NULL CHECK(bytes >= 0 AND bytes <= 8388608),
    summary TEXT NOT NULL,
    payload TEXT NOT NULL
);
CREATE INDEX websocket_sessions_workspace ON websocket_sessions(workspace_id, saved_at DESC, id DESC);
