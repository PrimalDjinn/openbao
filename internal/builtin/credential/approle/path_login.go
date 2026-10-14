// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package approle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/go-secure-stdlib/parseutil"
	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/helper/cidrutil"
	"github.com/openbao/openbao/sdk/v2/logical"
)

func pathLogin(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "login$",
		DisplayAttrs: &framework.DisplayAttributes{
			OperationPrefix: operationPrefixAppRole,
			OperationVerb:   "login",
		},
		Fields: map[string]*framework.FieldSchema{
			"role_id": {
				Type:        framework.TypeString,
				Description: "Unique identifier of the Role. Required to be supplied when the 'bind_secret_id' constraint is set.",
			},
			"secret_id": {
				Type:        framework.TypeString,
				Default:     "",
				Description: "SecretID belong to the App role",
			},
			"ttl": {
				Type: framework.TypeDurationSecond,
				Description: `Optional shorter TTL for the issued token. It can only lower the role's
token_ttl (or the mount's default lease TTL when the role sets none), so it
never exceeds the role's token_max_ttl, the mount's max lease TTL or the
system max lease TTL. Ignored when the role issues periodic tokens.`,
			},
			"num_uses": {
				Type: framework.TypeInt,
				Description: `Optional number of times the issued token may be used. It may lower,
but never raise, the role's token_num_uses.`,
			},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.pathLoginUpdate,
				Responses: map[int][]framework.Response{
					http.StatusOK: {{
						Description: http.StatusText(http.StatusOK),
					}},
				},
			},
			logical.AliasLookaheadOperation: &framework.PathOperation{
				Callback: b.pathLoginUpdateAliasLookahead,
				Responses: map[int][]framework.Response{
					http.StatusOK: {{
						Description: http.StatusText(http.StatusOK),
					}},
				},
			},
			logical.ResolveRoleOperation: &framework.PathOperation{
				Callback: b.pathLoginResolveRole,
				Responses: map[int][]framework.Response{
					http.StatusOK: {{
						Description: http.StatusText(http.StatusOK),
						Fields: map[string]*framework.FieldSchema{
							"role": {
								Type:     framework.TypeString,
								Required: true,
							},
						},
					}},
				},
			},
		},
		HelpSynopsis:    pathLoginHelpSys,
		HelpDescription: pathLoginHelpDesc,
	}
}

func (b *backend) pathLoginUpdateAliasLookahead(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	roleID := strings.TrimSpace(data.Get("role_id").(string))
	if roleID == "" {
		return nil, errors.New("missing role_id")
	}

	return &logical.Response{
		Auth: &logical.Auth{
			Alias: &logical.Alias{
				Name: roleID,
			},
		},
	}, nil
}

func (b *backend) pathLoginResolveRole(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	txRollback, err := logical.StartTxStorage(ctx, req)
	if err != nil {
		return nil, err
	}
	defer txRollback()

	// RoleID must be supplied during every login
	roleID := strings.TrimSpace(data.Get("role_id").(string))
	if roleID == "" {
		return logical.ErrorResponse("missing role_id"), nil
	}

	// Look for the storage entry that maps the roleID to role
	roleIDIndex, err := b.roleIDEntry(ctx, req.Storage, roleID)
	if err != nil {
		return nil, err
	}
	if roleIDIndex == nil {
		return logical.ErrorResponse("invalid role or secret ID"), nil
	}

	roleName := roleIDIndex.Name

	roleLock := b.roleLock(roleName)
	roleLock.RLock()

	role, err := b.roleEntry(ctx, req.Storage, roleName)
	roleLock.RUnlock()
	if err != nil {
		return nil, err
	}
	if role == nil {
		return logical.ErrorResponse("invalid role or secret ID"), nil
	}

	if err := logical.EndTxStorage(ctx, req); err != nil {
		return nil, err
	}

	return logical.ResolveRoleResponse(roleName)
}

