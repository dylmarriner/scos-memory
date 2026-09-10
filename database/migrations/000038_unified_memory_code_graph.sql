BEGIN;

-- P6: one tenant-scoped identity registry for durable graph traversal. The
-- source tables remain canonical; this registry gives heterogeneous rows a
-- stable graph identity without turning the graph into a second truth store.
CREATE TABLE IF NOT EXISTS unified_graph_nodes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    node_type text NOT NULL CHECK (node_type IN ('memory_fact','code_symbol','code_symbol_version','code_snapshot','evidence','impact')),
    node_key text NOT NULL,
    source_table text NOT NULL,
    source_id uuid NOT NULL,
    snapshot_id uuid,
    provenance jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, node_type, node_key),
    UNIQUE (tenant_id, node_type, source_id)
);
CREATE INDEX IF NOT EXISTS unified_graph_nodes_lookup_idx ON unified_graph_nodes (tenant_id, node_type, node_key);

CREATE TABLE IF NOT EXISTS unified_graph_edges (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    source_node_id uuid NOT NULL REFERENCES unified_graph_nodes(id) ON DELETE CASCADE,
    target_node_id uuid NOT NULL REFERENCES unified_graph_nodes(id) ON DELETE CASCADE,
    relation text NOT NULL,
    confidence double precision NOT NULL DEFAULT 1.0 CHECK (confidence >= 0 AND confidence <= 1),
    provenance jsonb NOT NULL DEFAULT '{}'::jsonb,
    valid_from timestamptz,
    valid_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (source_node_id <> target_node_id),
    UNIQUE (tenant_id, source_node_id, target_node_id, relation)
);
CREATE INDEX IF NOT EXISTS unified_graph_edges_source_idx ON unified_graph_edges (tenant_id, source_node_id);
CREATE INDEX IF NOT EXISTS unified_graph_edges_target_idx ON unified_graph_edges (tenant_id, target_node_id);

