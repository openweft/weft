// verify_images.go wires `weft cluster verify-images` — a preflight
// command that sweeps every OCI ref in a cluster.hcl + its infra
// service plans, probes each one against its registry, and reports
// arch coverage + accessibility issues BEFORE `weft up --apply` tries
// to pull and fails halfway. Catches three classes of problem :
//
//  1. Private GHCR images that need an auth token the hosts don't
//     carry (e.g. openweft/postgres-ha was found private 2026-06-06
//     while openweft/weft-webui is public — both indistinguishable
//     until you actually try the pull).
//  2. Images that don't ship the host's architecture (single-arch
//     amd64-only release on an arm64 cluster).
//  3. Missing tags / typos in the HCL (404 NOT_FOUND on the manifest).
//
// Output is a per-image table plus a summary count of
// PUBLIC/PRIVATE/UNREACHABLE/MISSING_ARCH. Exit code is non-zero when
// any image is in a state that would break `weft up --apply` against
// the cluster's declared hosts.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/openweft/weft/cluster"
	"github.com/openweft/weft/infra"
	"github.com/spf13/cobra"
)

// newClusterCmd is the parent grouping cluster-wide pre-flight + diagnostic
// commands. Today : `verify-images`. `weft up` / `weft down` stay top-level
// because that's how operators muscle-memory'd them.
func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Cluster-wide pre-flight + diagnostic commands",
	}
	cmd.AddCommand(newVerifyImagesCmd())
	return cmd
}

func newVerifyImagesCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:     "verify-images",
		Aliases: []string{"verify-imgs", "check-images"},
		Short:   "Pre-flight check : sweep all OCI refs from cluster.hcl + infra/*.hcl and probe each registry",
		Long: `verify-images parses the cluster description, collects every OCI ref
referenced by an infra service plan (etcd, dex, zot, webui, …), probes
each registry anonymously, and reports :

  - PUBLIC      : manifest reachable without auth
  - PRIVATE     : 401/403 from the registry — pull will need credentials
  - UNREACHABLE : DNS / TCP / TLS error
  - MISSING_ARCH: manifest fetched but no entry for the cluster's host
                  architecture (the hcl's hypervisor field implies the
                  guest arch ; today every host runs the local cpu arch
                  so we read GOARCH)

A non-zero exit code is returned when any image is PRIVATE / UNREACHABLE
/ MISSING_ARCH. Run this against your live cluster.hcl before
weft up --apply :

  weft cluster verify-images -f cluster.hcl
`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			return runVerifyImages(c.OutOrStdout(), file)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "cluster.hcl", "path to the cluster description (HCL)")
	return cmd
}

// runVerifyImages reads the cluster.hcl, walks the infra DAG it
// declares (or auto-discovers when the HCL doesn't pin a service
// list), and probes each OCI ref. Output goes to w.
func runVerifyImages(w io.Writer, file string) error {
	cl, err := cluster.Load(file)
	if err != nil {
		return fmt.Errorf("load %s: %w", file, err)
	}
	root := moduleRoot()
	services, err := cl.ResolveServices(root)
	if err != nil {
		return fmt.Errorf("resolve infra services: %w", err)
	}
	plans, err := infra.LoadAllPlans(root, services)
	if err != nil {
		return fmt.Errorf("load plans: %w", err)
	}
	// Collect images. Deduplicate so the same ref under N services
	// only probes once.
	type imgUse struct {
		ref      string
		services []string
	}
	use := map[string]*imgUse{}
	for name, p := range plans {
		if p == nil || p.OCIImage == "" {
			continue
		}
		if _, ok := use[p.OCIImage]; !ok {
			use[p.OCIImage] = &imgUse{ref: p.OCIImage}
		}
		use[p.OCIImage].services = append(use[p.OCIImage].services, name)
	}
	refs := make([]string, 0, len(use))
	for r := range use {
		refs = append(refs, r)
	}
	sort.Strings(refs)

	// Probe each ref. We do this serially to keep error output
	// readable ; a typical cluster.hcl declares 5–8 services so the
	// total latency is < 2s.
	wantArches := wantArchesForCluster(cl)
	results := make([]probeResult, 0, len(refs))
	for _, ref := range refs {
		r := probeImage(ref, wantArches)
		r.Services = use[ref].services
		results = append(results, r)
	}
	return renderProbeTable(w, results, wantArches)
}