// Returns the Auth object indicating the authentication and authorization information
// if the credentials provided are validated by the backend.
func (b *backend) pathLoginUpdate(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	txRollback, err := logical.StartTxStorage(ctx, req)
	if err != nil {
		return nil, err
	}
	defer txRollback()

	// RoleID must be supplied during every login
	roleID := strings.TrimSpace(data.Get("role_id").(string))
	if roleID == "" {
		return logical.ErrorResponse("missing role_id"), nil
	}

	// Validate requested token limits before any secret ID use is consumed.
	requestedTTL := time.Duration(data.Get("ttl").(int)) * time.Second
	requestedNumUses := data.Get("num_uses").(int)
	if requestedTTL < 0 {
		return logical.ErrorResponse("ttl cannot be negative"), nil
	}
	if requestedNumUses < 0 {
		return logical.ErrorResponse("num_uses cannot be negative"), nil
	}

	// Look for the storage entry that maps the roleID to role
	roleIDIndex, err := b.roleIDEntry(ctx, req.Storage, roleID)
	if err != nil {
		return nil, err
	}
	if roleIDIndex == nil {
		return logical.ErrorResponse("invalid role or secret ID"), nil
	}

	roleName := roleIDIndex.Name

	roleLock := b.roleLock(roleName)
	roleLock.RLock()

	role, err := b.roleEntry(ctx, req.Storage, roleName)
	roleLock.RUnlock()
	if err != nil {
		return nil, err
	}
	if role == nil {
		return logical.ErrorResponse("invalid role or secret ID"), nil
	}

	metadata := make(map[string]string)
	var entry *secretIDStorageEntry
	if role.BindSecretID {
		secretID := strings.TrimSpace(data.Get("secret_id").(string))
		if secretID == "" {
			return logical.ErrorResponse("missing secret_id"), nil
		}

		secretIDHMAC, err := createHMAC(role.HMACKey, secretID)
		if err != nil {
			return nil, fmt.Errorf("failed to create HMAC of secret_id: %w", err)
		}

		roleNameHMAC, err := createHMAC(role.HMACKey, role.name)
		if err != nil {
			return nil, fmt.Errorf("failed to create HMAC of role_name: %w", err)
		}

		entryIndex := fmt.Sprintf("%s%s/%s", role.SecretIDPrefix, roleNameHMAC, secretIDHMAC)

		secretIDLock := b.secretIDLock(secretIDHMAC)
		secretIDLock.RLock()

		unlockFunc := secretIDLock.RUnlock
		defer func() {
			unlockFunc()
		}()

		entry, err = b.nonLockedSecretIDStorageEntry(ctx, req.Storage, role.SecretIDPrefix, roleNameHMAC, secretIDHMAC)
		if err != nil {
			return nil, err
		}
		if entry == nil || entry.expired() {
			return logical.ErrorResponse("invalid role or secret ID"), logical.ErrInvalidCredentials
		}

		// If a secret ID entry does not have a corresponding accessor
		// entry, revoke the secret ID immediately
		accessorEntry, err := b.secretIDAccessorEntry(ctx, req.Storage, entry.SecretIDAccessor, role.SecretIDPrefix)
		if err != nil {
			return nil, fmt.Errorf("failed to read secret ID accessor entry: %w", err)
		}
		if accessorEntry == nil {
			// Switch the locks and recheck the conditions
			secretIDLock.RUnlock()
			secretIDLock.Lock()
			unlockFunc = secretIDLock.Unlock

			entry, err = b.nonLockedSecretIDStorageEntry(ctx, req.Storage, role.SecretIDPrefix, roleNameHMAC, secretIDHMAC)
			if err != nil {
				return nil, err
			}
			if entry == nil {
				return logical.ErrorResponse("invalid role or secret ID"), nil
			}

			accessorEntry, err := b.secretIDAccessorEntry(ctx, req.Storage, entry.SecretIDAccessor, role.SecretIDPrefix)
			if err != nil {
				return nil, fmt.Errorf("failed to read secret ID accessor entry: %w", err)
			}

			if accessorEntry == nil {
				if err := req.Storage.Delete(ctx, entryIndex); err != nil {
					return nil, fmt.Errorf("error deleting secret ID %q from storage: %w", secretIDHMAC, err)
				}
			}
			return logical.ErrorResponse("invalid role or secret ID"), nil
		}

		switch entry.SecretIDNumUses {
		case 0:
			//
			// SecretIDNumUses will be zero only if the usage limit was not set at all,
			// in which case, the SecretID will remain to be valid as long as it is not
			// expired.
			//

			// Ensure that the CIDRs on the secret ID are still a subset of that of
			// role's
			err = verifyCIDRRoleSecretIDSubset(entry.CIDRList, role.SecretIDBoundCIDRs)
			if err != nil {
				return nil, err
			}

			// If CIDR restrictions are present on the secret ID, check if the
			// source IP complies to it
			if len(entry.CIDRList) != 0 {
				if req.Connection == nil || req.Connection.RemoteAddr == "" {
					return nil, errors.New("failed to get connection information")
				}

				belongs, err := cidrutil.IPBelongsToCIDRBlocksSlice(req.Connection.RemoteAddr, entry.CIDRList)
				if err != nil {
					return logical.ErrorResponse(err.Error()), logical.ErrInvalidRequest
				}

				if !belongs {
					return logical.ErrorResponse(fmt.Errorf(
						"source address %q unauthorized through CIDR restrictions on the secret ID",
						req.Connection.RemoteAddr,
					).Error()), nil
				}
			}
		default:
			//
			// If the SecretIDNumUses is non-zero, it means that its use-count should be updated
			// in the storage. Switch the lock from a `read` to a `write` and update
			// the storage entry.
			//

			secretIDLock.RUnlock()
			secretIDLock.Lock()
			unlockFunc = secretIDLock.Unlock

			// Lock switching may change the data. Refresh the contents.
			entry, err = b.nonLockedSecretIDStorageEntry(ctx, req.Storage, role.SecretIDPrefix, roleNameHMAC, secretIDHMAC)
			if err != nil {
				return nil, err
			}
			if entry == nil {
				return logical.ErrorResponse("invalid secret_id %q", secretID), nil
			}

			// If there exists a single use left, delete the SecretID entry from
			// the storage but do not fail the validation request. Subsequent
			// requests to use the same SecretID will fail.
			if entry.SecretIDNumUses == 1 {
				// Delete the secret IDs accessor first
				err = b.deleteSecretIDAccessorEntry(ctx, req.Storage, entry.SecretIDAccessor, role.SecretIDPrefix)
				if err != nil {
					return nil, err
				}
				err = req.Storage.Delete(ctx, entryIndex)
				if err != nil {
					return nil, fmt.Errorf("failed to delete secret ID: %w", err)
				}
			} else {
				// If the use count is greater than one, decrement it and update the last updated time.
				entry.SecretIDNumUses -= 1
				entry.LastUpdatedTime = time.Now()

				sEntry, err := logical.StorageEntryJSON(entryIndex, &entry)
				if err != nil {
					return nil, err
				}

				err = req.Storage.Put(ctx, sEntry)
				if err != nil {
					return nil, err
				}
			}

			// Ensure that the CIDRs on the secret ID are still a subset of that of
			// role's
			err = verifyCIDRRoleSecretIDSubset(entry.CIDRList, role.SecretIDBoundCIDRs)
			if err != nil {
				return nil, err
			}

			// If CIDR restrictions are present on the secret ID, check if the
			// source IP complies to it
			if len(entry.CIDRList) != 0 {
				if req.Connection == nil || req.Connection.RemoteAddr == "" {
					return nil, errors.New("failed to get connection information")
				}

				belongs, err := cidrutil.IPBelongsToCIDRBlocksSlice(req.Connection.RemoteAddr, entry.CIDRList)
				if err != nil || !belongs {
					return logical.ErrorResponse(
						fmt.Errorf(
							"source address %q unauthorized by CIDR restrictions on the secret ID: %w",
							req.Connection.RemoteAddr,
							err,
						).Error(),
					), nil
				}
			}
		}

		metadata = entry.Metadata
	}

	if len(role.SecretIDBoundCIDRs) != 0 {
		if req.Connection == nil || req.Connection.RemoteAddr == "" {
			return nil, errors.New("failed to get connection information")
		}
		belongs, err := cidrutil.IPBelongsToCIDRBlocksSlice(req.Connection.RemoteAddr, role.SecretIDBoundCIDRs)
		if err != nil || !belongs {
			return logical.ErrorResponse(
				fmt.Errorf(
					"source address %q unauthorized by CIDR restrictions on the role: %w",
					req.Connection.RemoteAddr,
					err,
				).Error(),
			), nil
		}
	}

	// Parse the CIDRs we should be binding the token to.
	tokenBoundCIDRs := role.TokenBoundCIDRs
	if entry != nil && len(entry.TokenBoundCIDRs) > 0 {
		tokenBoundCIDRs, err = parseutil.ParseAddrs(entry.TokenBoundCIDRs)
		if err != nil {
			return logical.ErrorResponse(err.Error()), nil
		}
	}

	// For some reason, if metadata was set to nil while processing secret ID
	// binding, ensure that it is initialized again to avoid a panic.
	if metadata == nil {
		metadata = make(map[string]string)
	}

	// Always include the role name, for later filtering
	metadata["role_name"] = role.name

	auth := &logical.Auth{
		InternalData: map[string]any{
			"role_name": role.name,
		},
		Metadata: metadata,
		Alias: &logical.Alias{
			Name:     role.RoleID,
			Metadata: metadata,
		},
	}
	if err := role.PopulateTokenAuth(auth, req); err != nil {
		return nil, fmt.Errorf("failed to populate auth information: %w", err)
	}

	// Allow for overridden token bound CIDRs
	auth.BoundCIDRs = tokenBoundCIDRs

	resp := &logical.Response{Auth: auth}
	for _, warning := range applyRequestedTokenLimits(auth, requestedTTL, requestedNumUses, b.System().DefaultLeaseTTL()) {
		resp.AddWarning(warning)
	}

	if err := logical.EndTxStorage(ctx, req); err != nil {
		return nil, err
	}

	return resp, nil
}

