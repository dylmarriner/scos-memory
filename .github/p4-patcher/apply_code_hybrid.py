from pathlib import Path

pgstore = Path("services/code-intelligence/cmd/server/pgstore.go")
pg = pgstore.read_text()
old_struct = '''\tMatchType string\n\tRelevance float64\n\t// Centrality is this symbol's personalized-PageRank score within a\n'''
new_struct = '''\tMatchType string\n\tRelevance float64\n\t// P4 dense-retrieval provenance. Relevance remains the raw lexical\n\t// ts_rank score; DenseScore is raw cosine similarity; RRFScore is the\n\t// cross-lane reciprocal-rank contribution. None overwrites another.\n\tDenseScore   float64\n\tRRFScore     float64\n\tKind         string\n\tSignature    string\n\tDocComment   string\n\tChunkContent string\n\t// Centrality is this symbol's personalized-PageRank score within a\n'''
if old_struct not in pg:
    raise SystemExit("PGSymbolVersion score anchor missing")
pg = pg.replace(old_struct, new_struct, 1)
pgstore.write_text(pg)

main_path = Path("services/code-intelligence/cmd/server/main.go")
main = main_path.read_text()
old_server = '''\tpgStore *pgCodeStore\n}\n\nfunc main() {\n'''
new_server = '''\tpgStore     *pgCodeStore\n\tcodeRuntime *codeModelRuntimeClient\n\tcodeSpace   codeEmbeddingSpace\n}\n\nfunc main() {\n'''
if old_server not in main:
    raise SystemExit("server struct anchor missing")
main = main.replace(old_server, new_server, 1)
old_init = '''\tdatabase.TuneSQLDB(db)\n\tsrv.pgStore = newPGCodeStore(db)\n\n\tmux := http.NewServeMux()\n'''
new_init = '''\tdatabase.TuneSQLDB(db)\n\tsrv.pgStore = newPGCodeStore(db)\n\n\tcodeRuntime, err := newCodeModelRuntimeClientFromEnvironment()\n\tif err != nil {\n\t\tlog.Fatalf("configure BGE-Code runtime: %v", err)\n\t}\n\tcodeSpace, err := activeCodeEmbeddingSpaceFromEnvironment()\n\tif err != nil {\n\t\tlog.Fatalf("configure BGE-Code space: %v", err)\n\t}\n\tsrv.codeRuntime = codeRuntime\n\tsrv.codeSpace = codeSpace\n\n\tmux := http.NewServeMux()\n'''
if old_init not in main:
    raise SystemExit("runtime startup anchor missing")
main = main.replace(old_init, new_init, 1)
main_path.write_text(main)

handlers_path = Path("services/code-intelligence/cmd/server/pghandlers.go")
handlers = handlers_path.read_text()
old_index = '''\tif err := s.pgStore.IndexSnapshot(r.Context(), snapshotID, req.RepositoryID, files); err != nil {\n\t\thttp.Error(w, "index snapshot: "+err.Error(), http.StatusInternalServerError)\n\t\treturn\n\t}\n\t// The diff against whatever was previously active for this branch is\n'''
new_index = '''\tif err := s.pgStore.IndexSnapshot(r.Context(), snapshotID, req.RepositoryID, files); err != nil {\n\t\thttp.Error(w, "index snapshot: "+err.Error(), http.StatusInternalServerError)\n\t\treturn\n\t}\n\tdegradedModels := []string{}\n\tif s.codeRuntime == nil {\n\t\tdegradedModels = append(degradedModels, "bge_code")\n\t} else if err := s.embedCodeSnapshot(r.Context(), tenant, snapshotID); err != nil {\n\t\tdegradedModels = append(degradedModels, "bge_code")\n\t}\n\t// The diff against whatever was previously active for this branch is\n'''
if old_index not in handlers:
    raise SystemExit("PG index anchor missing")
