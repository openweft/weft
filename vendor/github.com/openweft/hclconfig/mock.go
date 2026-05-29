package hclconfig

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
)

// DiskDef represents one disk block from HCL.
type DiskDef struct {
	// Image is the OCI image reference (empty for blank data disks).
	Image string
	// Label is the optional block label (e.g. "data1" from `disk data1 { }`).
	Label string
	// SizeGiB is the disk size in gibibytes.
	SizeGiB int
	// Mountpoint is the optional guest-OS mount path (may use ${self.label}).
	Mountpoint string
}

// ExtraDisk carries provisioning metadata for a data disk beyond the boot disk.
type ExtraDisk struct {
	SizeGiB    int    `json:"size_gib"`
	Label      string `json:"label,omitempty"`
	Mountpoint string `json:"mountpoint,omitempty"`
}

type VMDef struct {
	Name   string
	Count  int
	CPU    int
	Memory int
	// Disks holds all disk blocks in declaration order.
	// Disks[0] is the boot disk; Disks[1:] are extra data disks.
	Disks []DiskDef
	// Script is the raw cloud-init user_data content from the `script` attribute.
	Script string
	// SSHUser is the SSH username to use when connecting to this VM.
	// Parsed from `ssh { user = "..." }` inside the vms block.
	SSHUser string
	// SSHKeyPath is the path to the SSH private key file for this VM,
	// resolved from `ssh { keypair = ... }` inside the vms block.
	SSHKeyPath string
	// SSHPubKeyPath is the path to the SSH public key file for this VM,
	// derived from SSHKeyPath by appending ".pub".
	SSHPubKeyPath string
	// SSHPubKey is the content of the SSH public key file.
	SSHPubKey string
}

type Row struct {
	Name         string `json:"name"`
	CPU          int    `json:"cpu"`
	Mem          int    `json:"mem"`
	Disk         int    `json:"disk"`
	Image        string `json:"image"`
	OS           string `json:"os"`
	Distribution string `json:"distribution"`
	State        string `json:"state"`
	IP           string `json:"ip"`
	// ExtraDisks holds metadata for each data disk beyond the boot disk.
	ExtraDisks []ExtraDisk `json:"extra_disks,omitempty"`
	// SSHUser is the SSH username for this VM; set from vms.ssh.user in HCL.
	SSHUser string `json:"-"`
	// Script is the cloud-init user_data script; excluded from JSON output.
	Script string `json:"-"`
	// CloudInitISO is the path to the per-VM cloud-init seed ISO; set by the
	// up command before starting the VM. Excluded from JSON output.
	CloudInitISO string `json:"-"`
	// CloudInitISOData holds the raw bytes of the cloud-init seed ISO built
	// before provisioning. The up command writes them to the VM directory
	// after the VM directory is created by CloneVM.
	CloudInitISOData []byte `json:"-"`
	// SSHKeyPath is the path to the SSH private key file for this VM.
	SSHKeyPath string `json:"-"`
	// SSHPubKeyPath is the path to the SSH public key file; derived from
	// SSHKeyPath by appending ".pub".
	SSHPubKeyPath string `json:"-"`
	// SSHPubKey is the content of the SSH public key injected into
	// authorized_keys by cloud-init to bootstrap the first connection.
	SSHPubKey string `json:"-"`
}

// ParseMockConfig parses a mock HCL config (directory or single file) in a
// tolerant way for tests.
func ParseMockConfig(path string) ([]VMDef, string, string, error) {
	if path == "" {
		path = ".mock/hcl"
	}
	data, _, err := resolveConfig(path)
	if err != nil {
		return nil, "", "", err
	}
	s := string(data)

	ak, mid, err := parseMockHeader(s)
	if err != nil {
		return nil, "", "", err
	}

	mb := LoadMockBlock(path)
	out := parseVMDefs(s)
	enrichVMDefs(out, s, LoadKeypairs(path), mb)
	if err := validateVMDefs(out); err != nil {
		return nil, "", "", err
	}

	return out, ak, mid, nil
}

