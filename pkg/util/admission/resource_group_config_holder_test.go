// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package admission

import (
	"math"
	"reflect"
	"sync"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/stretchr/testify/require"
)

// testHolder constructs a holder backed by a fresh test cluster
// settings.Values. The holder requires non-nil settings for the
// snapshot's mode and utilization-target reads.
func testHolder() *ResourceGroupConfigHolder {
	st := cluster.MakeTestingClusterSettings()
	return newResourceGroupConfigHolder(&st.SV)
}

// TestResourceGroupConfigHolder covers the constructor seed and the
// unknown-key fallback. The behavioral cases for Set and Snapshot live in
// adjacent TestResourceGroupConfigHolder* functions.
func TestResourceGroupConfigHolder(t *testing.T) {
	t.Run("constructor_seed", func(t *testing.T) {
		h := testHolder()
		snap := h.Snapshot()
		// All built-in configs are present.
		require.Equal(t, systemTenantGroupConfig, snap.Groups[tenantGroupKey(1)])
		require.Equal(t,
			builtinGroupConfigs[rgGroupKey(highResourceGroupID)],
			snap.Groups[rgGroupKey(highResourceGroupID)])
		require.Equal(t,
			builtinGroupConfigs[rgGroupKey(lowResourceGroupID)],
			snap.Groups[rgGroupKey(lowResourceGroupID)])
		require.Len(t, snap.Groups, 3) // 3 built-ins
	})

	t.Run("get_or_default_unknown_rg", func(t *testing.T) {
		h := testHolder()
		require.Equal(t, defaultRGGroupConfig,
			h.Snapshot().Groups.GetOrDefault(rgGroupKey(9999)))
	})

	t.Run("get_or_default_unknown_tenant", func(t *testing.T) {
		h := testHolder()
		require.Equal(t, defaultTenantGroupConfig,
			h.Snapshot().Groups.GetOrDefault(tenantGroupKey(9999)))
	})

	t.Run("default_configs_have_burst_frac", func(t *testing.T) {
		h := testHolder()
		snap := h.Snapshot()
		highCfg := snap.Groups.GetOrDefault(rgGroupKey(highResourceGroupID))
		require.Equal(t, float64(0.8), highCfg.BurstFrac)
		lowCfg := snap.Groups.GetOrDefault(rgGroupKey(lowResourceGroupID))
		require.Equal(t, float64(0.2), lowCfg.BurstFrac)
		tenantCfg := snap.Groups.GetOrDefault(tenantGroupKey(9999))
		require.Equal(t, float64(0.20), tenantCfg.BurstFrac)
	})

	t.Run("builtins_seeded_at_max_version", func(t *testing.T) {
		h := testHolder()
		snap := h.Snapshot()
		require.Equal(t, uint64(math.MaxUint64),
			snap.Groups[tenantGroupKey(1)].Version)
		require.Equal(t, uint64(math.MaxUint64),
			snap.Groups[rgGroupKey(highResourceGroupID)].Version)
		require.Equal(t, uint64(math.MaxUint64),
			snap.Groups[rgGroupKey(lowResourceGroupID)].Version)
	})
}

