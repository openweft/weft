// Package login implements `vzc login` and `vzc logout`. The login
// flow is RFC 8628 device authorisation against dex (or any other
// OIDC issuer that supports the device grant). Token cache lives
// in ~/.config/vzc/token.hcl (HCL per [[hcl-over-json]]).
package login

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/openweft/weft-client"
	"github.com/spf13/cobra"
)

// Command returns the cobra command group: `vzc login` + `vzc
// logout` plus a `vzc whoami` introspection helper.
func LoginCommand() *cobra.Command {
	var issuer string
	var clientID string
	var scopes []string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate via OIDC device flow (dex) and cache the token",
		RunE: func(_ *cobra.Command, _ []string) error {
			if issuer == "" {
				return fmt.Errorf("--issuer is required (e.g. https://dex.internal.example.com)")
			}
			if clientID == "" {
				clientID = "vzc"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			da, err := vzclient.DeviceAuth(ctx, issuer, clientID, scopes)
			if err != nil {
				return fmt.Errorf("device auth init: %w", err)
			}
			fmt.Fprintf(os.Stderr,
				"\nOpen this URL in your browser to authenticate:\n  %s\nAnd enter the code:\n  %s\n\n(Waiting for completion — Ctrl-C to abort.)\n\n",
				preferredURL(da), da.UserCode)
			tok, idToken, err := vzclient.PollDeviceToken(ctx, issuer, clientID, da)
			if err != nil {
				return fmt.Errorf("poll token: %w", err)
			}
			cached := vzclient.FromOAuth2(tok, issuer, clientID, idToken)
			if err := vzclient.SaveCachedToken(cached); err != nil {
				return fmt.Errorf("save token: %w", err)
			}
			fmt.Fprintf(os.Stderr, "Logged in. Token cached at %s (expires %s).\n",
				vzclient.TokenCachePath(), cached.ExpiresAt)
			return nil
		},
	}
	cmd.Flags().StringVar(&issuer, "issuer", os.Getenv("VZC_OIDC_ISSUER"), "OIDC issuer URL (default $VZC_OIDC_ISSUER)")
	cmd.Flags().StringVar(&clientID, "client-id", os.Getenv("VZC_OIDC_CLIENT_ID"), "OAuth client ID registered in dex (default 'vzc')")
	cmd.Flags().StringSliceVar(&scopes, "scope", nil, "OIDC scopes (default: openid profile email groups offline_access)")
	return cmd
}

// LogoutCommand returns `vzc logout` — drops the token cache.
func LogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Forget the cached OIDC token",
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := vzclient.DeleteCachedToken(); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "Logged out.")
			return nil
		},
	}
}

// WhoamiCommand returns `vzc whoami` — reads the cache and prints
// what the operator is currently authenticated as. Decodes the
// id_token client-side so we don't need an extra round-trip to
// dex's userinfo endpoint.
func WhoamiCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Print the cached OIDC identity (or 'anonymous' when not logged in)",
		RunE: func(_ *cobra.Command, _ []string) error {
			t, err := vzclient.LoadCachedToken()
			if err != nil {
				return err
			}
			if t == nil {
				fmt.Println("anonymous (no token cached)")
				return nil
			}
			fmt.Printf("issuer:    %s\n", t.Issuer)
			fmt.Printf("client_id: %s\n", t.ClientID)
			fmt.Printf("expires:   %s\n", t.ExpiresAt)
			if t.IDToken != "" {
				fmt.Printf("id_token:  %s…\n", t.IDToken[:min(40, len(t.IDToken))])
			}
			return nil
		},
	}
}

// preferredURL returns verification_uri_complete when the IdP
// supplies it (the user clicks one link and skips typing the code),
// falling back to the bare verification_uri otherwise.
func preferredURL(da *vzclient.DeviceAuthResponse) string {
	if da.VerificationURIComplete != "" {
		return da.VerificationURIComplete
	}
	return da.VerificationURI
}
