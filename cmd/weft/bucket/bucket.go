// Package bucket implements the `weft bucket` subcommand group — S3
// bucket catalogue (data lives on versitygw / CubeFS objectnode ; the
// agent tracks credentials + a mutable policy JSON).
//
//	weft bucket ls            [--project P] [--format json]
//	weft bucket show          <uuid>
//	weft bucket create        --project P --name N --endpoint E ...
//	weft bucket rm            <uuid>
//	weft bucket policy get    <uuid>
//	weft bucket policy set    <uuid> <policy-json-or-@file>
package bucket

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/cmd/weft/shared"
	"github.com/spf13/cobra"
)

// Command returns the `weft bucket` parent command.
func Command(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bucket",
		Short: "Manage S3 bucket catalogue (data on versitygw / CubeFS objectnode)",
	}
	cmd.AddCommand(
		lsCmd(socket, sshSocket, sshKey),
		showCmd(socket, sshSocket, sshKey),
		createCmd(socket, sshSocket, sshKey),
		rmCmd(socket, sshSocket, sshKey),
		policyCmd(socket, sshSocket, sshKey),
	)
	return cmd
}

func lsCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, format string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List buckets",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.ListBuckets(context.Background(), &weftv1.ListBucketsRequest{Project: project})
			if err != nil {
				return err
			}
			if format == "json" {
				return dumpJSON(resp.Buckets)
			}
			return renderTable(resp.Buckets)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Limit to one project")
	cmd.Flags().StringVar(&format, "format", "", "Output format (json)")
	return cmd
}

func showCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "show <uuid>",
		Short: "Show one bucket (includes credentials and policy)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.GetBucket(context.Background(), &weftv1.GetBucketRequest{Uuid: args[0]})
			if err != nil {
				return err
			}
			return dumpJSON([]*weftv1.BucketInfo{resp.Bucket})
		},
	}
}

func createCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	var project, name, endpoint, region, accessKey, secretKey string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Register a new bucket (admin-only)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := c.CreateBucket(context.Background(), &weftv1.CreateBucketRequest{
				Project:         project,
				Name:            name,
				Endpoint:        endpoint,
				Region:          region,
				AccessKeyId:     accessKey,
				SecretAccessKey: secretKey,
			})
			if err != nil {
				return err
			}
			fmt.Printf("created\t%s\t%s\t%s\n", resp.Bucket.Uuid, resp.Bucket.Name, resp.Bucket.Endpoint)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project (name or UUID, required)")
	cmd.Flags().StringVar(&name, "name", "", "Bucket name (unique within the project, required)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "S3 endpoint (e.g. https://s3.example.com, required)")
	cmd.Flags().StringVar(&region, "region", "", "S3 region (e.g. us-east-1)")
	cmd.Flags().StringVar(&accessKey, "access-key-id", "", "S3 access key id")
	cmd.Flags().StringVar(&secretKey, "secret-access-key", "", "S3 secret access key")
	_ = cmd.MarkFlagRequired("project")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("endpoint")
	return cmd
}

func rmCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <uuid>",
		Short: "Delete a bucket (admin-only ; openweft side only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := c.DeleteBucket(context.Background(), &weftv1.DeleteBucketRequest{Uuid: args[0]}); err != nil {
				return err
			}
			fmt.Println(args[0])
			return nil
		},
	}
}

func policyCmd(socket, sshSocket, sshKey *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Get or set the bucket policy JSON",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "get <uuid>",
			Short: "Print the current bucket policy JSON",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
				if err != nil {
					return err
				}
				defer conn.Close()
				resp, err := c.GetBucketPolicy(context.Background(), &weftv1.GetBucketPolicyRequest{Uuid: args[0]})
				if err != nil {
					return err
				}
				fmt.Println(resp.Policy)
				return nil
			},
		},
		&cobra.Command{
			Use:   "set <uuid> <policy-json-or-@file>",
			Short: "Replace the bucket policy JSON (admin-only)",
			Args:  cobra.ExactArgs(2),
			RunE: func(_ *cobra.Command, args []string) error {
				c, conn, err := shared.Client(*socket, *sshSocket, *sshKey)
				if err != nil {
					return err
				}
				defer conn.Close()
				body := args[1]
				if strings.HasPrefix(body, "@") {
					b, err := os.ReadFile(body[1:])
					if err != nil {
						return fmt.Errorf("read policy file: %w", err)
					}
					body = string(b)
				}
				if _, err := c.SetBucketPolicy(context.Background(), &weftv1.SetBucketPolicyRequest{Uuid: args[0], Policy: body}); err != nil {
					return err
				}
				fmt.Printf("policy set\t%s\n", args[0])
				return nil
			},
		},
	)
	return cmd
}

func renderTable(buckets []*weftv1.BucketInfo) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UUID\tPROJECT_UUID\tNAME\tENDPOINT\tREGION\tCREATED")
	for _, b := range buckets {
		created := "-"
		if b.CreatedAtUnixNs != 0 {
			created = time.Unix(0, b.CreatedAtUnixNs).UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", b.Uuid, b.ProjectUuid, b.Name, b.Endpoint, dashIf(b.Region), created)
	}
	return tw.Flush()
}

func dashIf(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dumpJSON(buckets []*weftv1.BucketInfo) error {
	type out struct {
		UUID            string `json:"uuid"`
		ProjectUUID     string `json:"project_uuid"`
		Name            string `json:"name"`
		Endpoint        string `json:"endpoint"`
		Region          string `json:"region,omitempty"`
		AccessKeyID     string `json:"access_key_id,omitempty"`
		SecretAccessKey string `json:"secret_access_key,omitempty"`
		Policy          string `json:"policy,omitempty"`
		CreatedAt       string `json:"created_at,omitempty"`
	}
	flat := make([]out, len(buckets))
	for i, b := range buckets {
		var created string
		if b.CreatedAtUnixNs != 0 {
			created = time.Unix(0, b.CreatedAtUnixNs).UTC().Format(time.RFC3339Nano)
		}
		flat[i] = out{b.Uuid, b.ProjectUuid, b.Name, b.Endpoint, b.Region, b.AccessKeyId, b.SecretAccessKey, b.Policy, created}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(flat)
}