// TestResourceGroupConfigHolderSet covers Set's wholesale-replace and
// input-aliasing contracts.
func TestResourceGroupConfigHolderSet(t *testing.T) {
	t.Run("replaces_wholesale", func(t *testing.T) {
		h := testHolder()
		h.Set(ResourceGroupConfigSet{
			rgGroupKey(42): {Weight: 100, MaxCPU: false},
		})
		groups := h.Snapshot().Groups
		// Set is wholesale: the freshly-Set key is present alongside
		// the built-ins (system tenant + high/low RM groups).
		require.Equal(t, ResourceGroupConfig{Weight: 100, MaxCPU: false},
			groups[rgGroupKey(42)])
		require.Equal(t, systemTenantGroupConfig, groups[tenantGroupKey(1)])
		require.Len(t, groups, 4) // 3 built-ins + 1 caller key
	})

	t.Run("input_aliasing_safe", func(t *testing.T) {
		h := testHolder()
		input := ResourceGroupConfigSet{
			rgGroupKey(7): {Weight: 100, MaxCPU: false},
		}
		h.Set(input)

		// Mutate the input post-Set; the holder must not observe it.
		input[rgGroupKey(7)] = ResourceGroupConfig{Weight: 1, MaxCPU: true}
		input[rgGroupKey(8)] = ResourceGroupConfig{Weight: 1, MaxCPU: false}

		groups := h.Snapshot().Groups
		require.Equal(t, ResourceGroupConfig{Weight: 100, MaxCPU: false},
			groups[rgGroupKey(7)])
		require.Len(t, groups, 4) // 3 built-ins + 1 caller key

		// And the holder's internal map is not aliased to the input.
		// Same-package access lets us check the underlying pointer.
		h.current.RLock()
		internalPtr := reflect.ValueOf(h.current.config).Pointer()
		h.current.RUnlock()
		require.NotEqual(t, reflect.ValueOf(input).Pointer(), internalPtr,
			"holder must not retain a reference to the caller's input map")
	})
}

// TestResourceGroupConfigHolderGet covers GetOrDefault for a configured key.
func TestResourceGroupConfigHolderGet(t *testing.T) {
	h := testHolder()
	h.Set(ResourceGroupConfigSet{
		rgGroupKey(42): {Weight: 75, MaxCPU: true},
	})
	require.Equal(t,
		ResourceGroupConfig{Weight: 75, MaxCPU: true},
		h.Snapshot().Groups.GetOrDefault(rgGroupKey(42)))
}

// TestResourceGroupConfigHolderSnapshot verifies Snapshot's contract:
// the Groups map is the holder's installed map (no defensive copy),
// and a subsequent Set installs a fresh map without mutating the
// previously-returned snapshot.
func TestResourceGroupConfigHolderSnapshot(t *testing.T) {
	h := testHolder()
	snap1 := h.Snapshot()

	// Snapshot aliases the internal map (no copy on read). This is
	// the contract: callers must treat the returned map as read-only.
	h.current.RLock()
	internalPtr := reflect.ValueOf(h.current.config).Pointer()
	h.current.RUnlock()
	require.Equal(t, internalPtr, reflect.ValueOf(snap1.Groups).Pointer(),
		"Snapshot should return the installed map directly")

	// A subsequent Set installs a brand-new map. snap1 must remain
	// stable (it points at the previously-installed map, which is
	// immutable post-install) while h.Snapshot() returns the new one.
	h.Set(ResourceGroupConfigSet{
		rgGroupKey(42): {Weight: 100, MaxCPU: true},
	})
	// snap1 must still have the built-in configs.
	require.Equal(t,
		builtinGroupConfigs[rgGroupKey(highResourceGroupID)],
		snap1.Groups[rgGroupKey(highResourceGroupID)],
		"prior snapshot must be unaffected by a subsequent Set")
	// New snapshot has builtins + the freshly-set key.
	snap2 := h.Snapshot()
	require.Equal(t, ResourceGroupConfig{Weight: 100, MaxCPU: true},
		snap2.Groups[rgGroupKey(42)])
	require.Equal(t, systemTenantGroupConfig, snap2.Groups[tenantGroupKey(1)])
	require.Len(t, snap2.Groups, 4) // 3 built-ins + 1 caller key
}

// TestSystemTenantGroupConfig verifies the field values of the built-in
// system tenant config.
func TestSystemTenantGroupConfig(t *testing.T) {
	require.Equal(t, uint32(math.MaxUint32), systemTenantGroupConfig.Weight)
	require.Equal(t, 1.0, systemTenantGroupConfig.BurstFrac)
	require.True(t, systemTenantGroupConfig.MaxCPU)
}

// TestBuiltinGroupConfigInSnapshot verifies that the system tenant
// config appears in the holder's snapshot immediately after
// construction, without any explicit Set call beyond the seed.
func TestBuiltinGroupConfigInSnapshot(t *testing.T) {
	h := testHolder()
	groups := h.Snapshot().Groups
	cfg, ok := groups[tenantGroupKey(1)]
	require.True(t, ok, "system tenant (ID 1) must be present in snapshot")
	require.Equal(t, systemTenantGroupConfig, cfg)
}