// parseMockHeader extracts the mock block id and authorized keys path.
func parseMockHeader(s string) (string, string, error) {
	mr := regexp.MustCompile(`(?ms)mock\s*(?:"([^"]+)"|([A-Za-z_][A-Za-z0-9_-]*))\s*\{(.*?)\}`)
	if !mr.MatchString(s) {
		return "", "", fmt.Errorf("mock config must declare a labeled mock block: mock \"ID\" { ... }")
	}
	m := mr.FindStringSubmatch(s)
	mid := ""
	if m[1] != "" {
		mid = m[1]
	} else {
		mid = m[2]
	}
	mb := m[3]
	ak := ""
	if ark := regexp.MustCompile(`authorized[_-]?keys[_-]?path\s*=\s*"([^\"]+)"`); ark.MatchString(mb) {
		ak = ark.FindStringSubmatch(mb)[1]
	}
	return ak, mid, nil
}

// parseSizeGiB parses a quoted size string (e.g. "20Gi", "10G", "2Ti") into
// a GiB integer. A unit suffix is mandatory; plain integers are rejected to
// prevent silent misconfiguration.
// Supported units: Mi/M (mebibytes → ÷1024), Gi/G (gibibytes), Ti/T (tebibytes → ×1024).
func parseSizeGiB(raw string) (int, bool) {
	s := strings.Trim(raw, `"`)
	m := regexp.MustCompile(`^([0-9]+)(Mi?|Gi?|Ti?)$`).FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	v, err := strconv.Atoi(m[1])
	if err != nil || v <= 0 {
		return 0, false
	}
	switch strings.ToUpper(m[2])[0] {
	case 'M':
		gib := v / 1024
		if gib <= 0 {
			return 0, false
		}
		return gib, true
	case 'G':
		return v, true
	case 'T':
		return v * 1024, true
	}
	return 0, false
}

// extractCurlyBody locates the opening `{` for the match starting at openPos
// (which must be the index of `{` in s) and returns the content inside the
// matching closing `}`, respecting nested braces.  Returns ("", false) on error.
func extractCurlyBody(s string, openPos int) (string, int, bool) {
	if openPos >= len(s) || s[openPos] != '{' {
		return "", 0, false
	}
	depth := 1
	for i := openPos + 1; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[openPos+1 : i], i + 1, true
			}
		}
	}
	return "", 0, false
}

// stripHCLComments removes /* ... */ block comments and // line comments from
// an HCL source string.  Newlines inside block comments are preserved so that
// line numbers remain stable for any subsequent error messages.
func stripHCLComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		// Block comment: /* ... */
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			i += 2
			for i < len(s) {
				if i+1 < len(s) && s[i] == '*' && s[i+1] == '/' {
					i += 2
					break
				}
				if s[i] == '\n' {
					b.WriteByte('\n')
				} else {
					b.WriteByte(' ')
				}
				i++
			}
			continue
		}
		// Line comment: // ...
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '/' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// extractVMSBlocks returns all (name, body) pairs for `vms NAME { ... }` blocks
// in s, with body contents correctly balanced across nested braces.
func extractVMSBlocks(s string) [][2]string {
	re := regexp.MustCompile(`vms\s+([A-Za-z0-9._-]+)\s*\{`)
	var results [][2]string
	offset := 0
	for {
		loc := re.FindStringSubmatchIndex(s[offset:])
		if loc == nil {
			break
		}
		name := s[offset+loc[2] : offset+loc[3]]
		openPos := offset + loc[1] - 1 // position of '{'
		body, end, ok := extractCurlyBody(s, openPos)
		if !ok {
			break
		}
		results = append(results, [2]string{name, body})
		offset = end
	}
	return results
}

// extractDiskBlocks returns (label, body) pairs for all `disk` blocks in s,
// supporting both unlabeled `disk { ... }` and labeled `disk LABEL { ... }`.
func extractDiskBlocks(s string) [][2]string {
	re := regexp.MustCompile(`\bdisk\b(?:\s+([A-Za-z][A-Za-z0-9_-]*))?\s*\{`)
	var results [][2]string
	offset := 0
	for {
		loc := re.FindStringSubmatchIndex(s[offset:])
		if loc == nil {
			break
		}
		label := ""
		if loc[2] >= 0 {
			label = s[offset+loc[2] : offset+loc[3]]
		}
		openPos := offset + loc[1] - 1 // position of '{'
		body, end, ok := extractCurlyBody(s, openPos)
		if !ok {
			break
		}
		results = append(results, [2]string{label, body})
		offset = end
	}
	return results
}

