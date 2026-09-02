package main

import "sort"

// personalizedPageRank ranks the nodes referenced by edges (an adjacency
// list of source -> outgoing targets) using topic-sensitive PageRank: on
// each iteration a random walker follows an outgoing edge with probability
// damping, and otherwise teleports back to one of seeds (uniformly) rather
// than to any node in the graph. This is P0-A of
// SCOS_MEMORY_FABRIC_AUDIT_AND_MASTER_PLAN_2026-09-02.md §6.7/§24: Aider's
// repomap.py technique, applied to code_edges CALLS edges so a change's
// dependents are ranked by structural importance -- personalized toward the
// changed symbol itself, not generic whole-graph centrality -- rather than
// left in arbitrary BFS-visitation order.
//
// A node with no outgoing edges (a dangling node) redistributes its entire
// rank back to the restart set on the next iteration rather than vanishing
// from the walk, the standard PageRank dangling-mass fix; without it, rank
// leaks out of the graph and every score keeps shrinking across
// iterations instead of converging.
//
// restartFloor keeps a small uniform-teleportation component alongside the
// seed bias (restart(v) = (1-restartFloor)*seedShare(v) +
// restartFloor*1/N) rather than teleporting exclusively back to seeds.
// Without it, a seed whose reachable subgraph is a DAG (no cycle routes
// mass back through a non-seed node) converges with every non-seed node's
// rank decaying all the way to zero -- mathematically correct for a pure
// personalized walk, but useless for ranking ChangeImpact's dependents:
// e.g. seed=hub with hub -> heavyCaller -> indirect and hub -> lightCaller
// (both edges reversed-CALLS) leaves heavyCaller and indirect statistically
// indistinguishable from lightCaller at convergence, even though indirect
// depending on heavyCaller too is exactly the signal ranking exists to
// surface. A small non-seed floor keeps every node minimally "alive" so
// genuine differences in how connected a node is (not just how close to
// the seed) survive to the stationary distribution, while damping still
// keeps the ranking dominated by proximity/connectivity to seeds rather
// than degenerating into plain whole-graph PageRank.
//
// nodes may include ids that appear in edges only as a target, or not at
// all (an isolated node passed for completeness); every id in nodes ends
// up with an entry in the result, defaulting to its share of the restart
// mass if it is never reached by a walk.
func personalizedPageRank(nodes []string, edges map[string][]string, seeds []string, damping float64, iterations int) map[string]float64 {
	const restartFloor = 0.15

	rank := make(map[string]float64, len(nodes))
	if len(nodes) == 0 {
		return rank
	}
	if damping <= 0 || damping >= 1 {
		damping = 0.85
	}
	if iterations <= 0 {
		iterations = 30
	}

	uniformShare := 1.0 / float64(len(nodes))
	restart := make(map[string]float64, len(nodes))
	if len(seeds) > 0 {
		seedShare := 1.0 / float64(len(seeds))
		for _, n := range nodes {
			restart[n] = restartFloor * uniformShare
		}
		for _, s := range seeds {
			restart[s] += (1 - restartFloor) * seedShare
		}
	} else {
		// No seed set: fall back to uniform teleportation, i.e. ordinary
		// (non-personalized) PageRank over the same graph.
		for _, n := range nodes {
			restart[n] = uniformShare
		}
	}

	init := 1.0 / float64(len(nodes))
	for _, n := range nodes {
		rank[n] = init
	}

	outDegree := make(map[string]int, len(nodes))
	for _, n := range nodes {
		outDegree[n] = len(edges[n])
	}

	for iter := 0; iter < iterations; iter++ {
		next := make(map[string]float64, len(nodes))
		for n, mass := range restart {
			next[n] += (1 - damping) * mass
		}

		var danglingMass float64
		for _, n := range nodes {
			if outDegree[n] == 0 {
				danglingMass += rank[n]
			}
		}
		if danglingMass > 0 {
			for n, mass := range restart {
				next[n] += damping * danglingMass * mass
			}
		}

		for _, n := range nodes {
			if outDegree[n] == 0 {
				continue
			}
			share := damping * rank[n] / float64(outDegree[n])
			for _, target := range edges[n] {
				next[target] += share
			}
		}

		rank = next
	}

	return rank
}

// rankByCentrality sorts items in place, most central first, breaking ties
// by fullyQualifiedName so identical-score results are still deterministic
// across calls rather than depending on map/BFS iteration order. idOf and
// fqnOf let this stay generic over PGSymbolVersion (and any future caller)
// without importing the concrete type here.
func rankByCentrality[T any](items []T, ranks map[string]float64, idOf func(T) string, fqnOf func(T) string) {
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := ranks[idOf(items[i])], ranks[idOf(items[j])]
		if ri != rj {
			return ri > rj
		}
		return fqnOf(items[i]) < fqnOf(items[j])
	})
}
