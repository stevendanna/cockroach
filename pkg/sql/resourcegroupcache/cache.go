// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package resourcegroupcache

import (
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/kvclient/rangefeed"
	"github.com/cockroachdb/cockroach/pkg/kv/kvclient/rangefeed/rangefeedbuffer"
	"github.com/cockroachdb/cockroach/pkg/kv/kvclient/rangefeed/rangefeedcache"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/systemschema"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/util/admission/admissionpb"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/startup"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

// readThroughTimeout bounds the synchronous read issued by NameToID
// on a cache miss. SET resource_group is on the foreground path of a
// SQL session and we'd rather return a clear timeout than block on a
// slow or unavailable system table.
const readThroughTimeout = 5 * time.Second

// rangefeedBufferSize bounds the rangefeed event buffer between
// checkpoints. The table is expected to hold tens of rows, so a
// generous bound is still tiny.
const rangefeedBufferSize = 4096

// Entry is a single resource group as the cache exposes it. The
// BurstFrac on Config and the marshaled ConfigBytes have been
// normalized across the full cohort at the rangefeed checkpoint that
// produced this Entry, so callers can ship ConfigBytes on the wire
// without further processing.
type Entry struct {
	ID          uint64
	Name        string
	Version     uint64
	Config      admissionpb.ResourceGroupConfig
	ConfigBytes []byte
}

// Cache maintains an in-memory mirror of the local tenant's
// system.resource_groups table.
//
// Lifecycle:
//   - byID and byName are written only at rangefeed checkpoint
//     boundaries, by onUpdate. Any Entry handed back from LookupByID
//     therefore reflects a consistent view of the table at a closed
//     timestamp, with BurstFrac normalized across the full cohort.
//   - NameToID may additionally write to byName before the next
//     checkpoint, populating it from a synchronous SELECT. This is
//     the SET resource_group fast path: a group CREATEd on a peer
//     node can be resolved by name before our rangefeed catches up.
//     Such an entry has a name->id mapping but no Entry in byID until
//     the rangefeed delivers the row; callers that miss in byID
//     should treat that as "id is known but config is not yet
//     available" and proceed without a wire-carried config.
type Cache struct {
	stopper *stop.Stopper
	clock   *hlc.Clock
	f       *rangefeed.Factory
	db      isql.DB
	dec     rowDecoder

	initialScanDone chan struct{}
	mu              struct {
		syncutil.RWMutex
		byID    map[uint64]Entry
		byName  map[string]uint64
		initErr error
	}
}

// New constructs a Cache. Start must be called before lookups return
// useful data.
func New(
	clock *hlc.Clock, f *rangefeed.Factory, stopper *stop.Stopper, db isql.DB, codec keys.SQLCodec,
) *Cache {
	c := &Cache{
		stopper:         stopper,
		clock:           clock,
		f:               f,
		db:              db,
		dec:             makeRowDecoder(codec),
		initialScanDone: make(chan struct{}),
	}
	c.mu.byID = map[uint64]Entry{}
	c.mu.byName = map[string]uint64{}
	return c
}

// Start opens the rangefeed and blocks until the initial scan
// completes (or fails). The rangefeed continues to run, applying
// incremental updates, until the stopper quiesces.
func (c *Cache) Start(ctx context.Context, sysTableResolver catalog.SystemTableIDResolver) error {
	tableID, err := startup.RunIdempotentWithRetryEx(ctx,
		c.stopper.ShouldQuiesce(),
		"resource-group cache table id lookup",
		func(ctx context.Context) (uint32, error) {
			id, err := sysTableResolver.LookupSystemTableID(ctx,
				systemschema.ResourceGroupsTable.GetName())
			return uint32(id), err
		})
	if err != nil {
		return err
	}
	prefix := c.dec.codec.TablePrefix(tableID)
	span := roachpb.Span{Key: prefix, EndKey: prefix.PrefixEnd()}

	translateEvent := func(ctx context.Context, kv *kvpb.RangeFeedValue) (rangefeedbuffer.Event, bool) {
		row, err := c.dec.decodeRow(roachpb.KeyValue{Key: kv.Key, Value: kv.Value}, nil)
		if err != nil {
			log.Dev.Warningf(ctx, "resource-group cache: failed to decode row %v: %v", kv.Key, err)
			return nil, false
		}
		return &rgEvent{row: row, ts: kv.Value.Timestamp}, true
	}

	initErrCh := make(chan error, 1)
	var initialScanReported bool
	onUpdate := func(ctx context.Context, update rangefeedcache.Update[rangefeedbuffer.Event]) {
		switch update.Type {
		case rangefeedcache.CompleteUpdate:
			c.replaceAll(ctx, update.Events)
			if !initialScanReported {
				initialScanReported = true
				initErrCh <- nil
				close(initErrCh)
			}
		case rangefeedcache.IncrementalUpdate:
			c.applyBatch(ctx, update.Events)
		}
	}

	onError := func(err error) {
		if !initialScanReported {
			initialScanReported = true
			initErrCh <- err
			close(initErrCh)
		}
		// Post-startup rangefeed failures cause rangefeedcache to restart
		// and re-scan from scratch; the next CompleteUpdate will replace
		// our current view. Nothing to do here.
	}

	w := rangefeedcache.NewWatcher(
		"resource-group-cache",
		c.clock, c.f,
		rangefeedBufferSize,
		[]roachpb.Span{span},
		false, /* withPrevValue */
		true,  /* withRowTSInInitialScan */
		translateEvent,
		onUpdate,
		nil, /* knobs */
	)
	if err := rangefeedcache.Start(ctx, c.stopper, w, onError); err != nil {
		return err
	}

	select {
	case err := <-initErrCh:
		c.mu.Lock()
		c.mu.initErr = err
		c.mu.Unlock()
		close(c.initialScanDone)
		return err
	case <-c.stopper.ShouldQuiesce():
		return errors.Wrap(stop.ErrUnavailable, "resource-group cache initial scan")
	case <-ctx.Done():
		return errors.Wrap(ctx.Err(), "resource-group cache initial scan")
	}
}

// WaitForStarted blocks until Start's initial scan completes (or
// fails) or the context is cancelled.
func (c *Cache) WaitForStarted(ctx context.Context) error {
	select {
	case <-c.initialScanDone:
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.mu.initErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// LookupByID returns the Entry for id, or ok=false if the rangefeed
// has not delivered it yet (or the row has been dropped). A miss here
// after a NameToID hit on the same name is the expected window
// between a peer-node CREATE and our local rangefeed catching up; the
// caller should proceed with the id alone and let the host fall back
// to the per-tenant default config until a subsequent request carries
// the real one.
func (c *Cache) LookupByID(id uint64) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.mu.byID[id]
	return e, ok
}

// NameToID resolves a resource group name to its id, falling back to
// a synchronous read of system.resource_groups on cache miss. The
// fallback handles the brief window between a CREATE on a peer node
// and our local rangefeed delivering the row.
//
// A successful read-through populates byName so subsequent calls
// resolve from cache; it does not populate byID, because BurstFrac is
// a cohort property that only becomes accurate at the next rangefeed
// checkpoint.
//
// Returns ok=false with nil err when the name truly does not exist.
func (c *Cache) NameToID(ctx context.Context, name string) (uint64, bool, error) {
	c.mu.RLock()
	id, ok := c.mu.byName[name]
	c.mu.RUnlock()
	if ok {
		return id, true, nil
	}
	return c.readThroughName(ctx, name)
}

func (c *Cache) readThroughName(ctx context.Context, name string) (uint64, bool, error) {
	var id uint64
	err := timeutil.RunWithTimeout(ctx, "resource-group read-through", readThroughTimeout,
		func(ctx context.Context) error {
			row, err := c.db.Executor().QueryRowEx(ctx, "resource-group-cache-read-through",
				nil, /* txn */
				sessiondata.NodeUserSessionDataOverride,
				`SELECT id FROM system.resource_groups WHERE name = $1`,
				name,
			)
			if err != nil {
				return err
			}
			if row == nil {
				return nil
			}
			id = uint64(tree.MustBeDInt(row[0]))
			return nil
		})
	if err != nil {
		return 0, false, errors.Wrap(err, "reading resource group by name")
	}
	if id == 0 {
		return 0, false, nil
	}
	c.mu.Lock()
	c.mu.byName[name] = id
	c.mu.Unlock()
	return id, true, nil
}

// rgEvent is the buffer item produced by translateEvent and consumed
// by onUpdate at checkpoint boundaries.
type rgEvent struct {
	row decodedRow
	ts  hlc.Timestamp
}

func (e *rgEvent) Timestamp() hlc.Timestamp { return e.ts }

// replaceAll handles a CompleteUpdate: rebuild both maps from the
// scan's events, then normalize the cohort. Any byName entry written
// by a NameToID read-through since startup is dropped here, which is
// fine: the rangefeed delivered the same data (or didn't), and the
// next read-through can re-populate.
func (c *Cache) replaceAll(ctx context.Context, events []rangefeedbuffer.Event) {
	byID := make(map[uint64]Entry, len(events))
	byName := make(map[string]uint64, len(events))
	for _, ev := range events {
		row := ev.(*rgEvent).row
		if row.tombstone {
			continue
		}
		e, err := makeEntry(row)
		if err != nil {
			log.Dev.Warningf(ctx, "resource-group cache: skipping row id %d: %v", row.id, err)
			continue
		}
		byID[e.ID] = e
		byName[e.Name] = e.ID
	}
	normalizeCohort(byID)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mu.byID = byID
	c.mu.byName = byName
}

// applyBatch handles an IncrementalUpdate: apply each event in order
// to the existing maps, then re-normalize the cohort if anything
// changed. Read-through-populated byName entries are preserved.
func (c *Cache) applyBatch(ctx context.Context, events []rangefeedbuffer.Event) {
	if len(events) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := false
	for _, ev := range events {
		row := ev.(*rgEvent).row
		if row.tombstone {
			prev, ok := c.mu.byID[row.id]
			if !ok {
				continue
			}
			delete(c.mu.byID, row.id)
			if c.mu.byName[prev.Name] == row.id {
				delete(c.mu.byName, prev.Name)
			}
			changed = true
			continue
		}
		e, err := makeEntry(row)
		if err != nil {
			log.Dev.Warningf(ctx, "resource-group cache: skipping row id %d: %v", row.id, err)
			continue
		}
		// Rename invalidates the old name->id mapping.
		if prev, ok := c.mu.byID[e.ID]; ok && prev.Name != e.Name {
			if c.mu.byName[prev.Name] == e.ID {
				delete(c.mu.byName, prev.Name)
			}
		}
		c.mu.byID[e.ID] = e
		c.mu.byName[e.Name] = e.ID
		changed = true
	}
	if changed {
		normalizeCohort(c.mu.byID)
	}
}

// makeEntry unmarshals the row's stored config and returns the Entry
// with ConfigBytes set to the raw stored bytes. The caller is
// expected to invoke normalizeCohort over the resulting map to fill
// in BurstFrac and rewrite ConfigBytes.
func makeEntry(row decodedRow) (Entry, error) {
	e := Entry{
		ID:      row.id,
		Name:    row.name,
		Version: row.version,
	}
	if len(row.configBytes) == 0 {
		return Entry{}, errors.AssertionFailedf("resource group %q (id %d) has empty config", row.name, row.id)
	}
	if err := protoutil.Unmarshal(row.configBytes, &e.Config); err != nil {
		return Entry{}, errors.Wrapf(err, "decoding config for resource group %q (id %d)", row.name, row.id)
	}
	return e, nil
}

// normalizeCohort fills in BurstFrac on every Entry's Config by
// running admissionpb.Normalize across the full cohort, then
// re-marshals each Entry.ConfigBytes from the normalized Config so
// the wire path can ship the bytes directly.
func normalizeCohort(byID map[uint64]Entry) {
	if len(byID) == 0 {
		return
	}
	ids := make([]uint64, 0, len(byID))
	cfgs := make([]admissionpb.ResourceGroupConfig, 0, len(byID))
	for id, e := range byID {
		ids = append(ids, id)
		cfgs = append(cfgs, e.Config)
	}
	admissionpb.Normalize(cfgs)
	for i, id := range ids {
		e := byID[id]
		e.Config = cfgs[i]
		bytes, err := protoutil.Marshal(&e.Config)
		if err != nil {
			// Re-marshaling a config we just decoded should not fail. If
			// it does, drop the entry rather than ship stale bytes.
			delete(byID, id)
			continue
		}
		e.ConfigBytes = bytes
		byID[id] = e
	}
}