// parseMountpointExpr extracts the mountpoint value from a disk block body.
// It handles:
//   - quoted strings:            mountpoint = "/mnt/data"
//   - unquoted paths with ${…}:  mountpoint = /mnt/${self.label}
//   - join() calls (multiline):  mountpoint = join("/",[…, self.label])
//
// In all cases ${self.label} and bare self.label occurrences are substituted
// with the disk's label string.
func parseMountpointExpr(body, label string) string {
	loc := regexp.MustCompile(`mountpoint\s*=\s*`).FindStringIndex(body)
	if loc == nil {
		return ""
	}
	rest := strings.TrimSpace(body[loc[1]:])

	// quoted string literal
	if strings.HasPrefix(rest, `"`) {
		end := strings.Index(rest[1:], `"`)
		if end < 0 {
			return ""
		}
		mnt := rest[1 : end+1]
		mnt = strings.ReplaceAll(mnt, "${self.label}", label)
		return mnt
	}

	// join() function call — potentially multiline
	if strings.HasPrefix(rest, "join(") {
		depth, end := 0, -1
		for i, ch := range rest {
			switch ch {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					end = i
				}
			}
			if end >= 0 {
				break
			}
		}
		if end <= 0 {
			return ""
		}
		expr := rest[:end+1]
		// substitute self.label references before evaluation
		quotedLabel := `"` + label + `"`
		expr = strings.ReplaceAll(expr, `"`+`${self.label}`+`"`, quotedLabel)
		expr = strings.ReplaceAll(expr, "${self.label}", label)
		expr = strings.ReplaceAll(expr, "self.label", quotedLabel)
		return evalJoinExpr(expr)
	}

	// bare unquoted path (e.g. /mnt/${self.label})
	if nl := strings.IndexAny(rest, "\n#"); nl >= 0 {
		mnt := strings.TrimSpace(rest[:nl])
		mnt = strings.ReplaceAll(mnt, "${self.label}", label)
		return mnt
	}
	mnt := strings.TrimSpace(rest)
	mnt = strings.ReplaceAll(mnt, "${self.label}", label)
	return mnt
}

// evalJoinExpr evaluates a simple join("sep", ["a", "b", …]) expression
// and returns the joined string. All elements must be quoted strings.
func evalJoinExpr(expr string) string {
	m := regexp.MustCompile(`join\("([^"]*)"\s*,\s*\[`).FindStringSubmatch(expr)
	if m == nil {
		return ""
	}
	sep := m[1]
	start := strings.Index(expr, "[")
	end := strings.LastIndex(expr, "]")
	if start < 0 || end <= start {
		return ""
	}
	inner := expr[start+1 : end]
	var parts []string
	for _, pm := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(inner, -1) {
		parts = append(parts, pm[1])
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, sep)
}

// parseVMDefs extracts VM definitions (name/count/disk/from) from the HCL.
func parseVMDefs(s string) []VMDef {
	s = stripHCLComments(s)
	blocks := extractVMSBlocks(s)
	var out []VMDef
	for _, bl := range blocks {
		name := bl[0]
		body := bl[1]
		count := 0
		if cr := regexp.MustCompile(`count\s*=\s*([0-9]+)`); cr.MatchString(body) {
			cs := cr.FindStringSubmatch(body)
			if v, err := strconv.Atoi(cs[1]); err == nil && v > 0 {
				count = v
			}
		}
		diskBlocks := extractDiskBlocks(body)
		var disks []DiskDef
		for _, db := range diskBlocks {
			label := db[0]
			dBody := db[1]
			dd := DiskDef{Label: label}
			if fr := regexp.MustCompile(`from\s*=\s*(?:"([^"]+)"|([A-Za-z][A-Za-z0-9._+-]*(?:\.[A-Za-z0-9_-]+)*))`); fr.MatchString(dBody) {
				f := fr.FindStringSubmatch(dBody)
				if f[1] != "" {
					dd.Image = f[1] // quoted string literal
				} else if f[2] != "" {
					dd.Image = f[2] // token ref like "img.debian-13.from" or "oci.debian.from"
				}
			}
			if szRe := regexp.MustCompile(`size\s*=\s*"([^"]+)"`); szRe.MatchString(dBody) {
				ss := szRe.FindStringSubmatch(dBody)
				if gib, ok := parseSizeGiB(ss[1]); ok {
					dd.SizeGiB = gib
				}
			}
			dd.Mountpoint = parseMountpointExpr(dBody, label)
			disks = append(disks, dd)
		}
		out = append(out, VMDef{Name: name, Count: count, Disks: disks})
	}
	return out
}

