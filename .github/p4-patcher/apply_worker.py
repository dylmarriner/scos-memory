from pathlib import Path
import re

worker_path = Path("workers/embeddings/src/scos_embeddings/worker.py")
text = worker_path.read_text()


def replace_once(pattern: str, replacement: str, label: str, flags=re.S) -> None:
    global text
    text, count = re.subn(pattern, replacement, text, count=1, flags=flags)
    if count != 1:
        raise SystemExit(f"{label}: expected one replacement, got {count}")


text = text.replace(
    "The active vector space is explicit: model + revision + dimensions. Existing\n"
    "vectors from another space do not count as current. Because the current\n"
    "retrieval query predates per-space SQL filtering, this worker also removes\n"
    "stale memory-vector spaces before each backfill cycle so pgvector can never be\n"
    "asked to compare 384-d and 1024-d memory vectors during rollout. The existing\n"
    "local cross-encoder rerank endpoint is retained as a compatibility path.\n",
    "The active vector space is explicit: role + provider + model + revision +\n"
    "dimensions + generation. Historical spaces are retained; retrieval selects one\n"
    "exact space before any pgvector distance operation. The existing local\n"
    "cross-encoder rerank endpoint is retained as a compatibility path.\n",
    1,
)

anchor = 'CACHE_DIR = os.environ.get("SCOS_EMBED_CACHE", "/var/lib/scos/embeddings")\n'
if anchor not in text:
    raise SystemExit("MODEL_GENERATION anchor missing")
text = text.replace(
    anchor,
    'MODEL_GENERATION = int(os.environ.get("SCOS_EMBED_GENERATION", "1"))\n' + anchor,
    1,
)

replace_once(
    r"def _purge_stale_spaces\(.*?\n\ndef _content_hash_for",
    '''def _purge_stale_spaces(*_args, **_kwargs) -> int:\n    """Compatibility guard: explicit spaces make global purges unnecessary."""\n    return 0\n\n\ndef _embedding_provider() -> str:\n    return "scos-model-runtime" if MODEL_RUNTIME_URL else "fastembed"\n\n\ndef _ensure_embedding_space(\n    conn: psycopg.Connection,\n    provider: str,\n    model: str,\n    revision: str,\n    dimensions: int,\n    generation: int,\n) -> str:\n    """Resolve the immutable text embedding-space identity used by this cycle."""\n    if not provider or not model or not revision or dimensions <= 0 or generation <= 0:\n        raise ValueError("embedding space identity is incomplete")\n    with conn.cursor() as cur:\n        cur.execute(\n            """\n            INSERT INTO embedding_spaces\n                (role, provider, model, model_revision, dimensions, generation)\n            VALUES (%s, %s, %s, %s, %s, %s)\n            ON CONFLICT (role, provider, model, model_revision, dimensions, generation)\n            DO UPDATE SET provider = EXCLUDED.provider\n            RETURNING id::text\n            """,\n            ("text", provider, model, revision, dimensions, generation),\n        )\n        row = cur.fetchone()\n    if not row or not row[0]:\n        raise RuntimeError("embedding space could not be resolved")\n    return str(row[0])\n\n\ndef _unembedded_batch(\n    conn: psycopg.Connection,\n    limit: int,\n    space_id: str,\n) -> list[dict[str, Any]]:\n    """Return memory versions missing the exact active embedding space."""\n    with conn.cursor(row_factory=psycopg.rows.dict_row) as cur:\n        cur.execute(\n            """\n            SELECT mv.id::text AS id, mv.content\n            FROM memory_versions mv\n            WHERE NOT EXISTS (\n                SELECT 1 FROM memory_embeddings me\n                WHERE me.memory_version_id = mv.id\n                  AND me.embedding_space_id = %s::uuid\n                  AND me.chunk_number = 0\n                  AND me.embedding IS NOT NULL\n            )\n            ORDER BY mv.created_at ASC\n            LIMIT %s\n            """,\n            (space_id, limit),\n        )\n        return cur.fetchall()\n\n\ndef _content_hash_for''',
    "replace legacy global-space handling",
)

