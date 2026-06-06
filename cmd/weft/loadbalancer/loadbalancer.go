// Package loadbalancer implements the `weft loadbalancer` subcommand
// group : CRUD over the LoadBalancer registry (project-scoped VIPs)
// plus atomic backend-pool replacement via `set-backends`. Tier-3
// CLI parity gap closed via proto v0.8.0.
//
//	weft loadbalancer ls [--project=<...>]
//	weft loadbalancer show <uuid>
//	weft loadbalancer create --project=<...> --name=<...> --listen=<addr> --protocol=<l4_tcp|l4_udp|l7_http|l7_https> [--backends=addr@weight,...]
//	weft loadbalancer update <uuid> [--name --listen --protocol]
//	weft loadbalancer set-backends <uuid> --backends=addr@weight,...
//	weft loadbalancer rm <uuid>
package loadbalancer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/openweft/weft/cmd/weft/shared"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// Command returns the `weft loadbalancer` cobra command group.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "loadbalancer",
		Short:   "Manage load balancers (L4 / L7 virtual ingress)",
		Aliases: []string{"lb"},
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		showCmd(socket, sshSocket, sshKey),
		createCmd(socket, sshSocket, sshKey),
		updateCmd(socket, sshSocket, sshKey),
		setBackendsCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, format string
	cmd := &cobra.Command{
		Use:   "ls",
		Aliases: []string{"list"},
		Short: "List load balancers",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListLoadBalancers(context.Background(), &weftv1.ListLoadBalancersRequest{Project: project})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON(resp.LoadBalancers)
			}
			return renderTable(resp.LoadBalancers)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Filter to one project (name or UUID)")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func showCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "show <uuid>",
		Short: "Show a single load balancer",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.GetLoadBalancer(context.Background(), &weftv1.GetLoadBalancerRequest{Uuid: args[0]})
			if err != nil {
				return err
			}
			return renderTable([]*weftv1.LoadBalancerInfo{resp.LoadBalancer})
		},
	}
}

func createCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, name, listen, protocol, backends string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a load balancer",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if project == "" || name == "" || listen == "" || protocol == "" {
				return fmt.Errorf("--project, --name, --listen, --protocol are required")
			}
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			bs, err := parseBackends(backends)
			if err != nil {
				return err
			}
			resp, err := c.CreateLoadBalancer(context.Background(), &weftv1.CreateLoadBalancerRequest{
				Project:    project,
				Name:       name,
				ListenAddr: listen,
				Protocol:   protocol,
				Backends:   bs,
			})
			if err != nil {
				return err
			}
			tag := "created"
			if !resp.Created {
				tag = "exists"
			}
			fmt.Printf("%s\t%s\t%s\t%s\t%s\n", tag, resp.LoadBalancer.Uuid, resp.LoadBalancer.Name, resp.LoadBalancer.Protocol, resp.LoadBalancer.ListenAddr)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project (name or UUID)")
	cmd.Flags().StringVar(&name, "name", "", "Load balancer name")
	cmd.Flags().StringVar(&listen, "listen", "", "VIP / listen address (host[:port] or CIDR)")
	cmd.Flags().StringVar(&protocol, "protocol", "", "l4_tcp | l4_udp | l7_http | l7_https")
	cmd.Flags().StringVar(&backends, "backends", "", "Comma-separated backend list (addr@weight, weight optional)")
	return cmd
}

func updateCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var name, listen, protocol string
	cmd := &cobra.Command{
		Use:   "update <uuid>",
		Short: "Patch a load balancer's mutable fields",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.UpdateLoadBalancer(context.Background(), &weftv1.UpdateLoadBalancerRequest{
				Uuid:       args[0],
				Name:       name,
				ListenAddr: listen,
				Protocol:   protocol,
			})
			if err != nil {
				return err
			}
			fmt.Printf("updated\t%s\t%s\n", resp.LoadBalancer.Uuid, resp.LoadBalancer.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Display name")
	cmd.Flags().StringVar(&listen, "listen", "", "VIP / listen address")
	cmd.Flags().StringVar(&protocol, "protocol", "", "l4_tcp | l4_udp | l7_http | l7_https")
	return cmd
}

func setBackendsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var backends string
	cmd := &cobra.Command{
		Use:   "set-backends <uuid>",
		Short: "Atomically replace the backend pool (GET-modify-PUT pattern)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			bs, err := parseBackends(backends)
			if err != nil {
				return err
			}
			resp, err := c.SetLoadBalancerBackends(context.Background(), &weftv1.SetLoadBalancerBackendsRequest{
				Uuid:     args[0],
				Backends: bs,
			})
			if err != nil {
				return err
			}
			fmt.Printf("backends\t%s\t%d\n", resp.LoadBalancer.Uuid, len(resp.LoadBalancer.Backends))
			return nil
		},
	}
	cmd.Flags().StringVar(&backends, "backends", "", "Comma-separated backend list (addr@weight ; empty list = drain)")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <uuid>",
		Short: "Delete a load balancer (refuses while a FIP still maps to it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.DeleteLoadBalancer(context.Background(), &weftv1.DeleteLoadBalancerRequest{Uuid: args[0]})
			if err != nil {
				if resp != nil && resp.BlockedByFips > 0 {
					return fmt.Errorf("delete refused : %d floating-ip(s) still mapped to this LB", resp.BlockedByFips)
				}
				return err
			}
			fmt.Println(resp.DeletedUuid)
			return nil
		},
	}
}

// parseBackends parses "addr@weight,addr@weight,..." into LBBackend
// proto messages. Weight defaults to 1 when omitted.
func parseBackends(s string) ([]*weftv1.LBBackend, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]*weftv1.LBBackend, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		addr, weightStr, ok := strings.Cut(p, "@")
		weight := int32(1)
		if ok && weightStr != "" {
			n, err := strconv.Atoi(weightStr)
			if err != nil {
				return nil, fmt.Errorf("bad backend weight %q : %w", weightStr, err)
			}
			weight = int32(n)
		}
		out = append(out, &weftv1.LBBackend{Address: strings.TrimSpace(addr), Weight: weight})
	}
	return out, nil
}

func renderTable(lbs []*weftv1.LoadBalancerInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tPROJECT\tNAME\tPROTOCOL\tLISTEN\tBACKENDS\tCREATED")
	for _, lb := range lbs {
		created := time.Unix(0, lb.CreatedAtUnixNs).UTC().Format(time.RFC3339)
		var bs []string
		for _, b := range lb.Backends {
			bs = append(bs, fmt.Sprintf("%s@%d", b.Address, b.Weight))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			lb.Uuid, lb.ProjectUuid, lb.Name, lb.Protocol, lb.ListenAddr, dashIf(strings.Join(bs, ",")), created)
	}
	return tw.Flush()
}

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dumpJSON(lbs []*weftv1.LoadBalancerInfo) error {
	type backend struct {
		Address string `json:"address"`
		Weight  int32  `json:"weight"`
	}
	type out struct {
		UUID       string    `json:"uuid"`
		Project    string    `json:"project_uuid"`
		Name       string    `json:"name"`
		ListenAddr string    `json:"listen_addr"`
		Protocol   string    `json:"protocol"`
		Backends   []backend `json:"backends,omitempty"`
		CreatedAt  string    `json:"created_at"`
	}
	flat := make([]out, len(lbs))
	for i, lb := range lbs {
		bs := make([]backend, len(lb.Backends))
		for j, b := range lb.Backends {
			bs[j] = backend{Address: b.Address, Weight: b.Weight}
		}
		flat[i] = out{
			UUID:       lb.Uuid,
			Project:    lb.ProjectUuid,
			Name:       lb.Name,
			ListenAddr: lb.ListenAddr,
			Protocol:   lb.Protocol,
			Backends:   bs,
			CreatedAt:  time.Unix(0, lb.CreatedAtUnixNs).UTC().Format(time.RFC3339Nano),
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