// enrichVMDefs fills CPU, memory and disk defaults from VM bodies.
// mockDefaults provides the mock-level ssh defaults inherited by any vm without its own ssh block.
func enrichVMDefs(out []VMDef, s string, keypairs map[string]string, mockDefaults MockBlock) {
	s = stripHCLComments(s)
	// Build a name→body map using brace-balanced extraction.
	bodyMap := make(map[string]string)
	for _, bl := range extractVMSBlocks(s) {
		bodyMap[bl[0]] = bl[1]
	}
	for i := range out {
		body, ok := bodyMap[out[i].Name]
		if !ok {
			continue
		}
		if cr := regexp.MustCompile(`cpu\s*=\s*([0-9]+)`); cr.MatchString(body) {
			cs := cr.FindStringSubmatch(body)
			if v, err := strconv.Atoi(cs[1]); err == nil {
				out[i].CPU = v
			}
		}
		if mr := regexp.MustCompile(`(?:memory|mem)\s*=\s*([0-9]+)`); mr.MatchString(body) {
			ms := mr.FindStringSubmatch(body)
			if v, err := strconv.Atoi(ms[1]); err == nil {
				out[i].Memory = v
			}
		}
		// if the boot disk size wasn't set by parseVMDefs, try to find it in the body
		if len(out[i].Disks) > 0 && out[i].Disks[0].SizeGiB == 0 {
			if ds := regexp.MustCompile(`size\s*=\s*"([^"]+)"`); ds.MatchString(body) {
				if gib, ok := parseSizeGiB(ds.FindStringSubmatch(body)[1]); ok {
					out[i].Disks[0].SizeGiB = gib
				}
			}
		}
		// parse script = <<[-]MARKER ... MARKER heredoc
		out[i].Script = parseHeredocScript(body)
		// parse ssh { user = "..." keypair = ... } nested block
		if sshLoc := regexp.MustCompile(`(?m)ssh\s*\{`).FindStringIndex(body); sshLoc != nil {
			if sb, _, ok := extractCurlyBody(body, sshLoc[1]-1); ok {
				if ur := regexp.MustCompile(`user\s*=\s*"([^"]+)"`); ur.MatchString(sb) {
					out[i].SSHUser = ur.FindStringSubmatch(sb)[1]
				}
				// keypair attribute: either a literal path or a keypair.<name> reference
				if kp := regexp.MustCompile(`keypair\s*=\s*(?:"([^"]+)"|keypair\.([A-Za-z_][A-Za-z0-9_-]*))`); kp.MatchString(sb) {
					m := kp.FindStringSubmatch(sb)
					var privKeyPath string
					if m[1] != "" {
						// literal string value (private key path)
						privKeyPath = expandHomePath(m[1])
					} else if m[2] != "" {
						// reference: keypair.<name>
						if p, ok2 := keypairs[m[2]]; ok2 {
							privKeyPath = p
						}
					}
					if privKeyPath != "" {
						out[i].SSHKeyPath = privKeyPath
						pubKeyPath := privKeyPath + ".pub"
						out[i].SSHPubKeyPath = pubKeyPath
						if content, err := os.ReadFile(pubKeyPath); err == nil {
							out[i].SSHPubKey = strings.TrimSpace(string(content))
						}
					}
				}
			}
		}
		// Apply mock-level ssh defaults to any vm that didn't define its own ssh block.
		if out[i].SSHUser == "" && mockDefaults.SSHUser != "" {
			out[i].SSHUser = mockDefaults.SSHUser
		}
		if out[i].SSHKeyPath == "" && mockDefaults.SSHKeyPath != "" {
			out[i].SSHKeyPath = mockDefaults.SSHKeyPath
			pubKeyPath := mockDefaults.SSHKeyPath + ".pub"
			out[i].SSHPubKeyPath = pubKeyPath
			if content, err := os.ReadFile(pubKeyPath); err == nil {
				out[i].SSHPubKey = strings.TrimSpace(string(content))
			}
		}
	}
}

// parseHeredocScript extracts the content of a `script = <<[-]MARKER...MARKER`
// heredoc from a VM block body. For <<- heredocs it strips the minimal leading
// indentation. Returns empty string if no script attribute is present.
var heredocHeaderRe = regexp.MustCompile(`(?m)script\s*=\s*<<(-?)(\w+)\r?\n`)

