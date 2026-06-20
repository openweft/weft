package weft

// gpu_topo.go holds the platform-neutral parser that turns
// `nvidia-smi topo -m` into NVLink-domain labels on the GPU inventory.
// Like gpu_mig.go it lives in an untagged file so the parse is unit-
// testable on every GOOS ; the Linux detector runs `nvidia-smi topo -m`
// and hands the reader to assignNVLinkDomains.
//
// Why this matters (docs/operations/gpu-sharing.md) : on a 2×NVL4 node
// the eight cards form two NVLink islands of four. NVLink P2P is full
// bandwidth WITHIN an island and falls back to PCIe ACROSS islands, so a
// tensor-parallel group of 2–4 cards must stay inside one island. The
// scheduler enforces that via GPU.NVLinkDomain ; this parser fills it.

import (
	"bufio"
	"io"
	"sort"
	"strconv"
	"strings"
)

// assignNVLinkDomains parses `nvidia-smi topo -m` and labels each card
// with the NVLink island it belongs to. The matrix looks like :
//
//	        GPU0    GPU1    GPU2    GPU3    GPU4   CPU Affinity  NUMA Affinity
//	GPU0     X      NV18    NV18    NV18    SYS    0-23          0
//	GPU1    NV18     X      NV18    NV18    SYS    0-23          0
//	...
//
// A cell of `NV<n>` between GPU i and GPU j means they share NVLink ; any
// PCIe-level path (`SYS` / `PHB` / `NODE` / `PIX` / `PXB` / `X` self) does
// not. Cards are grouped into connected components over the NVLink
// adjacency ; each component of size ≥ 2 becomes one domain labelled
// `nvl-<minIndex>` (the lowest GPU index in the group — stable across
// reboots given stable PCI enumeration). Singletons (a card with no
// NVLink peer) are left with an EMPTY NVLinkDomain : per the design that
// means "no NVLink / unknown", and the scheduler's same-domain affinity
// is then a no-op for that card rather than forbidding a PCIe-only
// multi-GPU placement.
//
// The "GPU N" row/column index maps onto base[N] — the same PCI-order
// assumption enrichWithSMI / enumerateMIGFromSMIL rely on. Returns a copy ;
// base is not mutated. A parse that finds no usable matrix returns base
// unchanged (every domain empty), so a topo-less host degrades cleanly.
func assignNVLinkDomains(base []GPU, r io.Reader) []GPU {
	out := make([]GPU, len(base))
	copy(out, base)

	adj := parseNVLinkAdjacency(r, len(out))
	if adj == nil {
		return out
	}
	comp := connectedComponents(adj, len(out))
	for i := range out {
		members := comp[i]
		if len(members) < 2 {
			continue // singleton → no NVLink island → leave domain empty
		}
		out[i].NVLinkDomain = "nvl-" + strconv.Itoa(members[0]) // members sorted; [0] is min index
	}
	return out
}

// parseNVLinkAdjacency reads the topo matrix into an n×n boolean
// adjacency (adj[i][j] = GPU i and j share NVLink). Returns nil when no
// GPU rows were found. Robust to the trailing "CPU Affinity" / "NUMA
// Affinity" columns : only the first n cells of each row (the GPU columns,
// which always precede the affinity columns) are inspected.
func parseNVLinkAdjacency(r io.Reader, n int) [][]bool {
	if n == 0 {
		return nil
	}
	adj := make([][]bool, n)
	for i := range adj {
		adj[i] = make([]bool, n)
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	sawRow := false
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		row, ok := parseTopoGPUIndex(fields[0])
		if !ok || row >= n {
			continue // header row, blank, or an out-of-range label
		}
		sawRow = true
		// fields[1:] are the cells, GPU columns first. Inspect up to n.
		for col := 0; col < n && col+1 < len(fields); col++ {
			if col == row {
				continue // diagonal "X"
			}
			if strings.HasPrefix(fields[col+1], "NV") {
				adj[row][col] = true
				adj[col][row] = true // force symmetry even if the matrix is half-filled
			}
		}
	}
	if !sawRow {
		return nil
	}
	return adj
}

// parseTopoGPUIndex returns the N from a row/column label "GPUN".
func parseTopoGPUIndex(s string) (int, bool) {
	if !strings.HasPrefix(s, "GPU") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, "GPU"))
	if err != nil {
		return 0, false
	}
	return n, true
}

// connectedComponents returns, for each node, the sorted member list of
// the component it belongs to (so comp[i][0] is the component's min
// index). Plain BFS over the adjacency — n is ≤ a handful of GPUs per
// host, so an O(n²) flood is irrelevant.
func connectedComponents(adj [][]bool, n int) [][]int {
	compOf := make([]int, n)
	for i := range compOf {
		compOf[i] = -1
	}
	var comps [][]int
	for i := 0; i < n; i++ {
		if compOf[i] != -1 {
			continue
		}
		// BFS from i.
		var members []int
		queue := []int{i}
		compOf[i] = len(comps)
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			members = append(members, cur)
			for j := 0; j < n; j++ {
				if adj[cur][j] && compOf[j] == -1 {
					compOf[j] = len(comps)
					queue = append(queue, j)
				}
			}
		}
		sort.Ints(members)
		comps = append(comps, members)
	}
	// Map each node to its component's member list.
	out := make([][]int, n)
	for i := 0; i < n; i++ {
		out[i] = comps[compOf[i]]
	}
	return out
}
