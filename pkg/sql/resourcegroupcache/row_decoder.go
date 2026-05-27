// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package resourcegroupcache

import (
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/systemschema"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc/valueside"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/errors"
)

// decodedRow is the raw output of decoding one system.resource_groups
// KV. Tombstone is true when the value side was empty (a delete) and
// only id is meaningful.
type decodedRow struct {
	id          uint64
	name        string
	configBytes []byte
	version     uint64
	tombstone   bool
}

// Names of the value-side columns we read. These are resolved to
// table ordinals once at construction so decodeRow doesn't bake in
// positional assumptions: adding or reordering columns in the
// descriptor doesn't silently change what we read.
const (
	nameColIdx = iota
	configColIdx
	versionColIdx
	numValueCols
)

var valueColumnNames = [numValueCols]string{
	nameColIdx:    "name",
	configColIdx:  "config",
	versionColIdx: "version",
}

// rowDecoder decodes primary-index rows of system.resource_groups.
// The id lives in the key; name, config, and version live in the
// "primary" family of the value tuple.
type rowDecoder struct {
	codec               keys.SQLCodec
	columns             []catalog.Column
	decoder             valueside.Decoder
	valueColumnOrdinals [numValueCols]int
}

func makeRowDecoder(codec keys.SQLCodec) rowDecoder {
	table := systemschema.ResourceGroupsTable
	columns := table.PublicColumns()
	d := rowDecoder{
		codec:   codec,
		columns: columns,
		decoder: valueside.MakeDecoder(columns),
	}
	for i := 0; i < numValueCols; i++ {
		col := catalog.FindColumnByName(table, valueColumnNames[i])
		d.valueColumnOrdinals[i] = col.Ordinal()
	}
	return d
}

func (d *rowDecoder) decodeRow(kv roachpb.KeyValue, alloc *tree.DatumAlloc) (decodedRow, error) {
	if alloc == nil {
		alloc = &tree.DatumAlloc{}
	}

	keyTypes := []*types.T{d.columns[0].GetType()}
	keyVals := make([]rowenc.EncDatum, 1)
	if _, err := rowenc.DecodeIndexKey(d.codec, keyVals, nil, kv.Key); err != nil {
		return decodedRow{}, errors.Wrap(err, "decoding resource_groups key")
	}
	if err := keyVals[0].EnsureDecoded(keyTypes[0], alloc); err != nil {
		return decodedRow{}, err
	}
	id := uint64(tree.MustBeDInt(keyVals[0].Datum))

	if !kv.Value.IsPresent() {
		return decodedRow{id: id, tombstone: true}, nil
	}

	bytes, err := kv.Value.GetTuple()
	if err != nil {
		return decodedRow{}, errors.Wrap(err, "reading resource_groups value tuple")
	}
	datums, err := d.decoder.Decode(alloc, bytes)
	if err != nil {
		return decodedRow{}, errors.Wrap(err, "decoding resource_groups value")
	}

	r := decodedRow{id: id}
	if datum := d.value(datums, nameColIdx); datum != tree.DNull {
		r.name = string(tree.MustBeDString(datum))
	}
	if datum := d.value(datums, configColIdx); datum != tree.DNull {
		r.configBytes = []byte(tree.MustBeDBytes(datum))
	}
	if datum := d.value(datums, versionColIdx); datum != tree.DNull {
		r.version = uint64(tree.MustBeDInt(datum))
	}
	return r, nil
}

// value returns datums[ord] or DNull when ord is past the end. The
// value tuple from a row encoded by an older schema may be shorter
// than the current column set; treating missing trailing columns as
// NULL matches what valueside.Decoder does for a column with no
// stored bytes.
func (d *rowDecoder) value(datums []tree.Datum, which int) tree.Datum {
	ord := d.valueColumnOrdinals[which]
	if ord >= len(datums) {
		return tree.DNull
	}
	return datums[ord]
}
