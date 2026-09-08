BEGIN;

-- P1-D (content-hash-gated incremental indexing): the call graph signal
-- (CalledNames) and resolution boundary (Scope, Go's real package name or
-- Tree-sitter's module name) only ever existed transiently in-memory at
-- parse time (indexedDecl), never persisted. That made a correct "skip
-- unchanged files, don't re-parse" implementation impossible: a changed
-- file's re-parsed declarations resolve calls against every OTHER file's
-- indexedDecl entries from the SAME run, so skipping a file's re-parse
-- would silently drop every CALLS edge into it from a changed file. These
-- two columns let a skipped file still contribute a real indexedDecl,
-- reconstructed from its unchanged parent-snapshot row, no re-parse.
-- JSON-encoded array (text, not a native Postgres array) to sidestep
-- database/sql's array-Scan ceremony -- matches the codebase's existing
-- convention for structured columns (e.g. memory_versions.structured_content).
ALTER TABLE code_symbol_versions ADD COLUMN IF NOT EXISTS called_names text;
ALTER TABLE code_symbol_versions ADD COLUMN IF NOT EXISTS scope text;

COMMIT;
