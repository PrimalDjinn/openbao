package approle

import (
	"maps"
	"testing"
	"time"

	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/stretchr/testify/require"
)

// loginCredentials creates roleName with roleData and returns a role ID and a
// fresh secret ID for it.
func loginCredentials(t *testing.T, b *backend, s logical.Storage, roleName string, roleData map[string]any) (string, string) {
	t.Helper()
	b.requestNoErr(t, &logical.Request{
		Operation: logical.CreateOperation, Path: "role/" + roleName, Storage: s, Data: roleData,
	})
	resp := b.requestNoErr(t, &logical.Request{
		Operation: logical.ReadOperation, Path: "role/" + roleName + "/role-id", Storage: s,
	})
	roleID := resp.Data["role_id"].(string)
	resp = b.requestNoErr(t, &logical.Request{
		Operation: logical.UpdateOperation, Path: "role/" + roleName + "/secret-id", Storage: s,
	})
	return roleID, resp.Data["secret_id"].(string)
}

func login(t *testing.T, b *backend, s logical.Storage, data map[string]any) *logical.Response {
	t.Helper()
	resp, err := b.HandleRequest(t.Context(), &logical.Request{
		Operation:  logical.UpdateOperation,
		Path:       "login",
		Storage:    s,
		Data:       data,
		Connection: &logical.Connection{RemoteAddr: "127.0.0.1"},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	return resp
}

func renew(t *testing.T, b *backend, s logical.Storage, auth *logical.Auth) *logical.Response {
	t.Helper()
	resp, err := b.HandleRequest(t.Context(), generateRenewRequest(s, auth))
	require.NoError(t, err)
	require.False(t, resp.IsError(), "%v", resp.Error())
	return resp
}

func TestAppRole_LoginRequestedTTL(t *testing.T) {
	b, s := createBackendWithStorage(t)
	roleID, secretID := loginCredentials(t, b, s, "ttl", map[string]any{
		"policies": "default", "token_ttl": 400, "token_max_ttl": 500,
	})
	creds := func(extra map[string]any) map[string]any {
		data := map[string]any{"role_id": roleID, "secret_id": secretID}
		maps.Copy(data, extra)
		return data
	}

	// Without a request the role's token_ttl applies, also on renewal.
	resp := login(t, b, s, creds(nil))
	require.False(t, resp.IsError(), "%v", resp.Error())
	require.Equal(t, 400*time.Second, resp.Auth.TTL)
	require.NotContains(t, resp.Auth.InternalData, requestedTTLKey)
	renewed := renew(t, b, s, resp.Auth)
	require.Equal(t, 400*time.Second, renewed.Auth.TTL)

	// A requested TTL replaces token_ttl and survives renewal.
	for _, ttl := range []any{60, "60s", "1m"} {
		resp = login(t, b, s, creds(map[string]any{"ttl": ttl}))
		require.False(t, resp.IsError(), "%v", resp.Error())
		require.Equal(t, time.Minute, resp.Auth.TTL, "ttl %v", ttl)
		require.Equal(t, 500*time.Second, resp.Auth.MaxTTL, "the role max still bounds it")
		renewed = renew(t, b, s, resp.Auth)
		require.Equal(t, time.Minute, renewed.Auth.TTL, "ttl %v", ttl)
		require.Equal(t, 500*time.Second, renewed.Auth.MaxTTL)
	}

	// A request can only shorten the token: anything above token_ttl is capped.
	resp = login(t, b, s, creds(map[string]any{"ttl": "1h"}))
	require.False(t, resp.IsError(), "%v", resp.Error())
	require.Equal(t, 400*time.Second, resp.Auth.TTL)
	require.Equal(t, 500*time.Second, resp.Auth.MaxTTL)
	require.Contains(t, resp.Warnings, "ttl was capped at the role's default token TTL of 6m40s")
	renewed = renew(t, b, s, resp.Auth)
	require.Equal(t, 400*time.Second, renewed.Auth.TTL)

	// An explicit zero means "not requested".
	resp = login(t, b, s, creds(map[string]any{"ttl": 0}))
	require.False(t, resp.IsError(), "%v", resp.Error())
	require.Equal(t, 400*time.Second, resp.Auth.TTL)
}

func TestAppRole_LoginRequestedTTL_NeverLengthensBatchTokens(t *testing.T) {
	b, s := createBackendWithStorage(t)
	roleID, secretID := loginCredentials(t, b, s, "batch", map[string]any{
		"policies": "default", "token_type": "batch", "token_ttl": 60, "token_max_ttl": 3600,
	})
	creds := func(ttl any) map[string]any {
		return map[string]any{"role_id": roleID, "secret_id": secretID, "ttl": ttl}
	}

	// Batch tokens cannot be revoked, so a request must never raise their TTL
	// above token_ttl, even though token_max_ttl would allow it.
	resp := login(t, b, s, creds("30m"))
	require.False(t, resp.IsError(), "%v", resp.Error())
	require.Equal(t, logical.TokenTypeBatch, resp.Auth.TokenType)
	require.Equal(t, time.Minute, resp.Auth.TTL)
	require.Contains(t, resp.Warnings, "ttl was capped at the role's default token TTL of 1m0s")

	resp = login(t, b, s, creds("30s"))
	require.False(t, resp.IsError(), "%v", resp.Error())
	require.Equal(t, 30*time.Second, resp.Auth.TTL)
	require.Empty(t, resp.Warnings)
}

func TestAppRole_LoginRequestedTTL_CappedAtMountDefault(t *testing.T) {
	// A role without token_ttl falls back to the mount's default lease TTL, which
	// is then the cap (24h in the test system view).
	b, s := createBackendWithStorage(t)
	roleID, secretID := loginCredentials(t, b, s, "plain", map[string]any{"policies": "default", "token_type": "batch"})
	limit := b.System().DefaultLeaseTTL()
	require.Positive(t, limit)

	resp := login(t, b, s, map[string]any{"role_id": roleID, "secret_id": secretID, "ttl": int((limit + time.Hour) / time.Second)})
	require.False(t, resp.IsError(), "%v", resp.Error())
	require.Equal(t, limit, resp.Auth.TTL)
	require.Len(t, resp.Warnings, 1)

	resp = login(t, b, s, map[string]any{"role_id": roleID, "secret_id": secretID, "ttl": "5m"})
	require.False(t, resp.IsError(), "%v", resp.Error())
	require.Equal(t, 5*time.Minute, resp.Auth.TTL)
}

func TestAppRole_LoginRequestedTTL_PeriodicRole(t *testing.T) {
	b, s := createBackendWithStorage(t)
	roleID, secretID := loginCredentials(t, b, s, "periodic", map[string]any{
		"policies": "default", "token_period": 600,
	})
	resp := login(t, b, s, map[string]any{"role_id": roleID, "secret_id": secretID, "ttl": 60})
	require.False(t, resp.IsError(), "%v", resp.Error())
	require.Equal(t, 600*time.Second, resp.Auth.Period)
	require.Zero(t, resp.Auth.TTL)
	require.NotContains(t, resp.Auth.InternalData, requestedTTLKey)
	require.Contains(t, resp.Warnings, "ttl was ignored because the role issues periodic tokens")

	renewed := renew(t, b, s, resp.Auth)
	require.Equal(t, 600*time.Second, renewed.Auth.Period)
}

func TestAppRole_LoginRequestedNumUses(t *testing.T) {
	b, s := createBackendWithStorage(t)
	unlimitedID, unlimitedSecret := loginCredentials(t, b, s, "unlimited", map[string]any{"policies": "default"})
	limitedID, limitedSecret := loginCredentials(t, b, s, "limited", map[string]any{"policies": "default", "token_num_uses": 5})

	resp := login(t, b, s, map[string]any{"role_id": unlimitedID, "secret_id": unlimitedSecret, "num_uses": 3})
	require.False(t, resp.IsError(), "%v", resp.Error())
	require.Equal(t, 3, resp.Auth.NumUses)

	resp = login(t, b, s, map[string]any{"role_id": limitedID, "secret_id": limitedSecret, "num_uses": 3})
	require.False(t, resp.IsError(), "%v", resp.Error())
	require.Equal(t, 3, resp.Auth.NumUses)

	resp = login(t, b, s, map[string]any{"role_id": limitedID, "secret_id": limitedSecret, "num_uses": 10})
	require.False(t, resp.IsError(), "%v", resp.Error())
	require.Equal(t, 5, resp.Auth.NumUses, "a request cannot raise token_num_uses")
	require.Contains(t, resp.Warnings, "num_uses was capped at the role's token_num_uses of 5")

	resp = login(t, b, s, map[string]any{"role_id": limitedID, "secret_id": limitedSecret})
	require.False(t, resp.IsError(), "%v", resp.Error())
	require.Equal(t, 5, resp.Auth.NumUses)
}

func TestAppRole_LoginRequestValidationKeepsSecretIDUse(t *testing.T) {
	b, s := createBackendWithStorage(t)
	roleID, secretID := loginCredentials(t, b, s, "single-use", map[string]any{
		"policies": "default", "secret_id_num_uses": 1,
	})
	for _, extra := range []map[string]any{{"ttl": -1}, {"num_uses": -1}} {
		data := map[string]any{"role_id": roleID, "secret_id": secretID}
		maps.Copy(data, extra)
		resp := login(t, b, s, data)
		require.True(t, resp.IsError(), "%v", extra)
	}

	// The rejected requests did not consume the single secret ID use.
	resp := login(t, b, s, map[string]any{"role_id": roleID, "secret_id": secretID, "ttl": "30s"})
	require.False(t, resp.IsError(), "%v", resp.Error())
	require.Equal(t, 30*time.Second, resp.Auth.TTL)
	_, err := b.HandleRequest(t.Context(), &logical.Request{
		Operation:  logical.UpdateOperation,
		Path:       "login",
		Storage:    s,
		Data:       map[string]any{"role_id": roleID, "secret_id": secretID},
		Connection: &logical.Connection{RemoteAddr: "127.0.0.1"},
	})
	require.ErrorIs(t, err, logical.ErrInvalidCredentials, "the secret ID is now used up")
}

func TestAppRole_LoginRequestedTTLFromStoredLease(t *testing.T) {
	// Lease entries round-trip InternalData through JSON, so the stored value
	// may come back as a json.Number or float64.
	for _, raw := range []any{int64(90), float64(90), "90", "90s"} {
		ttl, ok := loginRequestedTTL(&logical.Auth{InternalData: map[string]any{requestedTTLKey: raw}})
		require.True(t, ok, "%T", raw)
		require.Equal(t, 90*time.Second, ttl, "%T", raw)
	}
	_, ok := loginRequestedTTL(&logical.Auth{InternalData: map[string]any{}})
	require.False(t, ok)
}
