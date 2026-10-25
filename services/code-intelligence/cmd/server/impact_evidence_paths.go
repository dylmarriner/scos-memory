package main

import "sort"

// codeImpactEvidencePaths reconstructs the shortest *reverse-CALLS* chain
// from any matched seed to every reachable caller in the already-bounded
// immutable snapshot subgraph. edges map caller -> callee (stored CALLS).
// Sorting seeds and each caller list makes equally short paths stable across
// PostgreSQL row order and Go map iteration.
func codeImpactEvidencePaths(seeds []string, edges map[string][]string) map[string][]string {
	paths := make(map[string][]string, len(seeds))
	reverse := make(map[string][]string)
	for caller, callees := range edges {
		for _, callee := range callees {
			reverse[callee] = append(reverse[callee], caller)
		}
	}
	for _, callers := range reverse {
		sort.Strings(callers)
	}
	queue := append([]string(nil), seeds...)
	sort.Strings(queue)
	unique := queue[:0]
	for _, seed := range queue {
		if _, exists := paths[seed]; exists {
			continue
		}
		paths[seed] = []string{"code:" + seed}
		unique = append(unique, seed)
	}
	queue = unique
	for pos := 0; pos < len(queue); pos++ {
		callee := queue[pos]
		for _, caller := range reverse[callee] {
			if _, exists := paths[caller]; exists {
				continue
			}
			path := make([]string, len(paths[callee]), len(paths[callee])+1)
			copy(path, paths[callee])
			paths[caller] = append(path, "code:"+caller)
			queue = append(queue, caller)
		}
	}
	return paths
}