// TestSetPanicsOnBuiltinOverwrite verifies that Set panics if the
// caller tries to overwrite any built-in key.
func TestSetPanicsOnBuiltinOverwrite(t *testing.T) {
	h := testHolder()
	for k := range builtinGroupConfigs {
		require.Panics(t, func() {
			h.Set(ResourceGroupConfigSet{
				k: {Weight: 1, BurstFrac: 0.5, MaxCPU: false},
			})
		})
	}
}

// TestConfigSnapshotDefaults verifies the default utilization targets
// surfaced through Snapshot when the underlying cluster settings are
// at their registered defaults.
func TestConfigSnapshotDefaults(t *testing.T) {
	h := testHolder()
	snap := h.Snapshot()
	require.Equal(t, 0.8, snap.AppNoBurstFrac)
	require.Equal(t, 0.95, snap.SystemNoBurstFrac)
	require.Equal(t, 0.05, snap.BurstDelta)
}

// TestConfigSnapshotMaxNonBurstableFraction verifies
// MaxNonBurstableFraction returns per-tier non-burstable targets.
func TestConfigSnapshotMaxNonBurstableFraction(t *testing.T) {
	snap := ConfigSnapshot{
		AppNoBurstFrac:    0.75,
		SystemNoBurstFrac: 0.90,
		BurstDelta:        0.10,
	}
	expected := [numResourceTiers]float64{0.90, 0.75}
	require.Equal(t, expected, snap.MaxNonBurstableFraction())
}

// TestConfigSnapshotMaxFraction verifies MaxFraction returns per-tier
// burstable targets (noBurstFrac + BurstDelta).
func TestConfigSnapshotMaxFraction(t *testing.T) {
	snap := ConfigSnapshot{
		AppNoBurstFrac:    0.75,
		SystemNoBurstFrac: 0.90,
		BurstDelta:        0.10,
	}
	expected := [numResourceTiers]float64{1.0, 0.85}
	require.Equal(t, expected, snap.MaxFraction())
}