// requestedTTLKey records a login's requested TTL in the token's internal data
// so that renewals keep it instead of reverting to the role's token_ttl.
const requestedTTLKey = "requested_ttl"

// applyRequestedTokenLimits applies the optional ttl and num_uses of a login
// request. Both can only narrow the token: the TTL is capped at the role's
// default (token_ttl, or the mount's default lease TTL when the role sets
// none), and the use count at token_num_uses. A request therefore never
// lengthens a token, which matters for batch tokens: they cannot be revoked.
// Core still applies token_max_ttl, the mount's max lease TTL and the system
// max on top.
func applyRequestedTokenLimits(auth *logical.Auth, ttl time.Duration, numUses int, mountDefaultTTL time.Duration) []string {
	var warnings []string
	if ttl > 0 {
		if auth.Period > 0 {
			warnings = append(warnings, "ttl was ignored because the role issues periodic tokens")
		} else {
			limit := auth.TTL
			if limit <= 0 {
				limit = mountDefaultTTL
			}
			if limit > 0 && ttl > limit {
				ttl = limit
				warnings = append(warnings, fmt.Sprintf("ttl was capped at the role's default token TTL of %s", limit))
			}
			auth.TTL = ttl
			auth.InternalData[requestedTTLKey] = int64(ttl / time.Second)
		}
	}
	if numUses > 0 {
		if auth.NumUses == 0 || numUses <= auth.NumUses {
			auth.NumUses = numUses
		} else {
			warnings = append(warnings, fmt.Sprintf("num_uses was capped at the role's token_num_uses of %d", auth.NumUses))
		}
	}
	return warnings
}

