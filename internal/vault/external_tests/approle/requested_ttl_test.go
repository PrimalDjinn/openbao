package approle

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/openbao/openbao/api/v2"
	"github.com/openbao/openbao/sdk/v2/logical"
	credAppRole "github.com/openbao/openbao/v2/internal/builtin/credential/approle"
	vaulthttp "github.com/openbao/openbao/v2/internal/http"
	"github.com/openbao/openbao/v2/internal/vault"
	"github.com/stretchr/testify/require"
)

// TestAppRole_RequestedTokenTTL checks, through the full request path, that a
// login's requested ttl never exceeds the role's token_max_ttl or the mount's
// max lease TTL, and that a short mount default applies when neither the
// request nor the role sets a TTL.
func TestAppRole_RequestedTokenTTL(t *testing.T) {
	cluster := vault.NewTestCluster(t, &vault.CoreConfig{
		CredentialBackends: map[string]logical.Factory{"approle": credAppRole.Factory},
	}, &vault.TestClusterOptions{HandlerFunc: vaulthttp.Handler, NumCores: 1})
	cluster.Start()
	defer cluster.Cleanup()
	vault.TestWaitActive(t, cluster.Cores[0].Core)
	client := cluster.Cores[0].Client
	client.SetToken(cluster.RootToken)

	require.NoError(t, client.Sys().EnableAuthWithOptions("approle", &api.EnableAuthOptions{Type: "approle"}))
	// Equivalent to: bao auth tune -default-lease-ttl=10m -max-lease-ttl=1h approle/
	require.NoError(t, client.Sys().TuneMount("auth/approle", api.MountConfigInput{
		DefaultLeaseTTL: "10m", MaxLeaseTTL: "1h",
	}))

	roles := map[string]map[string]any{
		"plain":   {"token_policies": "default"},
		"bounded": {"token_policies": "default", "token_ttl": "5m", "token_max_ttl": "15m"},
		"legacy":  {"token_policies": "default", "token_ttl": "8760h"},
		"batch":   {"token_policies": "default", "token_type": "batch"},
	}
	creds := map[string][2]string{}
	for name, data := range roles {
		_, err := client.Logical().Write("auth/approle/role/"+name, data)
		require.NoError(t, err)
		roleID, err := client.Logical().Read("auth/approle/role/" + name + "/role-id")
		require.NoError(t, err)
		secretID, err := client.Logical().Write("auth/approle/role/"+name+"/secret-id", nil)
		require.NoError(t, err)
		creds[name] = [2]string{roleID.Data["role_id"].(string), secretID.Data["secret_id"].(string)}
	}

	anonymous, err := client.Clone()
	require.NoError(t, err)
	anonymous.ClearToken()
	login := func(role string, extra map[string]any) (*api.Secret, error) {
		data := map[string]any{"role_id": creds[role][0], "secret_id": creds[role][1]}
		for k, v := range extra {
			data[k] = v
		}
		return anonymous.Logical().Write("auth/approle/login", data)
	}

	for _, tc := range []struct {
		name    string
		role    string
		extra   map[string]any
		ttl     int
		capped  bool
		renewal int // expected TTL after renew-self, 0 to skip
	}{
		{name: "mount default when nothing is set", role: "plain", ttl: 600},
		{name: "requested below mount max", role: "plain", extra: map[string]any{"ttl": "30m"}, ttl: 1800},
		{name: "requested above mount max is capped", role: "plain", extra: map[string]any{"ttl": "2h"}, ttl: 3600, capped: true},
		{name: "role token_ttl", role: "bounded", ttl: 300, renewal: 300},
		{name: "requested shorter than role token_ttl", role: "bounded", extra: map[string]any{"ttl": "1m"}, ttl: 60, renewal: 60},
		{name: "requested above role token_max_ttl is capped", role: "bounded", extra: map[string]any{"ttl": "20m"}, ttl: 900, capped: true},
		{name: "legacy long role is capped by mount max", role: "legacy", ttl: 3600, capped: true},
		{name: "legacy long role with requested ttl", role: "legacy", extra: map[string]any{"ttl": "10m"}, ttl: 600},
		{name: "batch token honors requested ttl", role: "batch", extra: map[string]any{"ttl": "2m"}, ttl: 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secret, err := login(tc.role, tc.extra)
			require.NoError(t, err)
			require.Equal(t, tc.ttl, secret.Auth.LeaseDuration)
			capWarning := slices.ContainsFunc(secret.Warnings, func(w string) bool { return strings.Contains(w, "capped") })
			require.Equal(t, tc.capped, capWarning, "warnings: %v", secret.Warnings)

			if tc.renewal > 0 {
				owner, err := client.Clone()
				require.NoError(t, err)
				owner.SetToken(secret.Auth.ClientToken)
				renewed, err := owner.Auth().Token().RenewSelf(0)
				require.NoError(t, err)
				require.Equal(t, tc.renewal, renewed.Auth.LeaseDuration)
			}
		})
	}

	t.Run("num_uses lowers the token use count", func(t *testing.T) {
		secret, err := login("plain", map[string]any{"num_uses": 2})
		require.NoError(t, err)
		lookup, err := client.Auth().Token().Lookup(secret.Auth.ClientToken)
		require.NoError(t, err)
		numUses, err := lookup.Data["num_uses"].(json.Number).Int64()
		require.NoError(t, err)
		require.EqualValues(t, 2, numUses)
	})

	t.Run("negative values are rejected", func(t *testing.T) {
		// Field validation rejects these before the login handler runs. Core
		// reports field validation errors on any login path as a 500 (from
		// the alias lookahead), which is upstream behavior.
		for _, extra := range []map[string]any{{"ttl": -60}, {"num_uses": -1}} {
			_, err := login("plain", extra)
			require.Error(t, err, "%v", extra)
			require.Contains(t, err.Error(), "negative", "%v", extra)
		}
	})
}
