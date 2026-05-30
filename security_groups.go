package weft

// security_groups.go owns the per-project firewall registry. A
// security group is a named container of ingress / egress rules
// that gets attached to VM interfaces at boot. Pattern matches
// AWS / OpenStack: rules don't have independent identity — they
// live as a slice on the parent group and are edited atomically
// via setRules().
//
// Schema:
//
//   security_group "abc-…" {
//     project_uuid = "p-…"
//     name         = "web"
//     description  = "public-facing HTTP / HTTPS"
//     rule {
//       direction      = "ingress"     # ingress | egress
//       protocol       = "tcp"         # tcp | udp | icmp | any
//       port_min       = 443
//       port_max       = 443
//       remote_cidr    = "0.0.0.0/0"   # optional
//       remote_group   = "lb-…"        # optional (UUID)
//     }
//     created_at = "..."
//   }
//
// One of remote_cidr / remote_group must be set per rule.
// (UUID, ProjectUUID, CreatedAt) immutable; name, description,
// rules mutable.

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// SecurityRuleDirection: ingress / egress.
type SecurityRuleDirection string

const (
	SGDirectionIngress SecurityRuleDirection = "ingress"
	SGDirectionEgress  SecurityRuleDirection = "egress"
)

// SecurityRuleProtocol enumerates the L4 protocols the platform
// understands at registration time. "any" matches everything.
type SecurityRuleProtocol string

const (
	SGProtocolTCP  SecurityRuleProtocol = "tcp"
	SGProtocolUDP  SecurityRuleProtocol = "udp"
	SGProtocolICMP SecurityRuleProtocol = "icmp"
	SGProtocolAny  SecurityRuleProtocol = "any"
)

// SecurityRule is one entry inside a SecurityGroup. Validation
// happens in validateSecurityRule.
type SecurityRule struct {
	Direction   SecurityRuleDirection `json:"direction"`
	Protocol    SecurityRuleProtocol  `json:"protocol"`
	PortMin     int                   `json:"port_min,omitempty"`
	PortMax     int                   `json:"port_max,omitempty"`
	RemoteCIDR  string                `json:"remote_cidr,omitempty"`
	RemoteGroup string                `json:"remote_group,omitempty"` // UUID of another SG
}

