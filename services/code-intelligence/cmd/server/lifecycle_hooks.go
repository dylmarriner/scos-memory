package main

// indexChangePayload is the on_index_change payload (GOVERNANCE.md §22.6),
// raised after a new index generation or snapshot is durably committed and
// is the one served for its repository and branch.
func indexChangePayload(repositoryID, repositoryPath, branch, commitID, snapshotID string, filesIndexed int) map[string]any {
	return map[string]any{
		"repository_id":   repositoryID,
		"repository_path": repositoryPath,
		"branch":          branch,
		"commit_id":       commitID,
		"snapshot_id":     snapshotID,
		"files_indexed":   filesIndexed,
	}
}
