// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sql_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/admission"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

// TestResourceGroupEndToEndPropagation drives a user-defined resource
// group from the SQL boundary on one node down to the admission-control
// holder on another node, over a real BatchRequest. It exercises every
// link of the piggyback design at once:
//
//  1. CREATE RESOURCE GROUP on node A inserts into system.resource_groups.
//  2. SET resource_group = 'analytics' on node B resolves the name via
//     the local rangefeed cache and stores the id on the session.
//  3. A subsequent INSERT from node B that lands on a range whose
//     leaseholder is node A stamps (id, config) on the BatchRequest's
//     AdmissionHeader via the txn-level plumbing.
//  4. Node A's kvadmission layer ingests the carried config under
//     (tenantID, id) and the holder records it.
//  5. ALTER RESOURCE GROUP on node A bumps the row's version; the
//     rangefeed cache on node B picks it up; the next BatchRequest
//     carries the newer version and the holder on node A promotes it.
//
// The test asserts on node A's holder snapshot via the
// OnCPUGrantCoordinatorsCreated knob, which hands back a
// *admission.CPUGrantCoordinators captured at server startup.
func TestResourceGroupEndToEndPropagation(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()

	// Each server's CPUGrantCoordinators is captured here via a
	// per-node testing knob. We only need node 0's, but the knob fires
	// once per server so we wire one per node and only consult [0].
	coords := make([]*admission.CPUGrantCoordinators, 2)

	makeKnobs := func(i int) base.TestingKnobs {
		return base.TestingKnobs{
			AdmissionControl: &admission.TestingKnobs{
				OnCPUGrantCoordinatorsCreated: func(c *admission.CPUGrantCoordinators) {
					coords[i] = c
				},
			},
		}
	}
	perNode := map[int]base.TestServerArgs{
		0: {Knobs: makeKnobs(0)},
		1: {Knobs: makeKnobs(1)},
	}

	tc := serverutils.StartCluster(t, 2, base.TestClusterArgs{
		ReplicationMode: base.ReplicationManual,
		// Force shared-process tenant mode. The CPU grant coordinators
		// are created by the storage layer (pkg/server.NewServer); in
		// shared-process mode the secondary tenant SQL runs in that
		// same process, so the knob captures the only coordinator and
		// the BatchRequest sent from tenant SQL on one node lands on
		// the storage-layer holder of another. External-process mode
		// would split the SQL gateway off into its own process and
		// complicate this test without exercising anything different
		// end-to-end.
		ServerArgs: base.TestServerArgs{
			DefaultTestTenant: base.SharedTestTenantAlwaysEnabled,
		},
		ServerArgsPerNode: perNode,
	})
	defer tc.Stopper().Stop(ctx)

	require.NotNil(t, coords[0], "node 0 OnCPUGrantCoordinatorsCreated did not fire")
	require.NotNil(t, coords[1], "node 1 OnCPUGrantCoordinatorsCreated did not fire")

	dbA := tc.ServerConn(0)
	dbB := tc.ServerConn(1)
	// Pin node B's runner to a single backing connection so SET
	// resource_group and the subsequent INSERTs share a session.
	dbB.SetMaxOpenConns(1)

	connA := sqlutils.MakeSQLRunner(dbA)

	// Enable the user-defined resource-group SQL surface plus the
	// resource-manager admission mode that consults the holder. Both
	// are cluster settings so a single SET on either node suffices.
	connA.Exec(t, `SET CLUSTER SETTING sql.experimental_resource_groups.enabled = true`)
	connA.Exec(t, `SET CLUSTER SETTING admission.cpu_time_tokens.mode = 'resource_manager'`)

	// Build a table with two ranges; pin the second range's lease to
	// node 0. The leaseholder is the node whose kvadmission will see
	// the BatchRequest, so all asserts target coords[0].
	connA.Exec(t, `CREATE TABLE t (k INT PRIMARY KEY)`)
	connA.Exec(t, `ALTER TABLE t SPLIT AT VALUES (100)`)
	storeAID := tc.Server(0).GetFirstStoreID()
	connA.ExecSucceedsSoon(t, fmt.Sprintf(
		`ALTER TABLE t EXPERIMENTAL_RELOCATE VALUES (ARRAY[%d], 100)`, storeAID,
	))

	// Create the group on node A.
	connA.Exec(t, `CREATE RESOURCE GROUP analytics WITH cpu_weight = 200`)
	var groupID uint64
	connA.QueryRow(t,
		`SELECT id FROM system.resource_groups WHERE name = 'analytics'`,
	).Scan(&groupID)
	require.NotZero(t, groupID)

	tenantID := tc.Server(1).RPCContext().TenantID.ToUint64()

	// Wait for node B's rangefeed cache to see the new group, then
	// bind it on the pinned connection. The SET-then-INSERT pair must
	// share a session, which is why dbB has MaxOpenConns=1.
	testutils.SucceedsSoon(t, func() error {
		_, err := dbB.Exec(`SET resource_group = 'analytics'`)
		return err
	})

	// Wait for node B's rangefeed to deliver the group's byID entry
	// before driving writes. SET's read-through only populates the
	// byName index, so the gateway would otherwise ship an empty
	// config until the rangefeed catches up — and an empty config is
	// gated out before Ingest.
	testutils.SucceedsSoon(t, func() error {
		var w int
		if err := dbB.QueryRow(
			`SELECT cpu_weight FROM [SHOW RESOURCE GROUP analytics]`).Scan(&w); err != nil {
			return err
		}
		if w != 200 {
			return errors.Newf("node B cache not yet seeing weight 200, got %d", w)
		}
		return nil
	})

	// Retry the INSERT-then-lookup in a single loop. The INSERT is what
	// actually triggers an Ingest on node A's holder; one shot may
	// race with promotion timing, so we redrive.
	keys := 0
	testutils.SucceedsSoon(t, func() error {
		keys++
		if _, err := dbB.Exec(`INSERT INTO t VALUES (100 + $1)`, keys); err != nil {
			return err
		}
		cfg, ok := coords[0].LookupIngestedResourceGroupConfig(tenantID, groupID)
		if !ok {
			return errors.Newf(
				"no config installed yet for (tenant=%d, group=%d)",
				tenantID, groupID)
		}
		if cfg.Weight != 200 {
			return errors.Newf("weight: got %d, want 200", cfg.Weight)
		}
		if cfg.Version != 1 {
			return errors.Newf("version: got %d, want 1", cfg.Version)
		}
		return nil
	})

	// Bump the config and expect node A to promote a strictly newer
	// version. ALTER lives on node A so we don't measure rangefeed lag
	// on the originating node's cache too; node B's cache is what
	// drives the next batch.
	connA.Exec(t, `ALTER RESOURCE GROUP analytics WITH cpu_weight = 400`)

	// Wait for node B's rangefeed to deliver the updated row before
	// the next write. The session has the same id; only the cache
	// lookup picks up the new config.
	testutils.SucceedsSoon(t, func() error {
		var weight int
		if err := dbB.QueryRow(
			`SELECT cpu_weight FROM [SHOW RESOURCE GROUP analytics]`).Scan(&weight); err != nil {
			return err
		}
		if weight != 400 {
			return errors.Newf("node B SHOW: got weight %d, want 400", weight)
		}
		return nil
	})

	testutils.SucceedsSoon(t, func() error {
		keys++
		if _, err := dbB.Exec(`INSERT INTO t VALUES (100 + $1)`, keys); err != nil {
			return err
		}
		cfg, ok := coords[0].LookupIngestedResourceGroupConfig(tenantID, groupID)
		if !ok {
			return errors.Newf(
				"config disappeared for (tenant=%d, group=%d)", tenantID, groupID)
		}
		if cfg.Weight != 400 {
			return errors.Newf("weight: got %d, want 400", cfg.Weight)
		}
		if cfg.Version < 2 {
			return errors.Newf("version: got %d, want >=2", cfg.Version)
		}
		return nil
	})
}