ALTER TABLE unified_graph_nodes ENABLE ROW LEVEL SECURITY;
ALTER TABLE unified_graph_nodes FORCE ROW LEVEL SECURITY;
ALTER TABLE unified_graph_edges ENABLE ROW LEVEL SECURITY;
ALTER TABLE unified_graph_edges FORCE ROW LEVEL SECURITY;
CREATE POLICY unified_graph_nodes_tenant_access ON unified_graph_nodes FOR ALL
    USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY unified_graph_edges_tenant_access ON unified_graph_edges FOR ALL
    USING (unified_graph_edges.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (unified_graph_edges.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND EXISTS (SELECT 1 FROM unified_graph_nodes n WHERE n.id=unified_graph_edges.source_node_id AND n.tenant_id=unified_graph_edges.tenant_id)
        AND EXISTS (SELECT 1 FROM unified_graph_nodes n WHERE n.id=unified_graph_edges.target_node_id AND n.tenant_id=unified_graph_edges.tenant_id));

-- Complete P4 code-table RLS. Child rows inherit access through their
-- tenant-owned repository, snapshot, or symbol version.
ALTER TABLE code_repositories ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_repository_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_files ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_file_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_symbols ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_symbol_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_edges ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_impact_reconciliations ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_repositories FORCE ROW LEVEL SECURITY;
ALTER TABLE code_repository_snapshots FORCE ROW LEVEL SECURITY;
ALTER TABLE code_files FORCE ROW LEVEL SECURITY;
ALTER TABLE code_file_versions FORCE ROW LEVEL SECURITY;
ALTER TABLE code_symbols FORCE ROW LEVEL SECURITY;
ALTER TABLE code_symbol_versions FORCE ROW LEVEL SECURITY;
ALTER TABLE code_chunks FORCE ROW LEVEL SECURITY;
ALTER TABLE code_edges FORCE ROW LEVEL SECURITY;
ALTER TABLE code_impact_reconciliations FORCE ROW LEVEL SECURITY;
CREATE POLICY code_repositories_tenant_access ON code_repositories FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY code_snapshots_tenant_access ON code_repository_snapshots FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY code_files_tenant_access ON code_files FOR ALL USING (EXISTS (SELECT 1 FROM code_repositories r WHERE r.id=repository_id)) WITH CHECK (EXISTS (SELECT 1 FROM code_repositories r WHERE r.id=repository_id));
CREATE POLICY code_file_versions_tenant_access ON code_file_versions FOR ALL USING (EXISTS (SELECT 1 FROM code_repository_snapshots s WHERE s.id=snapshot_id)) WITH CHECK (EXISTS (SELECT 1 FROM code_repository_snapshots s WHERE s.id=snapshot_id));
CREATE POLICY code_symbols_tenant_access ON code_symbols FOR ALL USING (EXISTS (SELECT 1 FROM code_repositories r WHERE r.id=repository_id)) WITH CHECK (EXISTS (SELECT 1 FROM code_repositories r WHERE r.id=repository_id));
CREATE POLICY code_symbol_versions_tenant_access ON code_symbol_versions FOR ALL USING (EXISTS (SELECT 1 FROM code_repository_snapshots s WHERE s.id=snapshot_id)) WITH CHECK (EXISTS (SELECT 1 FROM code_repository_snapshots s WHERE s.id=snapshot_id));
CREATE POLICY code_chunks_tenant_access ON code_chunks FOR ALL USING (EXISTS (SELECT 1 FROM code_symbol_versions v WHERE v.id=symbol_version_id)) WITH CHECK (EXISTS (SELECT 1 FROM code_symbol_versions v WHERE v.id=symbol_version_id));
CREATE POLICY code_edges_tenant_access ON code_edges FOR ALL USING (EXISTS (SELECT 1 FROM code_repository_snapshots s WHERE s.id=snapshot_id)) WITH CHECK (EXISTS (SELECT 1 FROM code_repository_snapshots s WHERE s.id=snapshot_id));
CREATE POLICY code_impact_tenant_access ON code_impact_reconciliations FOR ALL USING (EXISTS (SELECT 1 FROM code_repositories r WHERE r.id=repository_id)) WITH CHECK (EXISTS (SELECT 1 FROM code_repositories r WHERE r.id=repository_id));

-- Idempotent backfill makes prior P1/P4 rows traversable immediately.
INSERT INTO unified_graph_nodes (tenant_id,node_type,node_key,source_table,source_id,snapshot_id,provenance)
SELECT mi.tenant_id,'memory_fact',mi.id::text,'memory_items',mi.id,NULL,jsonb_build_object('source_table','memory_items','source_id',mi.id) FROM memory_items mi ON CONFLICT DO NOTHING;
INSERT INTO unified_graph_nodes (tenant_id,node_type,node_key,source_table,source_id,snapshot_id,provenance)
SELECT crs.tenant_id,'code_snapshot',crs.id::text,'code_repository_snapshots',crs.id,crs.id,jsonb_build_object('source_table','code_repository_snapshots','source_id',crs.id,'commit_id',crs.commit_id,'branch',crs.branch) FROM code_repository_snapshots crs ON CONFLICT DO NOTHING;
INSERT INTO unified_graph_nodes (tenant_id,node_type,node_key,source_table,source_id,snapshot_id,provenance)
SELECT s.tenant_id,'code_symbol_version',v.id::text,'code_symbol_versions',v.id,v.snapshot_id,jsonb_build_object('source_table','code_symbol_versions','source_id',v.id,'snapshot_id',v.snapshot_id,'path',v.path,'range',v.range) FROM code_symbol_versions v JOIN code_repository_snapshots s ON s.id=v.snapshot_id ON CONFLICT DO NOTHING;
INSERT INTO unified_graph_nodes (tenant_id,node_type,node_key,source_table,source_id,provenance)
SELECT r.tenant_id,'code_symbol',c.id::text,'code_symbols',c.id,jsonb_build_object('source_table','code_symbols','source_id',c.id,'stable_key',c.stable_key,'kind',c.kind) FROM code_symbols c JOIN code_repositories r ON r.id=c.repository_id ON CONFLICT DO NOTHING;
INSERT INTO unified_graph_nodes (tenant_id,node_type,node_key,source_table,source_id,provenance)
SELECT i.tenant_id,'evidence',e.id::text,'memory_evidence',e.id,jsonb_build_object('source_table','memory_evidence','source_id',e.id,'evidence_kind',e.evidence_kind) FROM memory_evidence e JOIN memory_items i ON i.id=e.memory_id ON CONFLICT DO NOTHING;
INSERT INTO unified_graph_nodes (tenant_id,node_type,node_key,source_table,source_id,snapshot_id,provenance)
SELECT rp.tenant_id,'impact',cir.id::text,'code_impact_reconciliations',cir.id,cir.snapshot_id,jsonb_build_object('source_table','code_impact_reconciliations','source_id',cir.id,'snapshot_id',cir.snapshot_id,'commit_id',cir.commit_id) FROM code_impact_reconciliations cir JOIN code_repositories rp ON rp.id=cir.repository_id ON CONFLICT DO NOTHING;
INSERT INTO unified_graph_edges (tenant_id,source_node_id,target_node_id,relation,confidence,provenance)
SELECT l.tenant_id,sf.id,ss.id,'memory_code_'||l.link_type,l.confidence,jsonb_build_object('source_table','memory_code_links','source_id',l.id,'source',l.source)
FROM memory_code_links l JOIN unified_graph_nodes sf ON sf.tenant_id=l.tenant_id AND sf.node_type='memory_fact' AND sf.source_id=l.fact_id JOIN unified_graph_nodes ss ON ss.tenant_id=l.tenant_id AND ss.node_type='code_symbol_version' AND ss.source_id=l.code_symbol_version_id ON CONFLICT DO NOTHING;
INSERT INTO unified_graph_edges (tenant_id,source_node_id,target_node_id,relation,confidence,provenance)
SELECT s.tenant_id,sn.id,tn.id,e.relation,e.confidence_score,jsonb_build_object('source_table','code_edges','source_id',e.id,'extractor',e.extractor,'source_location',e.source_location,'snapshot_id',e.snapshot_id)
FROM code_edges e JOIN code_repository_snapshots s ON s.id=e.snapshot_id JOIN unified_graph_nodes sn ON sn.tenant_id=s.tenant_id AND sn.source_id=e.source_symbol_version_id JOIN unified_graph_nodes tn ON tn.tenant_id=s.tenant_id AND tn.source_id=e.target_symbol_version_id ON CONFLICT DO NOTHING;
INSERT INTO unified_graph_edges (tenant_id,source_node_id,target_node_id,relation,confidence,provenance)
SELECT s.tenant_id,vn.id,sn.id,'version_of',1.0,jsonb_build_object('source_table','code_symbol_versions','source_id',v.id,'snapshot_id',v.snapshot_id)
FROM code_symbol_versions v JOIN code_repository_snapshots s ON s.id=v.snapshot_id
JOIN unified_graph_nodes vn ON vn.tenant_id=s.tenant_id AND vn.node_type='code_symbol_version' AND vn.source_id=v.id
JOIN unified_graph_nodes sn ON sn.tenant_id=s.tenant_id AND sn.node_type='code_snapshot' AND sn.source_id=v.snapshot_id ON CONFLICT DO NOTHING;
INSERT INTO unified_graph_edges (tenant_id,source_node_id,target_node_id,relation,provenance)
SELECT s.tenant_id,vn.id,cn.id,'symbol_version_of',jsonb_build_object('source_table','code_symbol_versions','source_id',v.id,'symbol_id',v.symbol_id)
FROM code_symbol_versions v JOIN code_repository_snapshots s ON s.id=v.snapshot_id
JOIN unified_graph_nodes vn ON vn.tenant_id=s.tenant_id AND vn.node_type='code_symbol_version' AND vn.source_id=v.id
JOIN unified_graph_nodes cn ON cn.tenant_id=s.tenant_id AND cn.node_type='code_symbol' AND cn.source_id=v.symbol_id ON CONFLICT DO NOTHING;
INSERT INTO unified_graph_edges (tenant_id,source_node_id,target_node_id,relation,provenance)
SELECT rp.tenant_id,an.id,sn.id,'impact_snapshot',jsonb_build_object('source_table','code_impact_reconciliations','source_id',r.id,'snapshot_id',r.snapshot_id)
FROM code_impact_reconciliations r JOIN code_repositories rp ON rp.id=r.repository_id
JOIN unified_graph_nodes an ON an.tenant_id=rp.tenant_id AND an.node_type='impact' AND an.source_id=r.id
JOIN unified_graph_nodes sn ON sn.tenant_id=rp.tenant_id AND sn.node_type='code_snapshot' AND sn.source_id=r.snapshot_id
WHERE r.snapshot_id IS NOT NULL ON CONFLICT DO NOTHING;
INSERT INTO unified_graph_edges (tenant_id,source_node_id,target_node_id,relation,provenance)
SELECT i.tenant_id,en.id,fn.id,'evidence_for',jsonb_build_object('source_table','memory_evidence','source_id',e.id,'evidence_kind',e.evidence_kind)
FROM memory_evidence e JOIN memory_items i ON i.id=e.memory_id
JOIN unified_graph_nodes en ON en.tenant_id=i.tenant_id AND en.node_type='evidence' AND en.source_id=e.id
JOIN unified_graph_nodes fn ON fn.tenant_id=i.tenant_id AND fn.node_type='memory_fact' AND fn.source_id=i.id
WHERE e.evidence_kind IN ('fact','fact_citation') ON CONFLICT DO NOTHING;
INSERT INTO unified_graph_edges (tenant_id,source_node_id,target_node_id,relation,confidence,provenance)
SELECT mi.tenant_id,an.id,bn.id,r.relation_type,r.confidence_score,jsonb_build_object('source_table','memory_relations','source_id',r.source_memory_id,'valid_from',r.valid_from)
FROM memory_relations r JOIN memory_items mi ON mi.id=r.source_memory_id JOIN memory_items mt ON mt.id=r.target_memory_id AND mt.tenant_id=mi.tenant_id
JOIN unified_graph_nodes an ON an.tenant_id=mi.tenant_id AND an.node_type='memory_fact' AND an.source_id=r.source_memory_id
JOIN unified_graph_nodes bn ON bn.tenant_id=mi.tenant_id AND bn.node_type='memory_fact' AND bn.source_id=r.target_memory_id ON CONFLICT DO NOTHING;

-- Keep graph projections current for new canonical rows. Triggers copy only
-- identity and provenance; canonical content stays in source tables.
CREATE OR REPLACE FUNCTION unified_graph_sync_node() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE nt text; sid uuid; snap uuid; tid uuid; prov jsonb;
BEGIN
  IF TG_TABLE_NAME='memory_items' THEN nt:='memory_fact'; sid:=NEW.id; tid:=NEW.tenant_id; prov:=jsonb_build_object('source_table',TG_TABLE_NAME,'source_id',NEW.id);
  ELSIF TG_TABLE_NAME='code_repository_snapshots' THEN nt:='code_snapshot'; sid:=NEW.id; snap:=NEW.id; tid:=NEW.tenant_id; prov:=jsonb_build_object('source_table',TG_TABLE_NAME,'source_id',NEW.id,'commit_id',NEW.commit_id,'branch',NEW.branch);
  ELSIF TG_TABLE_NAME='code_symbols' THEN nt:='code_symbol'; sid:=NEW.id; SELECT tenant_id INTO tid FROM code_repositories WHERE id=NEW.repository_id; prov:=jsonb_build_object('source_table',TG_TABLE_NAME,'source_id',NEW.id,'stable_key',NEW.stable_key,'kind',NEW.kind);
  ELSIF TG_TABLE_NAME='code_symbol_versions' THEN nt:='code_symbol_version'; sid:=NEW.id; snap:=NEW.snapshot_id; SELECT tenant_id INTO tid FROM code_repository_snapshots WHERE id=NEW.snapshot_id; prov:=jsonb_build_object('source_table',TG_TABLE_NAME,'source_id',NEW.id,'snapshot_id',NEW.snapshot_id,'path',NEW.path,'range',NEW.range);
  ELSIF TG_TABLE_NAME='memory_evidence' THEN nt:='evidence'; sid:=NEW.id; SELECT tenant_id INTO tid FROM memory_items WHERE id=NEW.memory_id; prov:=jsonb_build_object('source_table',TG_TABLE_NAME,'source_id',NEW.id,'evidence_kind',NEW.evidence_kind);
  ELSIF TG_TABLE_NAME='code_impact_reconciliations' THEN nt:='impact'; sid:=NEW.id; snap:=NEW.snapshot_id; SELECT tenant_id INTO tid FROM code_repositories WHERE id=NEW.repository_id; prov:=jsonb_build_object('source_table',TG_TABLE_NAME,'source_id',NEW.id,'snapshot_id',NEW.snapshot_id,'commit_id',NEW.commit_id);
  ELSE RETURN NEW; END IF;
  INSERT INTO unified_graph_nodes(tenant_id,node_type,node_key,source_table,source_id,snapshot_id,provenance) VALUES(tid,nt,sid::text,TG_TABLE_NAME,sid,snap,prov) ON CONFLICT DO NOTHING;
  RETURN NEW;
END; $$;
CREATE OR REPLACE FUNCTION unified_graph_sync_edge() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE tid uuid; a uuid; b uuid; rel text; conf double precision; prov jsonb;
BEGIN
  IF TG_TABLE_NAME='memory_code_links' THEN tid:=NEW.tenant_id; rel:='memory_code_'||NEW.link_type; conf:=NEW.confidence; SELECT id INTO a FROM unified_graph_nodes WHERE tenant_id=tid AND node_type='memory_fact' AND source_id=NEW.fact_id; SELECT id INTO b FROM unified_graph_nodes WHERE tenant_id=tid AND node_type='code_symbol_version' AND source_id=NEW.code_symbol_version_id; prov:=jsonb_build_object('source_table',TG_TABLE_NAME,'source_id',NEW.id,'source',NEW.source);
  ELSIF TG_TABLE_NAME='code_edges' THEN SELECT tenant_id INTO tid FROM code_repository_snapshots WHERE id=NEW.snapshot_id; rel:=NEW.relation; conf:=NEW.confidence_score; SELECT id INTO a FROM unified_graph_nodes WHERE tenant_id=tid AND source_id=NEW.source_symbol_version_id; SELECT id INTO b FROM unified_graph_nodes WHERE tenant_id=tid AND source_id=NEW.target_symbol_version_id; prov:=jsonb_build_object('source_table',TG_TABLE_NAME,'source_id',NEW.id,'extractor',NEW.extractor,'source_location',NEW.source_location,'snapshot_id',NEW.snapshot_id);
  ELSE RETURN NEW; END IF;
  IF a IS NOT NULL AND b IS NOT NULL AND a<>b THEN INSERT INTO unified_graph_edges(tenant_id,source_node_id,target_node_id,relation,confidence,provenance) VALUES(tid,a,b,rel,conf,prov) ON CONFLICT DO NOTHING; END IF;
  RETURN NEW;
END; $$;
CREATE TRIGGER unified_graph_memory_items_sync AFTER INSERT ON memory_items FOR EACH ROW EXECUTE FUNCTION unified_graph_sync_node();
CREATE TRIGGER unified_graph_snapshots_sync AFTER INSERT ON code_repository_snapshots FOR EACH ROW EXECUTE FUNCTION unified_graph_sync_node();
CREATE TRIGGER unified_graph_symbols_sync AFTER INSERT ON code_symbols FOR EACH ROW EXECUTE FUNCTION unified_graph_sync_node();
CREATE TRIGGER unified_graph_symbol_versions_sync AFTER INSERT ON code_symbol_versions FOR EACH ROW EXECUTE FUNCTION unified_graph_sync_node();
CREATE TRIGGER unified_graph_evidence_sync AFTER INSERT ON memory_evidence FOR EACH ROW EXECUTE FUNCTION unified_graph_sync_node();
CREATE TRIGGER unified_graph_impact_sync AFTER INSERT ON code_impact_reconciliations FOR EACH ROW EXECUTE FUNCTION unified_graph_sync_node();
CREATE TRIGGER unified_graph_memory_code_edge_sync AFTER INSERT ON memory_code_links FOR EACH ROW EXECUTE FUNCTION unified_graph_sync_edge();
CREATE TRIGGER unified_graph_code_edge_sync AFTER INSERT ON code_edges FOR EACH ROW EXECUTE FUNCTION unified_graph_sync_edge();

-- Results retain node and edge provenance, including complete traversal paths.
CREATE OR REPLACE FUNCTION traverse_unified_graph(p_tenant_id uuid,p_node_type text,p_node_key text,p_max_depth integer DEFAULT 2)
RETURNS TABLE (node_id uuid,node_type text,node_key text,depth integer,node_provenance jsonb,path_node_ids uuid[],path_edge_ids uuid[],edge_provenance jsonb[])
LANGUAGE sql STABLE AS $$
WITH RECURSIVE walk AS (
 SELECT n.id AS id,n.node_type AS node_type,n.node_key AS node_key,0 AS depth,n.provenance AS provenance,ARRAY[n.id]::uuid[] AS path_node_ids,ARRAY[]::uuid[] AS path_edge_ids,ARRAY[]::jsonb[] AS edge_provenance FROM unified_graph_nodes n WHERE n.tenant_id=p_tenant_id AND n.node_type=p_node_type AND n.node_key=p_node_key
 UNION ALL
 SELECT n.id,n.node_type,n.node_key,w.depth+1,n.provenance,w.path_node_ids||n.id,w.path_edge_ids||e.id,w.edge_provenance||e.provenance FROM walk w JOIN unified_graph_edges e ON e.tenant_id=p_tenant_id AND e.source_node_id=w.id JOIN unified_graph_nodes n ON n.id=e.target_node_id AND n.tenant_id=p_tenant_id WHERE w.depth<GREATEST(0,LEAST(p_max_depth,8)) AND NOT n.id=ANY(w.path_node_ids)
) SELECT id,node_type,node_key,depth,provenance,path_node_ids,path_edge_ids,edge_provenance FROM walk;
$$;

-- Reverse traversal is the impact direction: incoming callers and memory
-- links that point at the changed code symbol. Forward graph traversal remains
-- the public graph API and keeps its original semantics.
CREATE OR REPLACE FUNCTION traverse_unified_graph_reverse(p_tenant_id uuid,p_node_type text,p_node_key text,p_max_depth integer DEFAULT 3)
RETURNS TABLE (node_id uuid,node_type text,node_key text,depth integer,node_provenance jsonb,path_node_ids uuid[],path_edge_ids uuid[],edge_provenance jsonb[])
LANGUAGE sql STABLE AS $$
WITH RECURSIVE walk AS (
 SELECT n.id AS id,n.node_type AS node_type,n.node_key AS node_key,0 AS depth,n.provenance AS provenance,ARRAY[n.id]::uuid[] AS path_node_ids,ARRAY[]::uuid[] AS path_edge_ids,ARRAY[]::jsonb[] AS edge_provenance FROM unified_graph_nodes n WHERE n.tenant_id=p_tenant_id AND n.node_type=p_node_type AND n.node_key=p_node_key
 UNION ALL
 SELECT n.id,n.node_type,n.node_key,w.depth+1,n.provenance,w.path_node_ids||n.id,e.id||w.path_edge_ids,e.provenance||w.edge_provenance FROM walk w JOIN unified_graph_edges e ON e.tenant_id=p_tenant_id AND e.target_node_id=w.id JOIN unified_graph_nodes n ON n.id=e.source_node_id AND n.tenant_id=p_tenant_id WHERE w.depth<GREATEST(0,LEAST(p_max_depth,8)) AND NOT n.id=ANY(w.path_node_ids)
) SELECT id,node_type,node_key,depth,provenance,path_node_ids,path_edge_ids,edge_provenance FROM walk;
$$;
COMMIT;
