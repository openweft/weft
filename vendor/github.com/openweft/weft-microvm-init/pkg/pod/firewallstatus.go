package pod

// FirewallStatus is the per-VM live state weft-microvm-agent
// publishes on "weft.firewall.<vm-uuid>.status" so the control
// plane (and the UI) can show whether the firewall reconciler is
// healthy and what's installed right now.
//
// Reverse direction of the firewall config subject : weft-network
// (or weft) PUBLISHES the desired ruleset on the per-VM
// "weft.firewall.<vm-uuid>" subject ; the agent PUBLISHES this
// status back on a sibling subject. Same Subscriber+ApplyFunc /
// emitter pair that weft-router uses for its BGP state.
type FirewallStatus struct {
	// Overall is "Healthy" when the reconciler successfully read
	// the kernel table on the last poll, "Degraded" when the
	// nftables read errored. Defined as a string so the UI can
	// render unknown future states without code churn.
	Overall string `json:"Overall"`
	// TableInstalled is true when the "weft-fw" table is present
	// in the kernel ; false when no ruleset has been applied yet
	// (fresh boot before the first publish lands) or it was
	// flushed externally.
	TableInstalled bool `json:"TableInstalled"`
	// RulesInstalled is the total number of nftables rules across
	// the input + output chains, including the unconditional
	// ct/lo accepts the reconciler always installs. The UI
	// subtracts those defaults to show the tenant-visible count.
	RulesInstalled int `json:"RulesInstalled"`
	// LastError carries the most recent read error message ;
	// empty on success. Surfaced so an operator can tell
	// "reconciler crashed" from "no policy yet" without ssh-ing
	// into the guest.
	LastError string `json:"LastError,omitempty"`
	// PublishedAtUnix is the wall-clock time of this status
	// message (set by the emitter, not by the reconciler).
	PublishedAtUnix int64 `json:"PublishedAtUnix"`
	// DropsPackets is the running total of packets dropped by
	// the firewall's default-drop tail rule on the input chain.
	// Surfaced by weft-microvm-agent as a Prometheus counter
	// (weft_microvm_agent_firewall_drops_total) so operators
	// can spot port-scan storms / mis-configured workloads at
	// a glance. Monotonic across reconciles ; 0 means either
	// no drops have happened yet OR the table was just
	// reinstalled (kernel resets the counter when the rule is
	// recreated — the metric is reset-aware so a Prometheus
	// rate() over it still makes sense).
	DropsPackets uint64 `json:"DropsPackets,omitempty"`
	// DropsBytes : matching byte counter, same semantics.
	DropsBytes uint64 `json:"DropsBytes,omitempty"`
}