// SecurityGroup is one entry in the registry.
type SecurityGroup struct {
	UUID        string         `json:"uuid"`
	ProjectUUID string         `json:"project_uuid"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Rules       []SecurityRule `json:"rules"`
	CreatedAt   time.Time      `json:"created_at"`
}

// HCL document structure.
type securityGroupsDoc struct {
	Groups []securityGroupBlock `hcl:"security_group,block"`
}

type securityGroupBlock struct {
	UUID        string              `hcl:",label"`
	ProjectUUID string              `hcl:"project_uuid"`
	Name        string              `hcl:"name"`
	Description string              `hcl:"description,optional"`
	Rules       []securityRuleBlock `hcl:"rule,block"`
	CreatedAt   string              `hcl:"created_at"`
}

type securityRuleBlock struct {
	Direction   string `hcl:"direction"`
	Protocol    string `hcl:"protocol"`
	PortMin     int    `hcl:"port_min,optional"`
	PortMax     int    `hcl:"port_max,optional"`
	RemoteCIDR  string `hcl:"remote_cidr,optional"`
	RemoteGroup string `hcl:"remote_group,optional"`
}

// securityGroupRegistry mirrors the volume / network shape.
type securityGroupRegistry struct {
	mu         sync.Mutex
	storage    Storage
	byUUID     map[string]SecurityGroup
	nameIdx    map[string]string                // (projectUUID,name) → UUID
	projectIdx map[string]map[string]struct{}   // projectUUID → set-of-UUIDs
}

func loadSecurityGroupRegistry(ctx context.Context, storage Storage) (*securityGroupRegistry, error) {
	reg := &securityGroupRegistry{
		storage:    storage,
		byUUID:     make(map[string]SecurityGroup),
		nameIdx:    make(map[string]string),
		projectIdx: make(map[string]map[string]struct{}),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load security-group registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc securityGroupsDoc
	if err := hclsimple.Decode("security-groups.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse security-group registry: %w", err)
	}
	for _, b := range doc.Groups {
		created, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
		rules := make([]SecurityRule, 0, len(b.Rules))
		for _, r := range b.Rules {
			rules = append(rules, SecurityRule{
				Direction:   SecurityRuleDirection(r.Direction),
				Protocol:    SecurityRuleProtocol(r.Protocol),
				PortMin:     r.PortMin,
				PortMax:     r.PortMax,
				RemoteCIDR:  r.RemoteCIDR,
				RemoteGroup: r.RemoteGroup,
			})
		}
		g := SecurityGroup{
			UUID:        b.UUID,
			ProjectUUID: b.ProjectUUID,
			Name:        b.Name,
			Description: b.Description,
			Rules:       rules,
			CreatedAt:   created,
		}
		reg.byUUID[g.UUID] = g
		reg.nameIdx[securityGroupNameKey(g.ProjectUUID, g.Name)] = g.UUID
		if _, ok := reg.projectIdx[g.ProjectUUID]; !ok {
			reg.projectIdx[g.ProjectUUID] = make(map[string]struct{})
		}
		reg.projectIdx[g.ProjectUUID][g.UUID] = struct{}{}
	}
	return reg, nil
}

func securityGroupNameKey(projectUUID, name string) string {
	return projectUUID + "\x00" + name
}

// saveLocked writes via Storage. Caller holds mu.
func (r *securityGroupRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type: 0,
		Bytes: []byte(
			"# weft security-group registry — UUID-keyed per [[weft-uuid-keyed-resources]].\n" +
				"# Edit `name`, `description`, and `rule {}` blocks; never change the\n" +
				"# security_group label (UUID) or `project_uuid`.\n\n",
		),
	}})
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	for _, u := range uuids {
		g := r.byUUID[u]
		block := body.AppendNewBlock("security_group", []string{u})
		bb := block.Body()
		bb.SetAttributeValue("project_uuid", cty.StringVal(g.ProjectUUID))
		bb.SetAttributeValue("name", cty.StringVal(g.Name))
		if g.Description != "" {
			bb.SetAttributeValue("description", cty.StringVal(g.Description))
		}
		for _, rule := range g.Rules {
			rb := bb.AppendNewBlock("rule", nil).Body()
			rb.SetAttributeValue("direction", cty.StringVal(string(rule.Direction)))
			rb.SetAttributeValue("protocol", cty.StringVal(string(rule.Protocol)))
			if rule.PortMin != 0 {
				rb.SetAttributeValue("port_min", cty.NumberIntVal(int64(rule.PortMin)))
			}
			if rule.PortMax != 0 {
				rb.SetAttributeValue("port_max", cty.NumberIntVal(int64(rule.PortMax)))
			}
			if rule.RemoteCIDR != "" {
				rb.SetAttributeValue("remote_cidr", cty.StringVal(rule.RemoteCIDR))
			}
			if rule.RemoteGroup != "" {
				rb.SetAttributeValue("remote_group", cty.StringVal(rule.RemoteGroup))
			}
		}
		bb.SetAttributeValue("created_at", cty.StringVal(g.CreatedAt.Format(time.RFC3339Nano)))
		body.AppendNewline()
	}
	return r.storage.Save(context.Background(), f.Bytes())
}

// validateSecurityRule enforces: direction in {ingress,egress};
// protocol known; port range coherent (only when protocol takes
// ports); exactly one of remote_cidr / remote_group set.
func validateSecurityRule(rule SecurityRule) error {
	switch rule.Direction {
	case SGDirectionIngress, SGDirectionEgress:
	default:
		return fmt.Errorf("unknown direction %q (want ingress or egress)", rule.Direction)
	}
	switch rule.Protocol {
	case SGProtocolTCP, SGProtocolUDP, SGProtocolICMP, SGProtocolAny:
	default:
		return fmt.Errorf("unknown protocol %q", rule.Protocol)
	}
	// Port range only meaningful for tcp/udp.
	hasPorts := rule.PortMin != 0 || rule.PortMax != 0
	if hasPorts {
		switch rule.Protocol {
		case SGProtocolTCP, SGProtocolUDP:
		default:
			return fmt.Errorf("port range only valid for tcp/udp, not %q", rule.Protocol)
		}
		if rule.PortMin < 0 || rule.PortMin > 65535 || rule.PortMax < 0 || rule.PortMax > 65535 {
			return fmt.Errorf("ports out of range [0,65535]: min=%d max=%d", rule.PortMin, rule.PortMax)
		}
		if rule.PortMax != 0 && rule.PortMin > rule.PortMax {
			return fmt.Errorf("port_min %d > port_max %d", rule.PortMin, rule.PortMax)
		}
	}
	// Exactly one of remote_cidr / remote_group.
	hasCIDR := rule.RemoteCIDR != ""
	hasGroup := rule.RemoteGroup != ""
	if hasCIDR == hasGroup {
		return fmt.Errorf("exactly one of remote_cidr / remote_group must be set")
	}
	if hasCIDR {
		if _, _, err := net.ParseCIDR(rule.RemoteCIDR); err != nil {
			return fmt.Errorf("remote_cidr %q: %w", rule.RemoteCIDR, err)
		}
	}
	return nil
}

// lookupByUUID returns (SecurityGroup, true) when the UUID is known.
func (r *securityGroupRegistry) lookupByUUID(uuid string) (SecurityGroup, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.byUUID[uuid]
	return g, ok
}

// lookupByName resolves (projectUUID, name) → SecurityGroup.
func (r *securityGroupRegistry) lookupByName(projectUUID, name string) (SecurityGroup, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuid, ok := r.nameIdx[securityGroupNameKey(projectUUID, name)]
	if !ok {
		return SecurityGroup{}, false
	}
	g, ok := r.byUUID[uuid]
	return g, ok
}

// listForProject returns every security group owned by the
// project, sorted by name.
func (r *securityGroupRegistry) listForProject(projectUUID string) []SecurityGroup {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuids := r.projectIdx[projectUUID]
	if len(uuids) == 0 {
		return nil
	}
	out := make([]SecurityGroup, 0, len(uuids))
	for u := range uuids {
		out = append(out, r.byUUID[u])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// list returns every security group across all projects, sorted
// by (ProjectUUID, Name).
func (r *securityGroupRegistry) list() []SecurityGroup {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SecurityGroup, 0, len(r.byUUID))
	for _, g := range r.byUUID {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectUUID != out[j].ProjectUUID {
			return out[i].ProjectUUID < out[j].ProjectUUID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// CreateSecurityGroupSpec carries the inputs to create().
type CreateSecurityGroupSpec struct {
	ProjectUUID string
	Name        string
	Description string
	Rules       []SecurityRule // optional — empty group is fine
}

// create registers a new SecurityGroup. Refuses name collisions
// within the project and invalid rules.
func (r *securityGroupRegistry) create(spec CreateSecurityGroupSpec) (SecurityGroup, error) {
	if spec.ProjectUUID == "" {
		return SecurityGroup{}, fmt.Errorf("empty project_uuid")
	}
	if spec.Name == "" {
		return SecurityGroup{}, fmt.Errorf("empty security-group name")
	}
	for i, rule := range spec.Rules {
		if err := validateSecurityRule(rule); err != nil {
			return SecurityGroup{}, fmt.Errorf("rule[%d]: %w", i, err)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := securityGroupNameKey(spec.ProjectUUID, spec.Name)
	if _, taken := r.nameIdx[key]; taken {
		return SecurityGroup{}, fmt.Errorf("security-group name %q already in use in project %s", spec.Name, spec.ProjectUUID)
	}
	g := SecurityGroup{
		UUID:        newUUID(),
		ProjectUUID: spec.ProjectUUID,
		Name:        spec.Name,
		Description: spec.Description,
		Rules:       append([]SecurityRule(nil), spec.Rules...),
		CreatedAt:   time.Now().UTC(),
	}
	r.byUUID[g.UUID] = g
	r.nameIdx[key] = g.UUID
	if _, ok := r.projectIdx[g.ProjectUUID]; !ok {
		r.projectIdx[g.ProjectUUID] = make(map[string]struct{})
	}
	r.projectIdx[g.ProjectUUID][g.UUID] = struct{}{}
	if err := r.saveLocked(); err != nil {
		delete(r.byUUID, g.UUID)
		delete(r.nameIdx, key)
		delete(r.projectIdx[g.ProjectUUID], g.UUID)
		if len(r.projectIdx[g.ProjectUUID]) == 0 {
			delete(r.projectIdx, g.ProjectUUID)
		}
		return SecurityGroup{}, err
	}
	return g, nil
}

// setName renames within the project. Refuses name collisions.
func (r *securityGroupRegistry) setName(uuid, newName string) error {
	if newName == "" {
		return fmt.Errorf("empty security-group name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("security-group %q not found", uuid)
	}
	if g.Name == newName {
		return nil
	}
	newKey := securityGroupNameKey(g.ProjectUUID, newName)
	if existing, taken := r.nameIdx[newKey]; taken && existing != uuid {
		return fmt.Errorf("security-group name %q already in use in project %s", newName, g.ProjectUUID)
	}
	delete(r.nameIdx, securityGroupNameKey(g.ProjectUUID, g.Name))
	g.Name = newName
	r.byUUID[uuid] = g
	r.nameIdx[newKey] = uuid
	return r.saveLocked()
}

// setDescription updates the human-readable description.
func (r *securityGroupRegistry) setDescription(uuid, desc string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("security-group %q not found", uuid)
	}
	g.Description = desc
	r.byUUID[uuid] = g
	return r.saveLocked()
}

// setRules replaces the entire rule set atomically. Each rule is
// validated before any write happens. Pass nil / empty to clear.
func (r *securityGroupRegistry) setRules(uuid string, rules []SecurityRule) error {
	for i, rule := range rules {
		if err := validateSecurityRule(rule); err != nil {
			return fmt.Errorf("rule[%d]: %w", i, err)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("security-group %q not found", uuid)
	}
	g.Rules = append([]SecurityRule(nil), rules...)
	r.byUUID[uuid] = g
	return r.saveLocked()
}

// delete drops a security group from the registry. No cascade —
// callers must clear any VM ↔ SG attachment first.
func (r *securityGroupRegistry) delete(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("security-group %q not found", uuid)
	}
	delete(r.byUUID, uuid)
	delete(r.nameIdx, securityGroupNameKey(g.ProjectUUID, g.Name))
	delete(r.projectIdx[g.ProjectUUID], uuid)
	if len(r.projectIdx[g.ProjectUUID]) == 0 {
		delete(r.projectIdx, g.ProjectUUID)
	}
	return r.saveLocked()
}
