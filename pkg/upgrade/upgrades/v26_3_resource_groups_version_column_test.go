// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package upgrades_test

import (
	"context"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catenumpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/systemschema"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catconstants"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/upgrade/upgrades"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
)

// TestResourceGroupsVersionColumn verifies that the
// V26_3_AddResourceGroupsVersionColumn migration adds the version column
// and that the NOT NULL DEFAULT 1 lands on both existing and new rows.
func TestResourceGroupsVersionColumn(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	clusterversion.SkipWhenMinSupportedVersionIsAtLeast(t,
		clusterversion.V26_3_AddResourceGroupsVersionColumn)

	clusterArgs := base.TestClusterArgs{
		ServerArgs: base.TestServerArgs{
			Knobs: base.TestingKnobs{
				Server: &server.TestingKnobs{
					DisableAutomaticVersionUpgrade: make(chan struct{}),
					ClusterVersionOverride:         clusterversion.MinSupported.Version(),
				},
			},
		},
	}

	ctx := context.Background()
	tc := testcluster.StartTestCluster(t, 1, clusterArgs)
	defer tc.Stopper().Stop(ctx)
	s, sqlDB := tc.Server(0), tc.ServerConn(0)

	// Get the table created at its current bootstrap version (which already
	// has the column), then overwrite its descriptor with the pre-column
	// shape to simulate a real upgrade from a cluster that bootstrapped at
	// V26_3_AddResourceGroupsTable.
	upgrades.Upgrade(t, sqlDB, clusterversion.V26_3_AddResourceGroupsTable, nil, false)
	upgrades.InjectLegacyTable(ctx, t, s,
		systemschema.ResourceGroupsTable, getPreVersionColumnResourceGroupsDescriptor)

	_, err := sqlDB.Exec("SELECT version FROM system.resource_groups LIMIT 0")
	require.Error(t, err, "version column should not exist on injected legacy descriptor")

	_, err = sqlDB.Exec(
		"INSERT INTO system.resource_groups (id, name, config) VALUES (16, 'preexisting', b'')")
	require.NoError(t, err)

	upgrades.Upgrade(t, sqlDB, clusterversion.V26_3_AddResourceGroupsVersionColumn, nil, false)

	var version int64
	require.NoError(t,
		sqlDB.QueryRow(
			"SELECT version FROM system.resource_groups WHERE id = 16",
		).Scan(&version))
	require.Equal(t, int64(1), version, "existing row should backfill to version 1")

	_, err = sqlDB.Exec(
		"INSERT INTO system.resource_groups (id, name, config) VALUES (17, 'fresh', b'')")
	require.NoError(t, err)
	require.NoError(t,
		sqlDB.QueryRow(
			"SELECT version FROM system.resource_groups WHERE id = 17",
		).Scan(&version))
	require.Equal(t, int64(1), version)
}

// getPreVersionColumnResourceGroupsDescriptor returns the
// system.resource_groups descriptor as it existed in
// V26_3_AddResourceGroupsTable, before the version column was added.
func getPreVersionColumnResourceGroupsDescriptor() *descpb.TableDescriptor {
	return &descpb.TableDescriptor{
		Name:                    string(catconstants.ResourceGroupsTableName),
		ParentID:                keys.SystemDatabaseID,
		UnexposedParentSchemaID: keys.PublicSchemaID,
		Version:                 1,
		Columns: []descpb.ColumnDescriptor{
			{Name: "id", ID: 1, Type: types.Int},
			{Name: "name", ID: 2, Type: types.String},
			{Name: "config", ID: 3, Type: types.Bytes},
		},
		NextColumnID: 4,
		Families: []descpb.ColumnFamilyDescriptor{
			{
				Name:        "primary",
				ID:          0,
				ColumnNames: []string{"id", "name", "config"},
				ColumnIDs:   []descpb.ColumnID{1, 2, 3},
			},
		},
		NextFamilyID: 1,
		PrimaryIndex: descpb.IndexDescriptor{
			Name:                "primary",
			ID:                  1,
			Unique:              true,
			KeyColumnNames:      []string{"id"},
			KeyColumnDirections: []catenumpb.IndexColumn_Direction{catenumpb.IndexColumn_ASC},
			KeyColumnIDs:        []descpb.ColumnID{1},
			ConstraintID:        2,
		},
		Indexes: []descpb.IndexDescriptor{{
			Name:                "resource_groups_name_idx",
			ID:                  2,
			Unique:              true,
			KeyColumnNames:      []string{"name"},
			KeyColumnDirections: []catenumpb.IndexColumn_Direction{catenumpb.IndexColumn_ASC},
			KeyColumnIDs:        []descpb.ColumnID{2},
			KeySuffixColumnIDs:  []descpb.ColumnID{1},
			Version:             descpb.StrictIndexColumnIDGuaranteesVersion,
			ConstraintID:        3,
		}},
		NextIndexID: 3,
		Checks: []*descpb.TableDescriptor_CheckConstraint{{
			Name:         "check_id_reserved_range",
			Expr:         descpb.Expression("id >= 16:::INT8"),
			ColumnIDs:    []descpb.ColumnID{1},
			ConstraintID: 1,
		}},
		NextConstraintID: 4,
		Privileges:       catpb.NewCustomSuperuserPrivilegeDescriptor(privilege.ReadWriteData, username.NodeUserName()),
		NextMutationID:   1,
		FormatVersion:    3,
	}
}
