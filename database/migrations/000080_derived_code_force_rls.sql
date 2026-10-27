-- P6: prevent direct cross-tenant reads/writes of derived source and graph
-- evidence. All access inherits the verified transaction-local tenant via
-- the FORCE-RLS provenance tables installed by migration 000079.
--
-- The relationships are deliberately checked as well: a cross-repository
-- UUID does not authorize a symbol/file/edge simply because its parent exists.
-- No unrestricted SELECT/USING(true) policy and no security-definer bypass.
BEGIN;

ALTER TABLE code_files ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_file_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_edges ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_impact_reconciliations ENABLE ROW LEVEL SECURITY;

CREATE POLICY p6_code_file_tenant ON code_files FOR ALL
USING (
    EXISTS (
        SELECT 1 FROM code_repositories repo
        JOIN code_repository_snapshots snap
          ON snap.id = code_files.first_snapshot_id
         AND snap.repository_id = repo.id
        WHERE repo.id = code_files.repository_id
    )
)
WITH CHECK (
    EXISTS (
        SELECT 1 FROM code_repositories repo
        JOIN code_repository_snapshots snap
          ON snap.id = code_files.first_snapshot_id
         AND snap.repository_id = repo.id
        WHERE repo.id = code_files.repository_id
    )
);

CREATE POLICY p6_code_file_version_tenant ON code_file_versions FOR ALL
USING (
    EXISTS (
        SELECT 1 FROM code_files f
        JOIN code_repository_snapshots snap
          ON snap.repository_id = f.repository_id
        WHERE f.id = code_file_versions.file_id
          AND snap.id = code_file_versions.snapshot_id
    )
)
WITH CHECK (
    EXISTS (
        SELECT 1 FROM code_files f
        JOIN code_repository_snapshots snap
          ON snap.repository_id = f.repository_id
        WHERE f.id = code_file_versions.file_id
          AND snap.id = code_file_versions.snapshot_id
    )
);

-- Source chunks are sensitive data, not merely nonsecret index metadata.
CREATE POLICY p6_code_chunk_tenant ON code_chunks FOR ALL
USING (
    EXISTS (
        SELECT 1 FROM code_symbol_versions sv
        WHERE sv.id = code_chunks.symbol_version_id
    )
)
WITH CHECK (
    EXISTS (
        SELECT 1 FROM code_symbol_versions sv
        WHERE sv.id = code_chunks.symbol_version_id
    )
);

CREATE POLICY p6_code_edge_tenant ON code_edges FOR ALL
USING (
    EXISTS (
        SELECT 1 FROM code_repository_snapshots snap
        JOIN code_symbol_versions src
          ON src.snapshot_id = snap.id
        JOIN code_symbol_versions dst
          ON dst.snapshot_id = snap.id
        WHERE snap.id = code_edges.snapshot_id
          AND src.id = code_edges.source_symbol_version_id
          AND dst.id = code_edges.target_symbol_version_id
    )
)
WITH CHECK (
    EXISTS (
        SELECT 1 FROM code_repository_snapshots snap
        JOIN code_symbol_versions src
          ON src.snapshot_id = snap.id
        JOIN code_symbol_versions dst
          ON dst.snapshot_id = snap.id
        WHERE snap.id = code_edges.snapshot_id
          AND src.id = code_edges.source_symbol_version_id
          AND dst.id = code_edges.target_symbol_version_id
    )
);

CREATE POLICY p6_code_impact_reconciliation_tenant ON code_impact_reconciliations FOR ALL
USING (
    EXISTS (
        SELECT 1 FROM code_repositories repo
        WHERE repo.id = code_impact_reconciliations.repository_id
          AND (
            code_impact_reconciliations.snapshot_id IS NULL OR EXISTS (
                SELECT 1 FROM code_repository_snapshots snap
                WHERE snap.id = code_impact_reconciliations.snapshot_id
                  AND snap.repository_id = repo.id
            )
          )
    )
)
WITH CHECK (
    EXISTS (
        SELECT 1 FROM code_repositories repo
        WHERE repo.id = code_impact_reconciliations.repository_id
          AND (
            code_impact_reconciliations.snapshot_id IS NULL OR EXISTS (
                SELECT 1 FROM code_repository_snapshots snap
                WHERE snap.id = code_impact_reconciliations.snapshot_id
                  AND snap.repository_id = repo.id
            )
          )
    )
);

ALTER TABLE code_files FORCE ROW LEVEL SECURITY;
ALTER TABLE code_file_versions FORCE ROW LEVEL SECURITY;
ALTER TABLE code_chunks FORCE ROW LEVEL SECURITY;
ALTER TABLE code_edges FORCE ROW LEVEL SECURITY;
ALTER TABLE code_impact_reconciliations FORCE ROW LEVEL SECURITY;

COMMIT;