// TestResourceGroupConfigHolderIngest covers Ingest's version
// semantics. Ingest is the primary entry point for the BatchRequest
// piggyback path that propagates resource-group configs from tenants
// to KV nodes; the strict-newer-wins check is what keeps stale traffic
// from clobbering a freshly-applied ALTER.
func TestResourceGroupConfigHolderIngest(t *testing.T) {
	userKey := groupKey{tenantID: 5, groupID: 100}

	t.Run("first_ingest_visible_after_promote", func(t *testing.T) {
		h := testHolder()
		cfg := ResourceGroupConfig{Weight: 50, BurstFrac: 0.4, Version: 1}
		h.Ingest(userKey, cfg)
		got, ok := h.Snapshot().Groups[userKey]
		require.True(t, ok, "ingested key must be visible after Snapshot")
		require.Equal(t, cfg, got)
	})

	t.Run("stale_version_is_noop", func(t *testing.T) {
		h := testHolder()
		newer := ResourceGroupConfig{Weight: 50, Version: 2}
		older := ResourceGroupConfig{Weight: 999, Version: 1}
		h.Ingest(userKey, newer)
		// Force promote so newer is in current; the next stale Ingest
		// must take the fast-path no-op.
		_ = h.Snapshot()
		h.Ingest(userKey, older)
		require.Equal(t, newer, h.Snapshot().Groups[userKey])
	})

	t.Run("equal_version_is_noop", func(t *testing.T) {
		h := testHolder()
		first := ResourceGroupConfig{Weight: 50, Version: 7}
		// Re-Ingest at same version with a different Weight; must not win.
		second := ResourceGroupConfig{Weight: 999, Version: 7}
		h.Ingest(userKey, first)
		_ = h.Snapshot()
		h.Ingest(userKey, second)
		require.Equal(t, first, h.Snapshot().Groups[userKey])
	})

	t.Run("newer_overrides_after_promote", func(t *testing.T) {
		h := testHolder()
		v1 := ResourceGroupConfig{Weight: 50, Version: 1}
		v2 := ResourceGroupConfig{Weight: 200, Version: 2}
		h.Ingest(userKey, v1)
		_ = h.Snapshot()
		h.Ingest(userKey, v2)
		require.Equal(t, v2, h.Snapshot().Groups[userKey])
	})

	t.Run("builtins_immortal", func(t *testing.T) {
		h := testHolder()
		// Even a max-version attempt cannot displace a built-in: the
		// stored builtin Version is math.MaxUint64, and the version
		// gate is strict-greater.
		attempt := ResourceGroupConfig{
			Weight: 1, BurstFrac: 0.01, MaxCPU: false, Version: math.MaxUint64,
		}
		h.Ingest(rgGroupKey(highResourceGroupID), attempt)
		_ = h.Snapshot()
		require.Equal(t,
			builtinGroupConfigs[rgGroupKey(highResourceGroupID)],
			h.Snapshot().Groups[rgGroupKey(highResourceGroupID)],
			"builtins must not be overwritable")
	})

	t.Run("snapshot_stable_across_later_ingest", func(t *testing.T) {
		h := testHolder()
		h.Ingest(userKey, ResourceGroupConfig{Weight: 50, Version: 1})
		// Take snapshot then ingest a new version; the previously
		// returned map must not mutate. The promote path installs a
		// fresh next map after each promote, preserving this.
		before := h.Snapshot().Groups
		beforeCfg := before[userKey]
		h.Ingest(userKey, ResourceGroupConfig{Weight: 999, Version: 2})
		_ = h.Snapshot() // force promote
		require.Equal(t, beforeCfg, before[userKey],
			"prior snapshot map must not mutate when later Ingest+promote happens")
	})

	t.Run("concurrent_ingest_highest_version_wins", func(t *testing.T) {
		h := testHolder()
		const N = 64
		var wg sync.WaitGroup
		wg.Add(N)
		for i := 1; i <= N; i++ {
			i := i
			go func() {
				defer wg.Done()
				h.Ingest(userKey, ResourceGroupConfig{
					Weight:  uint32(i),
					Version: uint64(i),
				})
			}()
		}
		wg.Wait()
		got := h.Snapshot().Groups[userKey]
		require.Equal(t, uint64(N), got.Version, "highest version must win")
		require.Equal(t, uint32(N), got.Weight)
	})

	t.Run("promote_thundering_herd_idempotent", func(t *testing.T) {
		h := testHolder()
		h.Ingest(userKey, ResourceGroupConfig{Weight: 50, Version: 1})
		// Many concurrent Snapshots: at most one promote effectively
		// runs (others find !dirty and bail). We can't directly
		// observe the count without instrumenting; assert the visible
		// post-condition: every snapshot sees the ingested entry, and
		// dirty is cleared.
		const N = 32
		var wg sync.WaitGroup
		wg.Add(N)
		for i := 0; i < N; i++ {
			go func() {
				defer wg.Done()
				snap := h.Snapshot()
				cfg, ok := snap.Groups[userKey]
				require.True(t, ok)
				require.Equal(t, uint64(1), cfg.Version)
			}()
		}
		wg.Wait()
		require.False(t, h.dirty.Load(), "dirty must be cleared after promote")
	})

	t.Run("ingest_with_zero_version_visible_initially", func(t *testing.T) {
		// First Ingest for a key always wins regardless of cfg.Version
		// (the "ok" branch is false; the strict-greater check is
		// skipped). This matches the "first-seen sets the floor"
		// semantics: an empty proto would still install with
		// Version=0, then any subsequent non-zero version supersedes.
		h := testHolder()
		k := groupKey{tenantID: 7, groupID: 200}
		zero := ResourceGroupConfig{Weight: 30, Version: 0}
		h.Ingest(k, zero)
		require.Equal(t, zero, h.Snapshot().Groups[k])
		// Re-ingest at Version=0 should be a no-op (equal, not
		// greater).
		other := ResourceGroupConfig{Weight: 99, Version: 0}
		h.Ingest(k, other)
		require.Equal(t, zero, h.Snapshot().Groups[k])
	})
}

