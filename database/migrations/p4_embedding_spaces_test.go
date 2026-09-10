package migrations

import (
	"os"
	"strings"
	"testing"
)

func TestP4EmbeddingSchemaMigration(t *testing.T) {
	migration, err := os.ReadFile("000036_embedding_spaces_and_code_embeddings.sql")
	if err != nil {
		t.Fatalf("read P4 migration: %v", err)
	}
	text := string(migration)
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS embedding_spaces",
		"role text NOT NULL",
		"provider text NOT NULL",
		"model_revision text NOT NULL",
		"generation integer NOT NULL",
		"ALTER TABLE memory_embeddings",
		"ADD COLUMN IF NOT EXISTS embedding_space_id",
		"CREATE TABLE IF NOT EXISTS code_embeddings",
		"symbol_version_id uuid NOT NULL",
		"embedding_space_id uuid NOT NULL",
		"ALTER TABLE code_embeddings ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE code_embeddings FORCE ROW LEVEL SECURITY",
		"CREATE POLICY code_embeddings_tenant_isolation",
		"selected_code_symbol_version_ids",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("P4 migration missing %q", want)
		}
	}
}