// loginRequestedTTL returns the TTL a token's login requested, if any.
func loginRequestedTTL(auth *logical.Auth) (time.Duration, bool) {
	raw, ok := auth.InternalData[requestedTTLKey]
	if !ok {
		return 0, false
	}
	ttl, err := parseutil.ParseDurationSecond(raw)
	return ttl, err == nil && ttl > 0
}

// Invoked when the token issued by this backend is attempting a renewal.
func (b *backend) pathLoginRenew(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	roleName := req.Auth.InternalData["role_name"].(string)
	if roleName == "" {
		return nil, errors.New("failed to fetch role_name during renewal")
	}

	lock := b.roleLock(roleName)
	lock.RLock()
	defer lock.RUnlock()

	// Ensure that the Role still exists.
	role, err := b.roleEntry(ctx, req.Storage, roleName)
	if err != nil {
		return nil, fmt.Errorf("failed to validate role %q during renewal: %w", roleName, err)
	}
	if role == nil {
		return nil, fmt.Errorf("role %q does not exist during renewal", roleName)
	}

	resp := &logical.Response{Auth: req.Auth}
	resp.Auth.TTL = role.TokenTTL
	if ttl, ok := loginRequestedTTL(req.Auth); ok && role.TokenPeriod == 0 {
		resp.Auth.TTL = ttl
	}
	resp.Auth.MaxTTL = role.TokenMaxTTL
	resp.Auth.Period = role.TokenPeriod
	return resp, nil
}

const pathLoginHelpSys = "Issue a token based on the credentials supplied"

const pathLoginHelpDesc = `
While the credential 'role_id' is required at all times,
other credentials required depends on the properties App role
to which the 'role_id' belongs to. The 'bind_secret_id'
constraint (enabled by default) on the App role requires the
'secret_id' credential to be presented.

'role_id' is fetched using the 'role/<role_name>/role_id'
endpoint and 'secret_id' is fetched using the 'role/<role_name>/secret_id'
endpoint.`
