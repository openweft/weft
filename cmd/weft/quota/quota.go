// Package quota implements the `weft quota` subcommand group : read
// + write the Quotas message that caps tenant and project resource
// consumption.
//
//	weft quota tenant get <name|uuid>
//	weft quota tenant set <name|uuid> --vcpu=N --ram-gib=N ...
//	weft quota project get <name|uuid>
//	weft quota project set <name|uuid> --vcpu=N --ram-gib=N ...
//
// `get` prints the cap alongside the live `allocated` total so
// operators see headroom at a glance. `set` is a partial PATCH :
// flags left unset reuse the current cap (zero is a valid value —
// pass it explicitly to disable a dimension).
//
// Quota dimensions mirror the proto's Quotas message :
//
//	--vcpu          --ram-gib       --volumes       --volumes-gib
//	--shares        --shares-gib    --buckets       --buckets-gib
//	--registry-gib  --floating-ips
//	--projects      (tenant-only — ignored on a project set)
package quota

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/openweft/weft/cmd/weft/shared"
	"github.com/openweft/weft/cmd/weft/tenant"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// quotaDims is the ordered list of (flag-name, label-for-tables,
// pointer-to-Quotas-field) tuples that every command in this package
// iterates over. Single source of truth keeps the flag set and the
// table output in lockstep.
//
// fieldOf returns a pointer to the field on a *Quotas so applyTo and
// readTo can mutate / read by-flag without reflection.
type quotaDim struct {
	flag        string // CLI flag name : "vcpu", "ram-gib", ...
	label       string // table column / row label : "vcpu", "ram_gib", ...
	doc         string // cobra flag help
	tenantOnly  bool   // "projects" : meaningless at project scope
	fieldOf     func(*weftv1.Quotas) *int32
}

var quotaDims = []quotaDim{
	{"vcpu", "vcpu", "Cap on total vCPU cores", false, func(q *weftv1.Quotas) *int32 { return &q.Vcpu }},
	{"ram-gib", "ram_gib", "Cap on total RAM (GiB)", false, func(q *weftv1.Quotas) *int32 { return &q.RamGib }},
	{"volumes", "volumes", "Cap on number of volumes", false, func(q *weftv1.Quotas) *int32 { return &q.Volumes }},
	{"volumes-gib", "volumes_gib", "Cap on total volume size (GiB)", false, func(q *weftv1.Quotas) *int32 { return &q.VolumesGib }},
	{"shares", "shares", "Cap on number of shares", false, func(q *weftv1.Quotas) *int32 { return &q.Shares }},
	{"shares-gib", "shares_gib", "Cap on total share size (GiB)", false, func(q *weftv1.Quotas) *int32 { return &q.SharesGib }},
	{"buckets", "buckets", "Cap on number of buckets", false, func(q *weftv1.Quotas) *int32 { return &q.Buckets }},
	{"buckets-gib", "buckets_gib", "Cap on total bucket size (GiB)", false, func(q *weftv1.Quotas) *int32 { return &q.BucketsGib }},
	{"registry-gib", "registry_gib", "Cap on container registry storage (GiB)", false, func(q *weftv1.Quotas) *int32 { return &q.RegistryGib }},
	{"floating-ips", "floating_ips", "Cap on floating IPs", false, func(q *weftv1.Quotas) *int32 { return &q.FloatingIps }},
	{"projects", "projects", "Cap on number of projects (tenant scope only ; ignored on a project set)", true, func(q *weftv1.Quotas) *int32 { return &q.Projects }},
}

// Command returns the `weft quota` cobra command group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Read / write tenant + project quotas",
	}
	cmd.AddCommand(
		tenantCmd(socket, sshSocket, sshKey),
		projectCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

// ---------------------------------------------------------------- tenant

func tenantCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tenant",
		Short: "Tenant-level quotas",
	}
	cmd.AddCommand(
		tenantGetCmd(socket, sshSocket, sshKey),
		tenantSetCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func tenantGetCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name|uuid>",
		Short: "Show a tenant's quota cap + current allocation",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			tenantUUID, err := tenant.ResolveArg(c, args[0])
			if err != nil {
				return err
			}
			resp, err := c.GetTenantQuota(context.Background(), &weftv1.GetTenantQuotaRequest{
				TenantUuid: tenantUUID,
			})
			if err != nil {
				return err
			}
			return renderTenantQuota(tenantUUID, resp.Cap, resp.Allocated)
		},
	}
}

func tenantSetCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	backing := make(map[string]*int32, len(quotaDims))
	cmd := &cobra.Command{
		Use:   "set <name|uuid>",
		Short: "Patch a tenant's quota cap (omitted flags reuse the current value)",
		Args:  cobra.ExactArgs(1),
	}
	registerQuotaFlags(cmd, backing, true /* tenantScope */)
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
		if err != nil {
			return err
		}
		defer conn.Close()
		tenantUUID, err := tenant.ResolveArg(c, args[0])
		if err != nil {
			return err
		}
		// Read-modify-write : merge --flags onto the live cap. Server
		// rejects deltas that would shrink the cap below `allocated` ;
		// we surface that error as-is.
		cur, err := c.GetTenantQuota(context.Background(), &weftv1.GetTenantQuotaRequest{TenantUuid: tenantUUID})
		if err != nil {
			return err
		}
		cap := cloneQuotas(cur.Cap)
		applyQuotaFlags(cmd, backing, cap, true /* tenantScope */)
		resp, err := c.SetTenantQuota(context.Background(), &weftv1.SetTenantQuotaRequest{
			TenantUuid: tenantUUID,
			Cap:        cap,
		})
		if err != nil {
			return err
		}
		return renderTenantQuota(tenantUUID, resp.Cap, resp.Allocated)
	}
	return cmd
}

// ---------------------------------------------------------------- project

func projectCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Project-level quotas",
	}
	cmd.AddCommand(
		projectGetCmd(socket, sshSocket, sshKey),
		projectSetCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func projectGetCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name|uuid>",
		Short: "Show a project's quota + its tenant cap + sibling totals",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			projectUUID, err := resolveProject(c, args[0])
			if err != nil {
				return err
			}
			resp, err := c.GetProjectQuota(context.Background(), &weftv1.GetProjectQuotaRequest{
				ProjectUuid: projectUUID,
			})
			if err != nil {
				return err
			}
			return renderProjectQuota(projectUUID, resp.Project, resp.TenantCap, resp.SiblingsTotal)
		},
	}
}

func projectSetCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	backing := make(map[string]*int32, len(quotaDims))
	cmd := &cobra.Command{
		Use:   "set <name|uuid>",
		Short: "Patch a project's quota (omitted flags reuse the current value)",
		Args:  cobra.ExactArgs(1),
	}
	registerQuotaFlags(cmd, backing, false /* tenantScope */)
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
		if err != nil {
			return err
		}
		defer conn.Close()
		projectUUID, err := resolveProject(c, args[0])
		if err != nil {
			return err
		}
		cur, err := c.GetProjectQuota(context.Background(), &weftv1.GetProjectQuotaRequest{ProjectUuid: projectUUID})
		if err != nil {
			return err
		}
		q := cloneQuotas(cur.Project)
		applyQuotaFlags(cmd, backing, q, false /* tenantScope */)
		resp, err := c.SetProjectQuota(context.Background(), &weftv1.SetProjectQuotaRequest{
			ProjectUuid: projectUUID,
			Quota:       q,
		})
		if err != nil {
			return err
		}
		return renderProjectQuota(projectUUID, resp.Project, resp.TenantCap, resp.SiblingsTotal)
	}
	return cmd
}

// ---------------------------------------------------------------- shared

// registerQuotaFlags wires one Int32 flag per quotaDim onto cmd. The
// backing map points at each dim's int32 so applyQuotaFlags can read
// the value AFTER cobra has parsed the command line.
func registerQuotaFlags(cmd *cobra.Command, backing map[string]*int32, tenantScope bool) {
	for _, d := range quotaDims {
		if d.tenantOnly && !tenantScope {
			continue
		}
		var v int32
		cmd.Flags().Int32Var(&v, d.flag, 0, d.doc)
		// We capture &v in the map so applyQuotaFlags can read the
		// final value after parsing. Because v is freshly declared
		// each iteration, the closure-over-loop-var trap doesn't
		// bite — each flag has its own backing word.
		backing[d.flag] = &v
	}
}

