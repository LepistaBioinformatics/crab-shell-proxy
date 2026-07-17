// Package authz resolves the caller's administrative tier from the mycelium
// profile and answers the admin-shared-content authorization questions. This is
// the "authority-over-target" check (is the caller's tier at/above the target
// scope, within the same branch) — a distinct shape from the self-scoped chat
// chain (design AD).
package authz

import (
	mycelium "github.com/LepistaBioinformatics/mycelium-sdk-go"
)

// Tier is the caller's authority level, highest last so comparisons read as
// "at or above" (>=).
type Tier int

const (
	// TierNone: no administrative authority over the target.
	TierNone Tier = iota
	// TierSubscription: subscriptions-manager on the target subscription.
	TierSubscription
	// TierTenant: tenant-owner/tenant-manager on the target tenant.
	TierTenant
	// TierInstance: instance staff/manager (authoritative over all branches).
	TierInstance
)

// Role slugs are the SystemActor Display/str forms verified in the gateway
// (mycelium-api-gateway/core/src/domain/actors/mod.rs).
const (
	roleTenantOwner          = "tenant-owner"
	roleTenantManager        = "tenant-manager"
	roleSubscriptionsManager = "subscriptions-manager"
)

// CallerTier resolves the caller's authority over the (tenantID, subsAccID)
// target from the injected profile (FR-1.1). Instance short-circuits every
// branch; Tenant matches the tenant only (so a tenant manager is authoritative
// over every subscription under it); Subscription requires the exact account.
// It returns the highest matching tier.
func CallerTier(p *mycelium.Profile, tenantID, subsAccID string) Tier {
	if p == nil {
		return TierNone
	}
	if p.HasAdminPrivileges() {
		return TierInstance
	}
	if p.LicensedResources == nil {
		return TierNone
	}
	best := TierNone
	for _, r := range p.LicensedResources.ToLicensesVector() {
		if r.TenantID.String() != tenantID {
			continue
		}
		switch r.Role {
		case roleTenantOwner, roleTenantManager:
			if TierTenant > best {
				best = TierTenant
			}
		case roleSubscriptionsManager:
			if subsAccID != "" && r.AccID.String() == subsAccID && TierSubscription > best {
				best = TierSubscription
			}
		}
	}
	return best
}

// AuthorizeSharedScope reports whether the caller may administer (view / list /
// upload / edit / delete) the shared content at the given scope: tenant scope
// needs tier >= Tenant on T; subscription scope needs tier >= Subscription in
// the T/S branch (FR-2/FR-3).
func AuthorizeSharedScope(p *mycelium.Profile, kind, tenantID, subsAccID string) bool {
	switch kind {
	case "tenant":
		return CallerTier(p, tenantID, "") >= TierTenant
	case "subscription":
		return CallerTier(p, tenantID, subsAccID) >= TierSubscription
	default:
		return false
	}
}

// AuthorizeUserManagement reports whether the caller is strictly above an end
// user in the (T, S) branch — Subscription tier of S, Tenant tier of T, or
// Instance — so may list metadata / delete that user's private files. It never
// grants content read or edit (FR-6/FR-7; there is no such endpoint).
func AuthorizeUserManagement(p *mycelium.Profile, tenantID, subsAccID string) bool {
	return CallerTier(p, tenantID, subsAccID) >= TierSubscription
}
