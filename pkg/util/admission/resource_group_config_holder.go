// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package admission

import (
	"math"
	"slices"
	"sync/atomic"

	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

// ResourceGroupConfig is the per-group state WorkQueue applies when admitting
// work in Resource Manager mode. Callers must pre-normalize the values; the
// holder stores them as-is.
type ResourceGroupConfig struct {
	// Weight is the group's share used for fair-share ordering in the heap.
	// Groups with higher weight get proportionally more CPU time before
	// yielding to others. Must be > 0.
	Weight uint32
	// BurstFrac is the fraction of the 100%-CPU rate allocated to this
	// group's per-group burst bucket. For example, 0.2 means the group's
	// burst bucket refills at 20% of the full CPU rate.
	BurstFrac float64
	// MaxCPU=true forces canBurst regardless of bucket utilization. The bucket
	// is still refilled at BurstFrac of the 100%-CPU rate; MaxCPU only exempts
	// the group from the bucket-fullness gate. Within the same canBurst
	// qualification, groups remain ordered by used/weight.
	MaxCPU bool
	// Version monotonically identifies the freshness of this config.
	// Ingest only overwrites a stored config when the incoming Version
	// is strictly greater than the stored one. Built-in configs are
	// seeded at math.MaxUint64 so Ingest can never displace them.
	// Caller-supplied configs originate from the per-tenant
	// system.resource_groups table, whose version column is bumped on
	// every ALTER.
	Version uint64
}

// ResourceGroupConfigSet is the set of per-group configs keyed by groupKey.
// Once installed in the holder, callers must treat the map as read-only.
type ResourceGroupConfigSet map[groupKey]ResourceGroupConfig

// SafeFormat renders one entry per line, sorted by tenantID then
// groupID, e.g.:
//
//	t0g1 weight=80 burstFrac=0.80 maxCPU=true version=18446744073709551615
//	t0g2 weight=20 burstFrac=0.20 maxCPU=false version=18446744073709551615
func (s ResourceGroupConfigSet) SafeFormat(w redact.SafePrinter, _ rune) {
	keys := make([]groupKey, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, groupKey.compare)
	for _, k := range keys {
		cfg := s[k]
		w.Printf("%s weight=%d burstFrac=%.2f maxCPU=%t version=%d\n",
			k, cfg.Weight, cfg.BurstFrac, cfg.MaxCPU, cfg.Version)
	}
}

// String implements fmt.Stringer via SafeFormat.
func (s ResourceGroupConfigSet) String() string {
	return redact.StringWithoutMarkers(s)
}

// GetOrDefault returns the config for k if installed; otherwise it
// applies the following fallbacks in order:
//
//  1. If k is a tenant group (groupID == 0), return
//     defaultTenantGroupConfig.
//  2. If k.tenantID != 0 (user-defined RG keyed by tenant), look up
//     the per-tenant default groupKey{k.tenantID, defaultUserResourceGroupID}.
//     This entry is lazily installed on first Ingest from k.tenantID,
//     so once any work has flowed in from that tenant, unknown
//     group IDs from the same tenant route to the tenant's own
//     default config rather than to the global fallback.
//  3. Otherwise return defaultRGGroupConfig.
//
// TODO(wenyihu6): collapse to a single fallback once we can align
// the rg and tenant defaults.
func (s ResourceGroupConfigSet) GetOrDefault(k groupKey) ResourceGroupConfig {
	if cfg, ok := s[k]; ok {
		return cfg
	}
	if k.groupID == 0 {
		// Tenant group (tenantID is set, groupID is zero).
		return defaultTenantGroupConfig
	}
	if k.tenantID != 0 {
		// User-defined RG keyed by tenant. Try the per-tenant default
		// first; this is the entry the holder installs lazily on the
		// first Ingest from this tenant.
		if cfg, ok := s[groupKey{tenantID: k.tenantID, groupID: defaultUserResourceGroupID}]; ok {
			return cfg
		}
	}
	// Resource group (groupID is set).
	return defaultRGGroupConfig
}

// defaultRGGroupConfig is the safety fallback returned by GetOrDefault
// for resource group keys (groupID != 0) not in the installed
// configuration and for which no per-tenant default has been
// installed. In steady state for user-defined RGs this is unreachable
// once any work has flowed in from the requesting tenant (the
// per-tenant lazy default takes over); it exists to keep Admit's
// lazy-create path total — if a caller installs a config that omits a
// known group ID, Admit gets a usable weight rather than a
// zero-weight group. Weight=20 mirrors the low default; MaxCPU=false
// keeps an unconfigured group from bypassing the burst-fullness gate.
var defaultRGGroupConfig = ResourceGroupConfig{
	Weight: 20, BurstFrac: 0.2, MaxCPU: false, Version: math.MaxUint64,
}

// defaultTenantGroupConfig is the fallback for tenant group keys
// (groupID == 0): every tenant gets defaultGroupWeight, since
// per-tenant weights are no longer configurable. MaxCPU=false because
// tenants don't carry burst flags.
var defaultTenantGroupConfig = ResourceGroupConfig{
	Weight: defaultGroupWeight, BurstFrac: 0.20, MaxCPU: false, Version: math.MaxUint64,
}

// systemTenantGroupConfig is the built-in config for the system tenant
// (ID 1). It has maximum weight to ensure system work is never starved,
// BurstFrac=1.0 so the full burst budget is available, and MaxCPU=true
// to bypass the burst-fullness gate.
var systemTenantGroupConfig = ResourceGroupConfig{
	Weight: math.MaxUint32, BurstFrac: 1.0, MaxCPU: true, Version: math.MaxUint64,
}

// builtinGroupConfigs are configs that are always present in the
// holder. Set seeds from this list first; callers cannot overwrite
// built-in keys. All built-ins are seeded at Version=math.MaxUint64
// so Ingest's strict-newer-wins check can never displace them.
var builtinGroupConfigs = ResourceGroupConfigSet{
	tenantGroupKey(1): systemTenantGroupConfig,
	rgGroupKey(highResourceGroupID): {
		Weight: 80, BurstFrac: 0.8, MaxCPU: true, Version: math.MaxUint64,
	},
	rgGroupKey(lowResourceGroupID): {
		Weight: 20, BurstFrac: 0.2, MaxCPU: false, Version: math.MaxUint64,
	},
}

// perTenantDefaultConfig is the config installed lazily under
// groupKey{tenantID: T, groupID: defaultUserResourceGroupID} on the
// first Ingest from tenant T. It is seeded at Version=math.MaxUint64
// so a subsequent Ingest cannot displace it (user-defined RG IDs
// start at 16; the reserved low IDs are never sent by tenants).
//
// We use the global defaultRGGroupConfig as the per-tenant default
// config: tenants that have never altered any group via SQL will see
// the same shape as the safety fallback. If product requirements
// later diverge, this is the seam to change.
var perTenantDefaultConfig = ResourceGroupConfig{
	Weight: defaultRGGroupConfig.Weight, BurstFrac: defaultRGGroupConfig.BurstFrac,
	MaxCPU: defaultRGGroupConfig.MaxCPU, Version: math.MaxUint64,
}

// ConfigSnapshot is the immutable snapshot returned by
// ResourceGroupConfigHolder.Snapshot. It bundles the per-group config
// set with the utilization targets derived from cluster settings, plus
// the cpuTimeTokenACMode value those targets were derived under.
type ConfigSnapshot struct {
	// Groups is the per-group config set (built-ins + caller-provided).
	Groups ResourceGroupConfigSet
	// Mode is the cpuTimeTokenACMode value read alongside the
	// utilization targets. Carried so consumers that need both stay
	// coherent without a second setting read.
	Mode cpuTimeTokenMode
	// AppNoBurstFrac is the non-burstable CPU utilization target for
	// the app tenant tier (e.g. 0.8 for 80% of CPU capacity).
	AppNoBurstFrac float64
	// SystemNoBurstFrac is the non-burstable CPU utilization target
	// for the system tenant tier.
	//
	// TODO(ssd): SystemNoBurstFrac will be removed when settings are
	// consolidated; both tiers will use AppNoBurstFrac.
	SystemNoBurstFrac float64
	// BurstDelta is the delta added to the non-burstable fraction to
	// produce the burstable utilization ceiling.
	BurstDelta float64
}

// MaxNonBurstableFraction returns the non-burstable CPU utilization
// target per tier.
//
// TODO(ssd): Returns per-tier values for now; will collapse to a
// scalar when tiers are removed.
func (s ConfigSnapshot) MaxNonBurstableFraction() [numResourceTiers]float64 {
	return [numResourceTiers]float64{s.SystemNoBurstFrac, s.AppNoBurstFrac}
}

// MaxFraction returns the burstable CPU utilization ceiling
// (noBurstFrac + BurstDelta) per tier.
//
// TODO(ssd): Returns per-tier values for now; will collapse to a
// scalar when tiers are removed.
func (s ConfigSnapshot) MaxFraction() [numResourceTiers]float64 {
	return [numResourceTiers]float64{
		s.SystemNoBurstFrac + s.BurstDelta,
		s.AppNoBurstFrac + s.BurstDelta,
	}
}

// ResourceGroupConfigHolder owns the source-of-truth config set for
// RM mode. It separates the hot read path (Snapshot, Ingest's fast
// version check) from the cold write path (Ingest's overwrite path,
// promote) using two maps:
//
//   - current: read by Snapshot and the Ingest fast path under
//     current.RLock. Writers (promote) take current.Lock briefly
//     during the swap.
//   - next: written by the Ingest slow path under next.Lock. Always a
//     per-key, by-version superset of current (invariant: never has
//     an older Version than current for any key, and contains every
//     key that current contains).
//   - dirty: atomic flag set by the Ingest slow path; checked by
//     Snapshot to gate the promote() call.
//
// The flow:
//
//   - Snapshot: if dirty, call promote(); then RLock current and
//     return the map. Most calls are no-ops on dirty (false branch)
//     plus one RLock.
//   - Ingest: RLock current to check version. If incoming is not
//     strictly newer, return. Otherwise Lock next, re-check version
//     (covers races against another Ingest that already promoted),
//     overwrite, set dirty.
//   - promote: Lock next; if !dirty, return (raced with another
//     promoter); Lock current; swap current.config = next.config; then
//     copy next.config to a fresh map so the next slow path mutates a
//     map that no Snapshot can observe; clear dirty.
//
// Built-in configs (builtinGroupConfigs) are seeded in both maps at
// construction and re-seeded by Set. Their Version is math.MaxUint64,
// so the Ingest version check makes them effectively immortal.
//
// The per-tenant default (groupKey{T, defaultUserResourceGroupID}) is
// installed lazily under the next lock during the first Ingest from
// tenant T. It also rides at Version=math.MaxUint64 so it cannot be
// overwritten by a subsequent Ingest with the same key (which would
// require a tenant having configured a user-defined RG at the
// reserved ID 3 — impossible, since user IDs start at 16).
type ResourceGroupConfigHolder struct {
	// sv provides access to cluster settings for the snapshot's
	// mode and utilization targets. Required.
	sv *settings.Values

	current struct {
		syncutil.RWMutex
		config ResourceGroupConfigSet
	}
	next struct {
		syncutil.Mutex
		config ResourceGroupConfigSet
	}
	dirty atomic.Bool
}

// newResourceGroupConfigHolder constructs a holder seeded with
// builtinGroupConfigs. sv must be non-nil; the holder reads cluster
// settings on every Snapshot.
func newResourceGroupConfigHolder(sv *settings.Values) *ResourceGroupConfigHolder {
	if sv == nil {
		panic(errors.AssertionFailedf("newResourceGroupConfigHolder: sv must be non-nil"))
	}
	h := &ResourceGroupConfigHolder{sv: sv}
	h.Set(nil)
	return h
}

// Set replaces the stored config wholesale. Keys absent from config are
// dropped. Built-in configs (builtinGroupConfigs) are always present;
// callers cannot overwrite them. Set installs fresh maps in both
// current and next, preserving the invariant that next is a per-key,
// by-version superset of current (they are equal post-Set).
//
// NB: caller may mutate config after Set returns; the input is copied.
func (h *ResourceGroupConfigHolder) Set(config ResourceGroupConfigSet) {
	cp := make(ResourceGroupConfigSet, len(builtinGroupConfigs)+len(config))
	for k, v := range builtinGroupConfigs {
		cp[k] = v
	}
	for k, v := range config {
		if _, ok := builtinGroupConfigs[k]; ok {
			panic(errors.AssertionFailedf(
				"ResourceGroupConfigHolder.Set: key %s is a built-in and cannot be overwritten", k))
		}
		cp[k] = v
	}
	// Fresh copy for next so subsequent Ingest mutations on next do
	// not aliasing-leak into the map handed out by Snapshot.
	nextCp := make(ResourceGroupConfigSet, len(cp))
	for k, v := range cp {
		nextCp[k] = v
	}
	// Take both locks; current.Lock first (writer), then next.Lock.
	// promote() takes next.Lock then current.Lock — Set is called only
	// at construction / from tests, never concurrently with promote in
	// production, so the ordering inversion is acceptable here. It
	// would be a deadlock risk only if Set could race with promote on
	// the same holder; tests do not do that.
	h.next.Lock()
	defer h.next.Unlock()
	h.current.Lock()
	defer h.current.Unlock()
	h.current.config = cp
	h.next.config = nextCp
	h.dirty.Store(false)
}

// Ingest installs cfg under key if cfg.Version is strictly greater
// than the currently stored version. No-op otherwise. Ingest is safe
// to call concurrently from many goroutines.
//
// On the first Ingest from a tenant (tenantID = key.tenantID, key
// itself may be any user-defined RG from that tenant), Ingest also
// installs the per-tenant default entry under
// groupKey{tenantID: key.tenantID, groupID: defaultUserResourceGroupID}
// at Version=math.MaxUint64, so subsequent GetOrDefault lookups for
// unknown user IDs from this tenant route to that entry rather than
// to the global defaultRGGroupConfig. The lazy install is a no-op for
// tenant-keyed (groupID==0) ingests, which never happen in production
// but may appear in tests.
func (h *ResourceGroupConfigHolder) Ingest(key groupKey, cfg ResourceGroupConfig) {
	// Fast path: read current. If we already have at least this
	// version AND the per-tenant default for key.tenantID is already
	// installed, there is nothing for the slow path to do. This
	// avoids serializing the common "re-receive the same config" case
	// behind next.Lock.
	needPTD := key.tenantID != 0
	cur, haveKey, havePTD := h.fastPathRead(key, needPTD)
	if haveKey && cur.Version >= cfg.Version && havePTD {
		return
	}

	// Slow path: stage in next.
	h.next.Lock()
	defer h.next.Unlock()
	if staged, ok := h.next.config[key]; !ok || staged.Version < cfg.Version {
		h.next.config[key] = cfg
		h.dirty.Store(true)
	}
	// Lazily install the per-tenant default. We check next (the
	// authoritative write-side map) under its own lock to avoid
	// double-installing across racing Ingests from the same tenant.
	if needPTD {
		ptdKey := groupKey{tenantID: key.tenantID, groupID: defaultUserResourceGroupID}
		if _, ok := h.next.config[ptdKey]; !ok {
			h.next.config[ptdKey] = perTenantDefaultConfig
			h.dirty.Store(true)
		}
	}
}

// fastPathRead performs the Ingest fast-path read of current under a
// single RLock. It returns the stored config for key (if any) and
// whether the per-tenant default for key.tenantID is already
// installed (or "true" when checkPTD is false, so callers can ignore
// it for tenant-keyed Ingests).
func (h *ResourceGroupConfigHolder) fastPathRead(
	key groupKey, checkPTD bool,
) (cur ResourceGroupConfig, haveKey, havePTD bool) {
	h.current.RLock()
	defer h.current.RUnlock()
	cur, haveKey = h.current.config[key]
	havePTD = !checkPTD
	if checkPTD {
		_, havePTD = h.current.config[groupKey{
			tenantID: key.tenantID, groupID: defaultUserResourceGroupID,
		}]
	}
	return cur, haveKey, havePTD
}

// Snapshot returns the installed config bundled with utilization
// targets from cluster settings. The Groups map is returned directly
// (no copy); it is immutable post-install because promote() installs
// a fresh map rather than mutating in place, so prior snapshots
// remain stable. Snapshot is hot — every Admit reads it.
func (h *ResourceGroupConfigHolder) Snapshot() ConfigSnapshot {
	if h.dirty.Load() {
		h.promote()
	}
	h.current.RLock()
	groups := h.current.config
	h.current.RUnlock()
	snap := ConfigSnapshot{
		Groups:     groups,
		Mode:       cpuTimeTokenACMode.Get(h.sv),
		BurstDelta: KVCPUTimeUtilBurstDelta.Get(h.sv),
	}
	// TODO(ssd): The mode switch will be removed when settings are
	// consolidated into a single target_util setting.
	switch snap.Mode {
	case resourceManagerMode:
		target := KVCPUTimeUtilTarget.Get(h.sv)
		snap.AppNoBurstFrac = target
		snap.SystemNoBurstFrac = target
	default:
		// offMode and serverlessMode both use the per-tier settings.
		// The old constructor mapped offMode → serverlessMode.
		snap.AppNoBurstFrac = KVCPUTimeAppUtilGoal.Get(h.sv)
		snap.SystemNoBurstFrac = KVCPUTimeSystemUtilGoal.Get(h.sv)
	}
	return snap
}

// promote installs the staged config from next onto current.
// Idempotent and safe to call from multiple goroutines: only the
// first promoter past the dirty re-check does the swap; the rest
// observe dirty=false and return.
//
// After the swap, next is reseeded with a deep copy of current so
// subsequent Ingest slow-path writes mutate a map no prior Snapshot
// can observe.
func (h *ResourceGroupConfigHolder) promote() {
	h.next.Lock()
	defer h.next.Unlock()
	if !h.dirty.Load() {
		return // raced with another promoter
	}
	h.current.Lock()
	h.current.config = h.next.config
	h.current.Unlock()
	cp := make(ResourceGroupConfigSet, len(h.next.config))
	for k, v := range h.next.config {
		cp[k] = v
	}
	h.next.config = cp
	h.dirty.Store(false)
}