// applyQuotaFlags walks quotaDims and patches q in place for every
// flag the operator actually passed on the command line. Unset
// flags leave q.<field> at its current value (so an unset --vcpu on
// `quota tenant set` reuses the tenant's existing vCPU cap).
//
// Pre-condition : cmd has already executed cobra's flag parsing.
// Inside RunE this is guaranteed.
func applyQuotaFlags(cmd *cobra.Command, backing map[string]*int32, q *weftv1.Quotas, tenantScope bool) {
	for _, d := range quotaDims {
		if d.tenantOnly && !tenantScope {
			continue
		}
		if !cmd.Flags().Changed(d.flag) {
			continue
		}
		ptr, ok := backing[d.flag]
		if !ok {
			continue
		}
		*d.fieldOf(q) = *ptr
	}
}

// cloneQuotas returns a fresh *Quotas carrying q's scalar fields.
// We construct field-by-field rather than dereference-and-copy so
// the proto message's embedded lock isn't aliased into a second
// value (go vet flags `copylocks` on `*q`-into-value moves).
func cloneQuotas(q *weftv1.Quotas) *weftv1.Quotas {
	if q == nil {
		return &weftv1.Quotas{}
	}
	return &weftv1.Quotas{
		Vcpu:        q.Vcpu,
		RamGib:      q.RamGib,
		Volumes:     q.Volumes,
		VolumesGib:  q.VolumesGib,
		Shares:      q.Shares,
		SharesGib:   q.SharesGib,
		Buckets:     q.Buckets,
		BucketsGib:  q.BucketsGib,
		RegistryGib: q.RegistryGib,
		FloatingIps: q.FloatingIps,
		Projects:    q.Projects,
	}
}

// resolveProject mirrors the project package's name → UUID
// resolution. Kept local to avoid a circular import — we can't
// import the `project` package because it'd risk a future import
// cycle if `project` ever wanted to call into `quota` for an
// inline "weft project quota" alias.
func resolveProject(c weftv1.WeftAgentClient, arg string) (string, error) {
	if looksLikeUUID(arg) {
		return arg, nil
	}
	resp, err := c.ListProjects(context.Background(), &weftv1.ListProjectsRequest{})
	if err != nil {
		return "", err
	}
	for _, p := range resp.Projects {
		if p.Name == arg {
			return p.Uuid, nil
		}
	}
	return "", fmt.Errorf("no project named %q (use `weft project ls` to inspect)", arg)
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

// renderTenantQuota draws a compact table showing each dimension's
// cap + allocated + headroom.
func renderTenantQuota(tenantUUID string, cap, allocated *weftv1.Quotas) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "tenant\t%s\n", tenantUUID)
	fmt.Fprintln(tw, "DIMENSION\tCAP\tALLOCATED\tHEADROOM")
	if cap == nil {
		cap = &weftv1.Quotas{}
	}
	if allocated == nil {
		allocated = &weftv1.Quotas{}
	}
	for _, d := range quotaDims {
		c := *d.fieldOf(cap)
		a := *d.fieldOf(allocated)
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\n", d.label, c, a, c-a)
	}
	return tw.Flush()
}

// renderProjectQuota draws project + sibling-total + tenant-cap so
// the operator sees at-a-glance whether headroom is project-local
// or shared with siblings.
func renderProjectQuota(projectUUID string, project, tenantCap, siblings *weftv1.Quotas) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "project\t%s\n", projectUUID)
	fmt.Fprintln(tw, "DIMENSION\tPROJECT\tSIBLINGS_TOTAL\tTENANT_CAP\tTENANT_HEADROOM")
	if project == nil {
		project = &weftv1.Quotas{}
	}
	if tenantCap == nil {
		tenantCap = &weftv1.Quotas{}
	}
	if siblings == nil {
		siblings = &weftv1.Quotas{}
	}
	for _, d := range quotaDims {
		if d.tenantOnly {
			continue
		}
		p := *d.fieldOf(project)
		s := *d.fieldOf(siblings)
		tc := *d.fieldOf(tenantCap)
		headroom := tc - (p + s)
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\n", d.label, p, s, tc, headroom)
	}
	return tw.Flush()
}
