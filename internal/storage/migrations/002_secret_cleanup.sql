ALTER TABLE variables ADD COLUMN secret_ref TEXT NOT NULL DEFAULT '';
UPDATE variables SET secret_ref = COALESCE(json_extract(payload, '$.secretRef'), '');
CREATE INDEX variables_secret_ref ON variables(secret_ref) WHERE secret_ref <> '';
CREATE TABLE secret_cleanup (
    ref TEXT PRIMARY KEY NOT NULL CHECK(length(ref) > 0)
);
