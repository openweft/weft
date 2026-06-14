package cluster

// hostmesh.go carries the cluster-planner glue for the host-level WireGuard
// mesh (cluster prereq #3). Kept in its own file so the host-mesh feature
// stays additive to cluster.go's core schema.

// overlaySubnet returns the cluster's overlay CIDR, or the empty string when
// no overlay block is set. Validate() already guarantees a non-empty subnet
// for a valid cluster, so renderAction's MeshSync case always has one; the
// nil-guard keeps the helper safe for callers that hold a partially-built
// Cluster (tests).
func (c *Cluster) overlaySubnet() string {
	if c.Overlay == nil {
		return ""
	}
	return c.Overlay.Subnet
}