func parseHeredocScript(body string) string {
	m := heredocHeaderRe.FindStringSubmatchIndex(body)
	if m == nil {
		return ""
	}
	strippedStr := body[m[2]:m[3]]
	marker := body[m[4]:m[5]]
	contentStart := m[1] // position right after the opening `\n`

	// Find the terminating marker line: a line whose trimmed value equals marker.
	rest := body[contentStart:]
	lines := strings.Split(rest, "\n")
	markerIdx := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == marker {
			markerIdx = i
			break
		}
	}
	if markerIdx < 0 {
		return ""
	}
	content := strings.Join(lines[:markerIdx], "\n")

	if strippedStr == "-" {
		content = stripIndent(content)
	}
	// Ensure the script is executable by cloud-init: prepend shebang if absent.
	if !strings.HasPrefix(strings.TrimLeft(content, " \t"), "#!") {
		content = "#!/bin/bash\n" + content
	}
	return content
}

// stripIndent removes the minimal common leading whitespace from all non-empty
// lines (used for <<- heredoc stripping).
func stripIndent(s string) string {
	lines := strings.Split(s, "\n")
	min := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if min < 0 || n < min {
			min = n
		}
	}
	if min <= 0 {
		return s
	}
	for i, l := range lines {
		if len(l) >= min {
			lines[i] = l[min:]
		}
	}
	return strings.Join(lines, "\n")
}