handlers = handlers.replace(old_index, new_index, 1)
old_index_resp = '''\twriteJSON(w, map[string]any{\n\t\t"snapshot_id": snapshotID,\n\t\t"file_count":  len(files),\n\t\t"diff":        diff,\n\t})\n'''
new_index_resp = '''\twriteJSON(w, map[string]any{\n\t\t"snapshot_id":      snapshotID,\n\t\t"file_count":       len(files),\n\t\t"diff":             diff,\n\t\t"degraded_models": degradedModels,\n\t})\n'''
if old_index_resp not in handlers:
    raise SystemExit("PG index response anchor missing")
handlers = handlers.replace(old_index_resp, new_index_resp, 1)
old_query = '''\tresults, err := s.pgStore.QueryByPathOrFQN(r.Context(), req.RepositoryID, req.Branch, req.Query)\n\tif err != nil {\n\t\thttp.Error(w, "query: "+err.Error(), http.StatusInternalServerError)\n\t\treturn\n\t}\n\t// Lexical fallback only, never a blend: an exact path/FQN match is\n\t// unambiguous, so a caller who typed the identifier they meant should\n\t// never see it buried under ranked full-text guesses.\n\tif len(results) == 0 {\n\t\tresults, err = s.pgStore.QueryLexical(r.Context(), req.RepositoryID, req.Branch, req.Query, 20)\n\t\tif err != nil {\n\t\t\thttp.Error(w, "lexical query: "+err.Error(), http.StatusInternalServerError)\n\t\t\treturn\n\t\t}\n\t}\n\tif results == nil {\n\t\tresults = []PGSymbolVersion{}\n\t}\n\twriteJSON(w, map[string]any{"results": results})\n'''
new_query = '''\tresults, err := s.pgStore.QueryByPathOrFQN(r.Context(), req.RepositoryID, req.Branch, req.Query)\n\tif err != nil {\n\t\thttp.Error(w, "query: "+err.Error(), http.StatusInternalServerError)\n\t\treturn\n\t}\n\tdegradedModels := []string{}\n\tlexicalCount := 0\n\tdenseCount := 0\n\t// Exact path/FQN remains the precision anchor. Only non-exact requests\n\t// enter hybrid lexical+dense retrieval, so a learned model can never\n\t// bury an identifier the caller named exactly.\n\tif len(results) == 0 {\n\t\tlexical, lexicalErr := s.pgStore.QueryLexical(r.Context(), req.RepositoryID, req.Branch, req.Query, 40)\n\t\tif lexicalErr != nil {\n\t\t\thttp.Error(w, "lexical query: "+lexicalErr.Error(), http.StatusInternalServerError)\n\t\t\treturn\n\t\t}\n\t\tlexicalCount = len(lexical)\n\t\tvar dense []PGSymbolVersion\n\t\tif s.codeRuntime == nil {\n\t\t\tdegradedModels = append(degradedModels, "bge_code")\n\t\t} else {\n\t\t\tdense, err = s.queryDenseCode(r.Context(), tenant, req.RepositoryID, req.Branch, req.Query, 40)\n\t\t\tif err != nil {\n\t\t\t\tdegradedModels = append(degradedModels, "bge_code")\n\t\t\t\tdense = nil\n\t\t\t}\n\t\t}\n\t\tdenseCount = len(dense)\n\t\tresults = fuseCodeResults(lexical, dense, 60)\n\t\tif len(results) > 20 {\n\t\t\tresults = results[:20]\n\t\t}\n\t}\n\tif results == nil {\n\t\tresults = []PGSymbolVersion{}\n\t}\n\twriteJSON(w, map[string]any{\n\t\t"results":            results,\n\t\t"degraded_models":    degradedModels,\n\t\t"lexical_candidates": lexicalCount,\n\t\t"dense_candidates":   denseCount,\n\t})\n'''
if old_query not in handlers:
    raise SystemExit("PG query anchor missing")
handlers = handlers.replace(old_query, new_query, 1)
handlers_path.write_text(handlers)
