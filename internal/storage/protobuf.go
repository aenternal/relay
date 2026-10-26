package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"strings"
	"time"

	"relay/internal/domain"
)

func validateProtoSchema(v domain.ProtoSchema, check func(string, int) bool) error {
	if !check(v.Name, 4096) || strings.TrimSpace(v.Name) == "" || (v.Kind != "proto" && v.Kind != "descriptor") || !check(v.ImportedAt, 128) {
		return invalid("invalid Protobuf schema metadata")
	}
	if _, err := time.Parse(time.RFC3339Nano, v.ImportedAt); err != nil {
		return invalid("invalid Protobuf import timestamp")
	}
	if !check(v.Checksum, 64) || len(v.Checksum) != 64 || !check(v.DescriptorSetBase64, base64.StdEncoding.EncodedLen(16<<20)) {
		return invalid("Protobuf descriptor set exceeds its limit")
	}
	artifact, err := base64.StdEncoding.DecodeString(v.DescriptorSetBase64)
	if err != nil || len(artifact) == 0 || len(artifact) > 16<<20 {
		return invalid("invalid Protobuf descriptor set encoding")
	}
	hash := sha256.Sum256(artifact)
	if hex.EncodeToString(hash[:]) != v.Checksum {
		return invalid("Protobuf descriptor checksum mismatch")
	}
	if len(v.Files) == 0 || len(v.Files) > 512 || len(v.RootFiles) == 0 || len(v.RootFiles) > 256 {
		return invalid("invalid Protobuf file count")
	}
	files := make(map[string]bool, len(v.Files))
	for _, name := range v.Files {
		if !check(name, 4096) || !fs.ValidPath(name) || strings.ContainsAny(name, "\\:\x00") || files[name] {
			return invalid("invalid or duplicate Protobuf filename")
		}
		files[name] = true
	}
	roots := make(map[string]bool, len(v.RootFiles))
	for _, name := range v.RootFiles {
		if !check(name, 4096) || !files[name] || roots[name] {
			return invalid("invalid Protobuf root file")
		}
		roots[name] = true
	}
	return nil
}

func readProtoSchemas(ctx context.Context, tx *sql.Tx, state *domain.State) error {
	rows, err := tx.QueryContext(ctx, "SELECT payload,descriptor_set_base64 FROM proto_schemas ORDER BY rowid")
	if err != nil {
		return databaseError(ctx, "read Protobuf schemas", err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload []byte
		var artifact string
		if err = rows.Scan(&payload, &artifact); err != nil {
			return databaseError(ctx, "read Protobuf schema", err)
		}
		var schema domain.ProtoSchema
		if json.Unmarshal(payload, &schema) != nil {
			return invalid("invalid stored Protobuf schema")
		}
		schema.DescriptorSetBase64 = artifact
		state.ProtoSchemas = append(state.ProtoSchemas, schema)
	}
	if err = rows.Err(); err != nil {
		return databaseError(ctx, "read Protobuf schemas", err)
	}
	return nil
}