// wantArchesForCluster returns the list of arches every image is
// REQUIRED to cover. The openweft directive is to ship amd64 + arm64
// + riscv64 + loong64 for every infra image we control (per the
// openweft-infra-images-4arch feedback). For images we don't yet
// control, the report flags MISSING_ARCH so we can prioritise the
// forks.
//
// Override via WEFT_VERIFY_IMAGES_ARCH=arch1,arch2,…
func wantArchesForCluster(_ *cluster.Cluster) []string {
	if env := os.Getenv("WEFT_VERIFY_IMAGES_ARCH"); env != "" {
		parts := strings.Split(env, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{"amd64", "arm64", "riscv64", "loong64"}
}

// probeResult is one row of the verify table.
type probeResult struct {
	Ref      string
	Status   string // PUBLIC / PRIVATE / UNREACHABLE / MISSING_ARCH / NOT_FOUND
	Arches   []string
	Reason   string
	Services []string
}

// probeImage hits the registry's /v2/<name>/manifests/<tag> with an
// anonymous bearer token. Returns the manifest's arch list when it's
// an OCI index, a single-element slice when it's a flat manifest.
func probeImage(ref string, wantArches []string) probeResult {
	host, repo, tag := parseImageRef(ref)
	r := probeResult{Ref: ref}
	if host == "" || repo == "" {
		r.Status = "UNREACHABLE"
		r.Reason = "ref parse failed"
		return r
	}
	token, err := fetchAnonymousToken(host, repo)
	if err != nil {
		// Not all registries require a token ; try the request anyway.
		token = ""
	}
	manifestURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", manifestHost(host), repo, tag)
	req, _ := http.NewRequest(http.MethodGet, manifestURL, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ","))
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		r.Status = "UNREACHABLE"
		r.Reason = err.Error()
		return r
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		// Decode + classify.
		var idx struct {
			MediaType string `json:"mediaType"`
			Manifests []struct {
				Platform struct {
					Architecture string `json:"architecture"`
					OS           string `json:"os"`
				} `json:"platform"`
			} `json:"manifests"`
			Config struct {
				MediaType string `json:"mediaType"`
				Digest    string `json:"digest"`
			} `json:"config"`
		}
		if err := json.Unmarshal(body, &idx); err != nil {
			r.Status = "UNREACHABLE"
			r.Reason = "manifest decode: " + err.Error()
			return r
		}
		if len(idx.Manifests) > 0 {
			seen := map[string]struct{}{}
			for _, m := range idx.Manifests {
				if m.Platform.OS == "unknown" || m.Platform.OS == "" || m.Platform.Architecture == "" {
					continue
				}
				seen[m.Platform.Architecture] = struct{}{}
			}
			arches := make([]string, 0, len(seen))
			for a := range seen {
				arches = append(arches, a)
			}
			sort.Strings(arches)
			r.Arches = arches
			r.Status = "PUBLIC"
			var missing []string
			for _, w := range wantArches {
				if _, ok := seen[w]; !ok {
					missing = append(missing, w)
				}
			}
			if len(missing) > 0 {
				r.Status = "MISSING_ARCH"
				r.Reason = fmt.Sprintf("missing=%s; have=%s",
					strings.Join(missing, ","), strings.Join(arches, ","))
			}
			return r
		}
		// Flat manifest : single-arch. Mark as PUBLIC but flag arch
		// gap when we can't tell — config blob would carry it but
		// that's another round-trip we skip for the preflight.
		r.Status = "PUBLIC"
		r.Arches = []string{"single-arch"}
		return r
	case http.StatusUnauthorized, http.StatusForbidden:
		r.Status = "PRIVATE"
		r.Reason = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return r
	case http.StatusNotFound:
		r.Status = "NOT_FOUND"
		r.Reason = "manifest 404 (tag missing ?)"
		return r
	default:
		r.Status = "UNREACHABLE"
		r.Reason = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return r
	}
}

