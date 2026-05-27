// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package upgrades

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/upgrade"
)

const addResourceGroupsVersionColumn = `
ALTER TABLE system.resource_groups
ADD COLUMN IF NOT EXISTS version INT8 NOT NULL DEFAULT 1:::INT8
FAMILY "primary"
`

// addResourceGroupsVersionColumnMigration adds the version column to
// system.resource_groups. The version is incremented on every config update
// and is carried alongside the config on BatchRequest admission headers so
// KV nodes can keep their in-memory holder fresh without a central
// reconciler.
//
// The table itself is created in V26_3_AddResourceGroupsTable with a
// dynamically-assigned ID, so we issue the ALTER by name rather than going
// through migrateTable (which needs a static descpb.ID). IF NOT EXISTS makes
// the statement idempotent across retries.
func addResourceGroupsVersionColumnMigration(
	ctx context.Context, _ clusterversion.ClusterVersion, d upgrade.TenantDeps,
) error {
	_, err := d.DB.Executor().ExecEx(
		ctx,
		"add-version-to-resource-groups",
		nil, /* txn */
		sessiondata.InternalExecutorOverride{User: username.NodeUserName()},
		addResourceGroupsVersionColumn,
	)
	return err
}
