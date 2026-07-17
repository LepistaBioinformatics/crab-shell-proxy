package authz

import (
	"testing"

	mycelium "github.com/LepistaBioinformatics/mycelium-sdk-go"
	"github.com/google/uuid"
)

const (
	tenantT = "11111111-1111-1111-1111-111111111111"
	tenantU = "33333333-3333-3333-3333-333333333333"
	subsX   = "22222222-2222-2222-2222-222222222222"
	subsY   = "22222222-2222-2222-2222-999999999999"
)

// profileWith builds a profile carrying a single licensed record.
func profileWith(tenantID, accID, role string) *mycelium.Profile {
	return &mycelium.Profile{
		LicensedResources: &mycelium.LicensedResources{
			Records: []mycelium.LicensedResource{{
				TenantID: uuid.MustParse(tenantID),
				AccID:    uuid.MustParse(accID),
				Role:     role,
				Perm:     mycelium.PermissionWrite,
			}},
		},
	}
}

func staffProfile() *mycelium.Profile { return &mycelium.Profile{IsStaff: true} }

func TestCallerTier(t *testing.T) {
	cases := []struct {
		name             string
		p                *mycelium.Profile
		tenant, subs     string
		want             Tier
	}{
		{"nil", nil, tenantT, subsX, TierNone},
		{"staff-instance", staffProfile(), tenantT, subsX, TierInstance},
		{"tenant-owner", profileWith(tenantT, subsX, "tenant-owner"), tenantT, subsX, TierTenant},
		{"tenant-manager", profileWith(tenantT, subsX, "tenant-manager"), tenantT, subsX, TierTenant},
		{"tenant-manager-any-subs", profileWith(tenantT, subsX, "tenant-manager"), tenantT, subsY, TierTenant},
		{"tenant-manager-tenant-only", profileWith(tenantT, subsX, "tenant-manager"), tenantT, "", TierTenant},
		{"subs-manager-match", profileWith(tenantT, subsX, "subscriptions-manager"), tenantT, subsX, TierSubscription},
		{"subs-manager-wrong-subs", profileWith(tenantT, subsX, "subscriptions-manager"), tenantT, subsY, TierNone},
		{"subs-manager-no-subs", profileWith(tenantT, subsX, "subscriptions-manager"), tenantT, "", TierNone},
		{"wrong-tenant", profileWith(tenantU, subsX, "tenant-owner"), tenantT, subsX, TierNone},
		{"guest-role", profileWith(tenantT, subsX, "alpha"), tenantT, subsX, TierNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CallerTier(c.p, c.tenant, c.subs); got != c.want {
				t.Errorf("CallerTier = %d, want %d", got, c.want)
			}
		})
	}
}

func TestAuthorizeSharedScope(t *testing.T) {
	cases := []struct {
		name                    string
		p                       *mycelium.Profile
		kind, tenant, subs      string
		want                    bool
	}{
		{"instance-tenant", staffProfile(), "tenant", tenantT, "", true},
		{"instance-subs", staffProfile(), "subscription", tenantT, subsX, true},
		{"tenant-owner-tenant", profileWith(tenantT, subsX, "tenant-owner"), "tenant", tenantT, "", true},
		{"tenant-owner-subs", profileWith(tenantT, subsX, "tenant-owner"), "subscription", tenantT, subsX, true},
		{"subs-manager-tenant-DENY", profileWith(tenantT, subsX, "subscriptions-manager"), "tenant", tenantT, "", false},
		{"subs-manager-subs", profileWith(tenantT, subsX, "subscriptions-manager"), "subscription", tenantT, subsX, true},
		{"subs-manager-wrong-subs-DENY", profileWith(tenantT, subsX, "subscriptions-manager"), "subscription", tenantT, subsY, false},
		{"guest-DENY", profileWith(tenantT, subsX, "alpha"), "subscription", tenantT, subsX, false},
		{"bad-kind-DENY", staffProfile(), "nonsense", tenantT, subsX, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AuthorizeSharedScope(c.p, c.kind, c.tenant, c.subs); got != c.want {
				t.Errorf("AuthorizeSharedScope = %v, want %v", got, c.want)
			}
		})
	}
}

// TestAuthorizeUserManagement is "strictly above the user": Subscription tier
// or above in the branch. It never depends on any content-read capability
// (FR-7 is enforced by the absence of a content endpoint, not by this gate).
func TestAuthorizeUserManagement(t *testing.T) {
	cases := []struct {
		name string
		p    *mycelium.Profile
		want bool
	}{
		{"instance", staffProfile(), true},
		{"tenant-manager", profileWith(tenantT, subsX, "tenant-manager"), true},
		{"subs-manager", profileWith(tenantT, subsX, "subscriptions-manager"), true},
		{"wrong-subs", profileWith(tenantT, subsY, "subscriptions-manager"), false},
		{"guest", profileWith(tenantT, subsX, "alpha"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AuthorizeUserManagement(c.p, tenantT, subsX); got != c.want {
				t.Errorf("AuthorizeUserManagement = %v, want %v", got, c.want)
			}
		})
	}
}
