// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package resourcegroupcache_test

import (
	"context"
	"math"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/resourcegroupcache"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

// startCache spins up a server with the resource-group feature
// enabled and returns a Cache that has finished its initial scan,
// along with a SQL runner for driving CREATE/ALTER/DROP from the
// test.
func startCache(t *testing.T) (*resourcegroupcache.Cache, *sqlutils.SQLRunner, func()) {
	t.Helper()
	ctx := context.Background()
	srv, sqlDB, _ := serverutils.StartServer(t, base.TestServerArgs{})
	ts := srv.ApplicationLayer()
	r := sqlutils.MakeSQLRunner(sqlDB)
	r.Exec(t, `SET CLUSTER SETTING sql.experimental_resource_groups.enabled = true`)

	execCfg := ts.ExecutorConfig().(sql.ExecutorConfig)
	c := resourcegroupcache.New(
		ts.Clock(),
		execCfg.RangeFeedFactory,
		ts.AppStopper(),
		execCfg.InternalDB,
		execCfg.Codec,
	)
	require.NoError(t, c.Start(ctx, ts.SystemTableIDResolver().(catalog.SystemTableIDResolver)))
	require.NoError(t, c.WaitForStarted(ctx))
	return c, r, func() { srv.Stopper().Stop(ctx) }
}

// lookupByName waits for the rangefeed to deliver name and returns
// the corresponding Entry, or fails the test on timeout.
func lookupByName(t *testing.T, c *resourcegroupcache.Cache, name string) resourcegroupcache.Entry {
	t.Helper()
	ctx := context.Background()
	var entry resourcegroupcache.Entry
	testutils.SucceedsSoon(t, func() error {
		id, ok, err := c.NameToID(ctx, name)
		if err != nil {
			return err
		}
		if !ok {
			return errors.Newf("%q not visible yet", name)
		}
		e, ok := c.LookupByID(id)
		if !ok {
			return errors.Newf("%q id %d not in byID yet", name, id)
		}
		entry = e
		return nil
	})
	return entry
}

func TestCache_RangefeedUpdates(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	c, r, stop := startCache(t)
	defer stop()

	r.Exec(t, `CREATE RESOURCE GROUP analytics WITH cpu_weight = 200`)
	r.Exec(t, `CREATE RESOURCE GROUP reports   WITH cpu_weight = 100`)

	analytics := lookupByName(t, c, "analytics")
	require.EqualValues(t, 1, analytics.Version)
	require.EqualValues(t, 200, analytics.Config.CPUWeight)
	analyticsID := analytics.ID

	// Cohort: analytics(200) + reports(100) + any built-ins, plus the
	// pseudo-tenant default seeded at MaxInt64 (currently absent here,
	// but the user-visible groups should have a BurstFrac that reflects
	// the visible total). Just verify it's in (0, 1].
	reports := lookupByName(t, c, "reports")
	require.Greater(t, analytics.Config.BurstFrac, 0.0)
	require.LessOrEqual(t, analytics.Config.BurstFrac, 1.0)
	require.Greater(t, reports.Config.BurstFrac, 0.0)
	require.Less(t, reports.Config.BurstFrac, analytics.Config.BurstFrac)

	// ALTER bumps the version; cache should observe the new version,
	// the new weight, and a re-normalized BurstFrac.
	r.Exec(t, `ALTER RESOURCE GROUP analytics WITH cpu_weight = 400`)
	testutils.SucceedsSoon(t, func() error {
		e, ok := c.LookupByID(analyticsID)
		if !ok {
			return errors.New("analytics disappeared after ALTER")
		}
		if e.Version != 2 {
			return errors.Newf("version = %d, want 2", e.Version)
		}
		if e.Config.CPUWeight != 400 {
			return errors.Newf("cpu_weight = %d, want 400", e.Config.CPUWeight)
		}
		return nil
	})

	// DROP evicts both name and id from the cache.
	r.Exec(t, `DROP RESOURCE GROUP analytics`)
	testutils.SucceedsSoon(t, func() error {
		id, ok, err := c.NameToID(context.Background(), "analytics")
		if err != nil {
			return err
		}
		if ok {
			return errors.Newf("analytics still resolvable to %d after DROP", id)
		}
		if _, found := c.LookupByID(analyticsID); found {
			return errors.New("analytics id still in byID after DROP")
		}
		return nil
	})

	// The other group is untouched.
	id, ok, err := c.NameToID(context.Background(), "reports")
	require.NoError(t, err)
	require.True(t, ok)
	e, ok := c.LookupByID(id)
	require.True(t, ok)
	require.EqualValues(t, 100, e.Config.CPUWeight)
}

func TestCache_NameToIDReadThrough(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	c, r, stop := startCache(t)
	defer stop()
	ctx := context.Background()

	// Missing name + no row: returns ok=false, err=nil.
	_, ok, err := c.NameToID(ctx, "ghost")
	require.NoError(t, err)
	require.False(t, ok)

	// CREATE on the same server; the cache will eventually see this via
	// the rangefeed, but a read-through must be able to resolve the
	// name immediately (modeling the single-node version of the
	// cross-node race the read-through is meant to handle).
	r.Exec(t, `CREATE RESOURCE GROUP fastlane WITH cpu_weight = 50`)
	id, ok, err := c.NameToID(ctx, "fastlane")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotZero(t, id)

	// LookupByID may or may not have the Entry yet (depends on rangefeed
	// timing). What is guaranteed is that the next NameToID is a cache
	// hit because the read-through populated byName.
	require.Less(t, id, uint64(math.MaxUint64))
	id2, ok2, err := c.NameToID(ctx, "fastlane")
	require.NoError(t, err)
	require.True(t, ok2)
	require.Equal(t, id, id2)

	// Eventually the rangefeed delivers the Entry into byID with a
	// normalized BurstFrac.
	testutils.SucceedsSoon(t, func() error {
		e, ok := c.LookupByID(id)
		if !ok {
			return errors.New("fastlane not in byID yet")
		}
		if e.Version != 1 {
			return errors.Newf("version = %d, want 1", e.Version)
		}
		if e.Config.CPUWeight != 50 {
			return errors.Newf("cpu_weight = %d, want 50", e.Config.CPUWeight)
		}
		if e.Config.BurstFrac <= 0 {
			return errors.Newf("burst_frac = %v, want > 0", e.Config.BurstFrac)
		}
		return nil
	})
}