replace_once(
    r"def _replace_embedding\(.*?\n\ndef run_cycle",
    '''def _replace_embedding(\n    conn: psycopg.Connection,\n    version_id: str,\n    content: str,\n    embedding: list[float],\n    space_id: str,\n    provider: str,\n    model: str,\n    revision: str,\n) -> None:\n    """Upsert one projection without deleting any historical embedding spaces."""\n    dimensions = len(embedding)\n    with conn.cursor() as cur:\n        cur.execute(\n            """\n            INSERT INTO memory_embeddings\n                (memory_version_id, embedding_space_id, provider, model, model_revision,\n                 dimensions, chunk_number, chunk_content_hash, embedding)\n            VALUES (%s, %s::uuid, %s, %s, %s, %s, 0, %s, %s::vector)\n            ON CONFLICT (memory_version_id, embedding_space_id, chunk_number)\n            DO UPDATE SET embedding = EXCLUDED.embedding,\n                          provider = EXCLUDED.provider,\n                          model = EXCLUDED.model,\n                          model_revision = EXCLUDED.model_revision,\n                          dimensions = EXCLUDED.dimensions,\n                          chunk_content_hash = EXCLUDED.chunk_content_hash,\n                          created_at = now()\n            """,\n            (\n                version_id,\n                space_id,\n                provider,\n                model,\n                revision,\n                dimensions,\n                _content_hash_for(content),\n                _vector_literal(embedding),\n            ),\n        )\n\n\ndef run_cycle''',
    "replace embedding upsert",
)

replace_once(
    r"def run_cycle\(conn: psycopg\.Connection\) -> int:.*?\n\ndef _serve_http",
    '''def run_cycle(conn: psycopg.Connection) -> int:\n    """Embed one batch of versions missing the active explicit text space."""\n    provider = _embedding_provider()\n    space_id = _ensure_embedding_space(\n        conn,\n        provider=provider,\n        model=MODEL_NAME,\n        revision=MODEL_REV,\n        dimensions=MODEL_DIMENSIONS,\n        generation=MODEL_GENERATION,\n    )\n    batch = _unembedded_batch(conn, BATCH_SIZE, space_id)\n    if not batch:\n        conn.commit()\n        return 0\n\n    embeddings = embed_texts([row["content"] for row in batch])\n    if embeddings is None:\n        conn.rollback()\n        return 0\n\n    written = 0\n    for row, vec in zip(batch, embeddings):\n        if len(vec) != MODEL_DIMENSIONS:\n            continue\n        _replace_embedding(\n            conn,\n            version_id=row["id"],\n            content=row["content"],\n            embedding=vec,\n            space_id=space_id,\n            provider=provider,\n            model=MODEL_NAME,\n            revision=MODEL_REV,\n        )\n        written += 1\n    conn.commit()\n    return written\n\n\ndef _serve_http''',
    "replace run cycle",
)

old_validation = '''    if MODEL_DIMENSIONS <= 0:\n        raise SystemExit("SCOS_EMBED_DIMENSIONS must be positive")\n'''
new_validation = '''    if MODEL_DIMENSIONS <= 0:\n        raise SystemExit("SCOS_EMBED_DIMENSIONS must be positive")\n    if MODEL_GENERATION <= 0:\n        raise SystemExit("SCOS_EMBED_GENERATION must be positive")\n'''
if old_validation not in text:
    raise SystemExit("worker validation anchor missing")
text = text.replace(old_validation, new_validation, 1)

worker_path.write_text(text)

migration_path = Path("database/migrations/000036_embedding_spaces_and_code_embeddings.sql")
migration = migration_path.read_text()
anchor = '''ALTER TABLE memory_embeddings\n    ALTER COLUMN embedding_space_id SET NOT NULL;\n\n'''
if anchor not in migration:
    raise SystemExit("migration uniqueness anchor missing")
replacement = anchor + '''-- The legacy uniqueness key ignored embedding generation. Drop whichever\n-- generated constraint name PostgreSQL assigned to that exact four-column key,\n-- then make the explicit space ID part of persisted projection identity.\nDO $$\nDECLARE\n    legacy_constraint text;\nBEGIN\n    SELECT con.conname\n      INTO legacy_constraint\n      FROM pg_constraint con\n      JOIN pg_class rel ON rel.oid = con.conrelid\n     WHERE rel.relname = 'memory_embeddings'\n       AND con.contype = 'u'\n       AND pg_get_constraintdef(con.oid) ILIKE '%memory_version_id%model%model_revision%chunk_number%'\n     LIMIT 1;\n    IF legacy_constraint IS NOT NULL THEN\n        EXECUTE format('ALTER TABLE memory_embeddings DROP CONSTRAINT %I', legacy_constraint);\n    END IF;\nEND $$;\n\nCREATE UNIQUE INDEX IF NOT EXISTS memory_embeddings_version_space_chunk_uidx\n    ON memory_embeddings (memory_version_id, embedding_space_id, chunk_number);\n\n'''
migration = migration.replace(anchor, replacement, 1)
migration_path.write_text(migration)
