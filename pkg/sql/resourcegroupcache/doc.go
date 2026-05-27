// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package resourcegroupcache holds a per-tenant in-memory mirror of
// system.resource_groups. The mirror is kept fresh by a rangefeed on
// the table's primary index; callers that need a row the rangefeed has
// not yet delivered (e.g. SET resource_group on a session that just
// CREATEd the group on a different node) can fall back to a synchronous
// SQL read-through.
//
// One Cache lives per SQL server. It is not safe to share a Cache
// across tenants: each tenant's rangefeed runs over its own copy of
// the system table.
package resourcegroupcache
