//go:build linux

package hostmetrics

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// readCPU parses the aggregate "cpu" line at the top of /proc/stat :
//
//	cpu  user nice system idle iowait irq softirq steal guest guest_nice
//
// All counts are in USER_HZ jiffies. We consume the first 8 fields ;
// guest / guest_nice exist on every modern kernel but the values are
// already included in user / nice, so reading them would double-count.
func readCPU() (cpuCounters, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuCounters{}, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 9 {
			return cpuCounters{}, fmt.Errorf("/proc/stat short cpu line: %q", line)
		}
		v := make([]uint64, 8)
		for i := 0; i < 8; i++ {
			x, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return cpuCounters{}, fmt.Errorf("/proc/stat field %d %q: %w", i, fields[i+1], err)
			}
			v[i] = x
		}
		return cpuCounters{
			user: v[0], nice: v[1], sys: v[2], idle: v[3],
			iowait: v[4], irq: v[5], softirq: v[6], steal: v[7],
		}, nil
	}
	return cpuCounters{}, fmt.Errorf("/proc/stat: cpu line not found")
}

// readMem returns {total, used} from /proc/meminfo. Used = MemTotal -
// MemAvailable when available (kernel >= 3.14), falling back to
// MemFree+Buffers+Cached for older kernels we don't actually ship to
// — but the fallback keeps the helper portable in the rare debian-old
// case.
func readMem() (memCounters, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return memCounters{}, err
	}
	m := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		key := line[:colon]
		fields := strings.Fields(strings.TrimSpace(line[colon+1:]))
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		// /proc/meminfo values are in kB. Convert to bytes upfront so
		// callers don't have to remember the unit.
		m[key] = v * 1024
	}
	total := m["MemTotal"]
	if total == 0 {
		return memCounters{}, fmt.Errorf("/proc/meminfo: MemTotal missing")
	}
	avail, ok := m["MemAvailable"]
	if !ok {
		avail = m["MemFree"] + m["Buffers"] + m["Cached"]
	}
	if avail > total {
		avail = total
	}
	return memCounters{total: total, used: total - avail}, nil
}

// readNet sums rx_bytes (field 1) + tx_bytes (field 9) across every
// non-loopback interface in /proc/net/dev. Format :
//
//	Inter-|   Receive                                                |  Transmit
//	 face |bytes    packets errs drop fifo frame compressed multicast|bytes  ...
//	  eth0:  12345     67       0    0    0     0          0         0  89    ...
//
// We skip the two-line header (no ":" → no IndexByte hit) and the
// "lo:" loopback interface so the published rate reflects external
// traffic only. Bridge / tap / VM-attached interfaces are included :
// for a hypervisor host, that's the operator's true throughput.
func readNet() (netCounters, error) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return netCounters{}, err
	}
	var c netCounters
	for _, line := range strings.Split(string(data), "\n") {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:colon])
		if iface == "" || iface == "lo" {
			continue
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) < 16 {
			continue
		}
		rx, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		tx, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			continue
		}
		c.rxBytes += rx
		c.txBytes += tx
	}
	return c, nil
}