// validateVMDefs ensures VM definitions have required fields.
func validateVMDefs(out []VMDef) error {
	var errs []string
	for _, v := range out {
		// count = 0 is explicitly valid: the VM definition is disabled.
		if v.Count == 0 {
			continue
		}
		if v.Count < 0 {
			errs = append(errs, fmt.Sprintf("vm %s: count must be >= 0", v.Name))
		}
		if v.CPU <= 0 {
			errs = append(errs, fmt.Sprintf("vm %s: missing or invalid cpu", v.Name))
		}
		if v.Memory <= 0 {
			errs = append(errs, fmt.Sprintf("vm %s: missing or invalid mem/memory", v.Name))
		}
		if len(v.Disks) == 0 {
			errs = append(errs, fmt.Sprintf("vm %s: missing or invalid disk size", v.Name))
		}
		for j, d := range v.Disks {
			if d.SizeGiB <= 0 {
				errs = append(errs, fmt.Sprintf("vm %s: disk[%d]: missing or invalid size", v.Name, j))
			}
		}
		if v.SSHUser == "" {
			errs = append(errs, fmt.Sprintf("vm %s: missing ssh user (set ssh { user = \"...\" } in the vm block or in the mock block)", v.Name))
		}
		if v.SSHKeyPath == "" {
			errs = append(errs, fmt.Sprintf("vm %s: missing ssh keypair (set ssh { keypair = ... } in the vm block or in the mock block)", v.Name))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid mock config: %s", strings.Join(errs, "; "))
	}
	return nil
}

// BuildRowsFromConfig expands VM definitions into a list of Rows.
// configPath may be a directory or a single HCL file.
func BuildRowsFromConfig(configPath, prefix string, tartMap map[string]map[string]interface{}, ociMap map[string]string) ([]Row, error) {
	if configPath == "" {
		configPath = ".mock/hcl"
	}
	vms, _, mid, err := ParseMockConfig(configPath)
	if err != nil {
		return nil, err
	}
	var rows []Row
	groupPrefix := computeGroupPrefix(vms)
	rows = buildRowsFromVMDefs(vms, groupPrefix, prefix, mid)
	enrichRows(rows, tartMap, ociMap)
	return rows, nil
}

// computeGroupPrefix determines the common prefix across VM names.
func computeGroupPrefix(vms []VMDef) string {
	if len(vms) == 0 {
		return ""
	}
	firstParts := strings.Split(vms[0].Name, "-")
	maxCommon := len(firstParts)
	for _, vm := range vms[1:] {
		parts := strings.Split(vm.Name, "-")
		if len(parts) < maxCommon {
			maxCommon = len(parts)
		}
		for i := 0; i < maxCommon; i++ {
			if parts[i] != firstParts[i] {
				maxCommon = i
				break
			}
		}
	}
	if maxCommon > 0 {
		return strings.Join(firstParts[:maxCommon], "-")
	}
	return ""
}

// buildRowsFromVMDefs expands VMDef entries into Rows with generated names.
func buildRowsFromVMDefs(vms []VMDef, groupPrefix, prefix, mid string) []Row {
	var rows []Row
	for _, v := range vms {
		for i := 0; i < v.Count; i++ {
			base := v.Name
			idToken := prefix
			if mid != "" {
				idToken = mid
			}
			namePart := base
			if groupPrefix != "" && strings.HasPrefix(base, groupPrefix+"-") {
				namePart = strings.TrimPrefix(base, groupPrefix+"-")
			} else if groupPrefix != "" && strings.HasPrefix(base, "mock-"+groupPrefix+"-") {
				namePart = strings.TrimPrefix(base, "mock-"+groupPrefix+"-")
			}
			instName := fmt.Sprintf("mock-%s-%s-%d", idToken, namePart, i+1)
			// collect extra data disks (skip index 0, which is the boot disk)
			extraDisks := make([]ExtraDisk, 0)
			if len(v.Disks) > 1 {
				for _, d := range v.Disks[1:] {
					extraDisks = append(extraDisks, ExtraDisk{
						SizeGiB:    d.SizeGiB,
						Label:      d.Label,
						Mountpoint: d.Mountpoint,
					})
				}
			}
			bootDisk := 0
			bootImage := ""
			if len(v.Disks) > 0 {
				bootDisk = v.Disks[0].SizeGiB
				bootImage = v.Disks[0].Image
			}
			rows = append(rows, Row{Name: instName, CPU: v.CPU, Mem: v.Memory, Disk: bootDisk, Image: bootImage, State: "not-created", ExtraDisks: extraDisks, Script: v.Script, SSHUser: v.SSHUser, SSHKeyPath: v.SSHKeyPath, SSHPubKeyPath: v.SSHPubKeyPath, SSHPubKey: v.SSHPubKey})
		}
	}
	return rows
}

// enrichRows applies runtime/tart/OCI-derived information to rows in-place.
func enrichRows(rows []Row, tartMap map[string]map[string]interface{}, ociMap map[string]string) {
	for i := range rows {
		applyTartInfo(&rows[i], tartMap)
		applyOCIMap(&rows[i], ociMap)
		if rows[i].OS == "" && rows[i].Image != "" {
			if oc := getOSFromImageRef(rows[i].Image); oc != "" {
				rows[i].OS = oc
			}
		}
		if rows[i].Image != "" {
			rows[i].Distribution = getDistributionFromImage(rows[i].Image)
		}
	}
}

// applyTartInfo absorbs tart-provided metadata into a row when available.
func applyTartInfo(r *Row, tartMap map[string]map[string]interface{}) {
	for name, it := range tartMap {
		if name == r.Name || strings.Contains(name, r.Name) {
			if osv, ok := it["os"].(string); ok && osv != "" {
				r.OS = normalizeOS(osv)
			} else if osv, ok := it["OS"].(string); ok && osv != "" {
				r.OS = normalizeOS(osv)
			}
			if r.Image == "" {
				if src, ok := it["source"].(string); ok && src != "" {
					r.Image = src
				} else if src, ok := it["Image"].(string); ok && src != "" {
					r.Image = src
				}
			}
			if st, ok := it["State"].(string); ok && st != "" {
				r.State = strings.ToLower(st)
			} else if st2, ok := it["state"].(string); ok && st2 != "" {
				r.State = strings.ToLower(st2)
			} else if run, ok := it["Running"].(bool); ok {
				if run {
					r.State = "running"
				} else {
					r.State = "stopped"
				}
			} else if run2, ok := it["running"].(bool); ok {
				if run2 {
					r.State = "running"
				} else {
					r.State = "stopped"
				}
			}
			break
		}
	}
}

// applyOCIMap sets an image from ociMap if not present already.
func applyOCIMap(r *Row, ociMap map[string]string) {
	if ociMap == nil {
		return
	}
	// resolve token refs like "img.debian-13.from" or "oci.debian.from"
	if r.Image != "" && strings.HasSuffix(r.Image, ".from") {
		parts := strings.Split(r.Image, ".")
		if len(parts) >= 2 {
			name := strings.Join(parts[1:len(parts)-1], ".")
			if url, ok := ociMap[name]; ok {
				r.Image = url
				return
			}
		}
		r.Image = "" // couldn't resolve token, clear for name-matching fallback
	}
	// name-based fallback: match ociMap key against row name
	if r.Image == "" {
		for k, v := range ociMap {
			if strings.Contains(strings.ToLower(r.Name), strings.ToLower(k)) {
				r.Image = v
				break
			}
		}
	}
}

// RenderTableFromRows writes a visual table to w.
func RenderTableFromRows(rows []Row, w io.Writer) {
	// Use the v1 tablewriter API: header includes DISTRIBUTION and IP is last.
	headers := []string{"", "NAME", "STATE", "OS", "DISTRIBUTION", "CPU", "MEM", "DISK", "IMAGE", "IP"}
	table := tablewriter.NewTable(w, tablewriter.WithHeader(headers), tablewriter.WithHeaderAutoWrap(tw.WrapNone))

	// Ensure the STATE column (index 2) is left-aligned for both header and rows.
	n := len(headers)
	headerAligns := make([]tw.Align, n)
	rowAligns := make([]tw.Align, n)
	for i := 0; i < n; i++ {
		headerAligns[i] = tw.AlignCenter
		rowAligns[i] = tw.AlignLeft
	}
	headerAligns[2] = tw.AlignLeft // STATE header -> left

	table.Configure(func(cfg *tablewriter.Config) {
		cfg.Header.Alignment.PerColumn = headerAligns
		cfg.Row.Alignment.PerColumn = rowAligns
	})

	for _, r := range rows {
		dot := colorDot(r.State)
		_ = table.Append([]string{dot, r.Name, r.State, r.OS, r.Distribution, strconv.Itoa(r.CPU), strconv.Itoa(r.Mem), strconv.Itoa(r.Disk), r.Image, r.IP})
	}
	_ = table.Render()
}

// MarshalRows encodes rows as JSON.
func MarshalRows(rows []Row) ([]byte, error) { return json.MarshalIndent(rows, "", "  ") }

// colorDot returns a colored dot for state.
func colorDot(state string) string {
	s := strings.ToLower(state)
	switch s {
	case "running":
		return "\x1b[32m●\x1b[0m"
	case "not-created", "stopped":
		return "\x1b[31m●\x1b[0m"
	default:
		return "\x1b[33m●\x1b[0m"
	}
}

// getOSFromImageRef infers OS from an OCI image reference's repo name.
func getOSFromImageRef(image string) string {
	if image == "" {
		return ""
	}
	base := filepath.Base(image)
	lb := strings.ToLower(base)
	// infer a distribution token then normalize to high-level OS
	if strings.Contains(lb, "debian") {
		return normalizeOS("debian")
	}
	if strings.Contains(lb, "ubuntu") {
		return normalizeOS("ubuntu")
	}
	if strings.Contains(lb, "rocky") {
		return normalizeOS("rocky")
	}
	if strings.Contains(lb, "alpine") {
		return normalizeOS("alpine")
	}
	if strings.Contains(lb, "centos") {
		return normalizeOS("centos")
	}
	if strings.Contains(lb, "macos") || strings.Contains(lb, "tahoe") || strings.Contains(lb, "darwin") {
		return normalizeOS("macos")
	}
	return ""
}

// normalizeOS normalizes OS names.
func normalizeOS(s string) string {
	sl := strings.ToLower(s)
	// map to high-level OS families: linux or darwin
	if strings.Contains(sl, "macos") || strings.Contains(sl, "tahoe") || strings.Contains(sl, "darwin") {
		return "darwin"
	}
	if strings.Contains(sl, "ubuntu") || strings.Contains(sl, "debian") || strings.Contains(sl, "rocky") || strings.Contains(sl, "centos") || strings.Contains(sl, "alpine") || strings.Contains(sl, "linux") {
		return "linux"
	}
	// fallback: return lowercased input
	return sl
}

// getDistributionFromImage extracts a best-effort distribution identifier from an image ref.
func getDistributionFromImage(image string) string {
	if image == "" {
		return ""
	}
	base := filepath.Base(image)
	if idx := strings.Index(base, ":"); idx != -1 {
		base = base[:idx]
	}
	s := strings.ToLower(base)
	switch {
	case strings.Contains(s, "debian"):
		return "debian"
	case strings.Contains(s, "rocky"):
		return "rocky"
	case strings.Contains(s, "ubuntu"):
		return "ubuntu"
	case strings.Contains(s, "alpine"):
		return "alpine"
	case strings.Contains(s, "centos"):
		return "centos"
	case strings.Contains(s, "tahoe"):
		return "tahoe"
	case strings.Contains(s, "macos"):
		return "macos"
	default:
		// fallback to the base name (without tag)
		return base
	}
}