// TestResourceGroupConfigHolderPerTenantDefault verifies that the
// per-tenant default entry is lazily installed on the first Ingest
// from a tenant and that GetOrDefault routes unknown user-defined RG
// keys from that tenant to the per-tenant default.
func TestResourceGroupConfigHolderPerTenantDefault(t *testing.T) {
	t.Run("absent_before_any_ingest", func(t *testing.T) {
		h := testHolder()
		_, ok := h.Snapshot().Groups[groupKey{tenantID: 5, groupID: defaultUserResourceGroupID}]
		require.False(t, ok, "per-tenant default must not be installed before any Ingest")
	})

	t.Run("installed_on_first_ingest", func(t *testing.T) {
		h := testHolder()
		k := groupKey{tenantID: 5, groupID: 100}
		h.Ingest(k, ResourceGroupConfig{Weight: 75, Version: 1})
		groups := h.Snapshot().Groups
		ptd, ok := groups[groupKey{tenantID: 5, groupID: defaultUserResourceGroupID}]
		require.True(t, ok, "per-tenant default must be installed after first Ingest")
		require.Equal(t, perTenantDefaultConfig, ptd)
		// Other tenants are untouched.
		_, ok = groups[groupKey{tenantID: 6, groupID: defaultUserResourceGroupID}]
		require.False(t, ok, "per-tenant default must be installed per-tenant only")
	})

	t.Run("get_or_default_routes_unknown_to_per_tenant", func(t *testing.T) {
		h := testHolder()
		// Prime tenant 5 by ingesting some user RG.
		h.Ingest(groupKey{tenantID: 5, groupID: 100},
			ResourceGroupConfig{Weight: 50, Version: 1})
		groups := h.Snapshot().Groups
		// Unknown user-defined RG from tenant 5 routes to the
		// per-tenant default, not the global one.
		unknown := groupKey{tenantID: 5, groupID: 9999}
		require.Equal(t, perTenantDefaultConfig, groups.GetOrDefault(unknown))
		// Unknown user-defined RG from tenant 6 (no prior Ingest)
		// falls back to the global default.
		untouched := groupKey{tenantID: 6, groupID: 9999}
		require.Equal(t, defaultRGGroupConfig, groups.GetOrDefault(untouched))
	})

	t.Run("per_tenant_default_immortal", func(t *testing.T) {
		// The per-tenant default is seeded at Version=math.MaxUint64;
		// a directly-targeted Ingest at the reserved key cannot
		// displace it. (In production this can't happen — tenant IDs
		// allocated by the SQL sequence start at 16 — but verify the
		// holder's contract.)
		h := testHolder()
		ptdKey := groupKey{tenantID: 5, groupID: defaultUserResourceGroupID}
		// Prime tenant 5.
		h.Ingest(groupKey{tenantID: 5, groupID: 100},
			ResourceGroupConfig{Weight: 50, Version: 1})
		_ = h.Snapshot()
		// Attempt to overwrite the per-tenant default itself.
		h.Ingest(ptdKey, ResourceGroupConfig{Weight: 1, Version: math.MaxUint64})
		require.Equal(t, perTenantDefaultConfig, h.Snapshot().Groups[ptdKey])
	})

	t.Run("not_installed_for_tenant_keyed_ingest", func(t *testing.T) {
		// An Ingest with key.tenantID==0 (only the system tenant
		// produces these; tests may craft them) must not install a
		// per-tenant default — there's no tenant to scope it to.
		h := testHolder()
		k := rgGroupKey(100)
		h.Ingest(k, ResourceGroupConfig{Weight: 50, Version: 1})
		// No per-tenant default for tenant 0.
		_, ok := h.Snapshot().Groups[groupKey{
			tenantID: 0, groupID: defaultUserResourceGroupID,
		}]
		require.False(t, ok)
	})
}