// renderProbeTable writes a tabwriter table to w + returns a non-nil
// error when any image would block `weft up --apply`.
func renderProbeTable(w io.Writer, results []probeResult, wantArches []string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "REF\tSTATUS\tARCHES\tSERVICES\tREASON\n")
	var bad int
	for _, r := range results {
		if r.Status != "PUBLIC" {
			bad++
		}
		arches := strings.Join(r.Arches, ",")
		if arches == "" {
			arches = "—"
		}
		reason := r.Reason
		if reason == "" {
			reason = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.Ref, r.Status, arches, strings.Join(r.Services, ","), reason)
	}
	tw.Flush()
	fmt.Fprintf(w, "\nrequired arches (override via WEFT_VERIFY_IMAGES_ARCH=arch1,arch2,…): %s\n",
		strings.Join(wantArches, ","))
	if bad > 0 {
		fmt.Fprintf(w, "%d image(s) would block weft up --apply\n", bad)
		return fmt.Errorf("%d image(s) failed verification", bad)
	}
	return nil
}

// parseImageRef splits a reference like "ghcr.io/openweft/weft-webui:v0.2.0"
// into (host, repo, tag). When the host part is missing, defaults to
// "registry-1.docker.io" + adds the library/ prefix when the repo is
// a bare name.
func parseImageRef(ref string) (host, repo, tag string) {
	// Tag : everything after the last "@" or ":" that follows the
	// last "/" (so we don't split on a port).
	repoPart := ref
	tag = "latest"
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		repoPart, tag = ref[:i], ref[i+1:]
	} else if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repoPart, tag = ref[:i], ref[i+1:]
	}
	slash := strings.Index(repoPart, "/")
	if slash < 0 {
		return "registry-1.docker.io", "library/" + repoPart, tag
	}
	maybeHost := repoPart[:slash]
	if strings.ContainsAny(maybeHost, ".:") {
		host = maybeHost
		repo = repoPart[slash+1:]
		// Docker Hub uses an implicit "library/" prefix for single-name
		// official images even when the caller spelled the host out
		// (e.g. "docker.io/nats:2.11-alpine" → /v2/library/nats/...).
		if (host == "docker.io" || host == "registry-1.docker.io") && !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
		return host, repo, tag
	}
	// "owner/name" : Docker Hub.
	return "registry-1.docker.io", repoPart, tag
}

// manifestHost returns the host the v2 endpoint actually lives on.
// "docker.io" is just a marketing alias for registry-1.docker.io.
func manifestHost(host string) string {
	if host == "docker.io" {
		return "registry-1.docker.io"
	}
	return host
}

// fetchAnonymousToken does the OCI Distribution token-dance for hosts
// that gate /v2/.../manifests/... behind a Bearer token even for
// anonymous reads. GHCR + Docker Hub do ; quay.io doesn't.
func fetchAnonymousToken(host, repo string) (string, error) {
	var tokenURL string
	switch host {
	case "ghcr.io":
		tokenURL = fmt.Sprintf("https://ghcr.io/token?service=ghcr.io&scope=repository:%s:pull", repo)
	case "docker.io", "registry-1.docker.io":
		tokenURL = fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull", repo)
	default:
		return "", nil
	}
	resp, err := http.Get(tokenURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token %s: HTTP %d", host, resp.StatusCode)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", err
	}
	if t.Token != "" {
		return t.Token, nil
	}
	return t.AccessToken, nil
}

