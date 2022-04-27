// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package cli

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"os"

	"github.com/cockroachdb/cockroach/pkg/cli/clierrorplus"
	"github.com/cockroachdb/cockroach/pkg/cli/clisqlexec"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/errors"
	"github.com/spf13/cobra"
)

var badSplitOpts = struct {
	attemptMerge bool
}{}

var debugFixBadSplitsCmd = &cobra.Command{
	Use:   "find-bad-splits",
	Short: "Find ranges that end before the end of a SQL row",
	Long: `
A previous bug in the backup/restore code can produces ranges that are split at invalid boundaries.

This command finds such incorrect boundaries by merging to offending
ranges with the range on its right-hand side.

If the --attempt-merge flag is passed, the command attempts to merge all ranges.
`,
	RunE: clierrorplus.MaybeDecorateError(runDebugFixBadSplits),
}

func runDebugFixBadSplits(cmd *cobra.Command, _ []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rangesToMerge, err := findRangesWithBadSplitKeys(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to query ranges")
	}

	if len(rangesToMerge) == 0 {
		fmt.Println("No row splits found.")
		return nil
	}

	header := []string{"RangeID", "Key", "EndKey"}
	forPrinting := make([][]string, len(rangesToMerge))
	for i := range rangesToMerge {
		forPrinting[i] = []string{
			fmt.Sprintf("%d", rangesToMerge[i].rangeID),
			rangesToMerge[i].key.String(),
			rangesToMerge[i].endKey.String(),
		}
	}
	fmt.Println("Ranges with suspicious end keys found:")
	PrintQueryOutput(os.Stdout, header, clisqlexec.NewRowSliceIter(forPrinting, "lll"))

	if badSplitOpts.attemptMerge {
		if err := mergeRanges(ctx, rangesToMerge); err != nil {
			return nil
		}
		fmt.Println("find-bad-splits --attempt-merge should be called on all nodes in the cluster.")
	}

	return nil
}

type rangeToMerge struct {
	key     roachpb.Key
	endKey  roachpb.Key
	rangeID roachpb.RangeID
	storeID roachpb.StoreID
}

func findRangesWithBadSplitKeys(ctx context.Context) ([]rangeToMerge, error) {
	c, err := makeSQLClient("cockroach debug fix-bad-splits", useSystemDb)
	if err != nil {
		return nil, errors.Wrap(err, "could not establish connection to cluster")
	}
	defer c.Close()

	badRangesQuery := `SELECT range_id, start_key, end_key, lease_holder FROM crdb_internal.ranges`
	var rangesToMerge []rangeToMerge
	if rows, err := c.Query(ctx, badRangesQuery); err == nil {
		for {
			vals := make([]driver.Value, 4)
			if err := rows.Next(vals); err == io.EOF {
				break
			} else if err != nil {
				return nil, err
			}

			rangeID, ok := vals[0].(int64)
			if !ok {
				return nil, errors.New("failed to parse range_id")
			}
			startKey, ok := vals[1].([]byte)
			if !ok {
				return nil, errors.New("failed to parse start_key")
			}
			endKey, ok := vals[2].([]byte)
			if !ok {
				return nil, errors.New("failed to parse end_key")
			}
			storeID, ok := vals[3].(int64)
			if !ok {
				return nil, errors.New("failed to parse lease_holder")
			}

			colFamilyLen, err := decodeColumnFamilyLength(endKey)
			if err != nil {
				continue
			}
			// We consider any key with a non-zero length column family to be
			// a bad split since it implies we didn't call EnsureSafeSplitKey on it.
			if colFamilyLen > 0 {
				fmt.Printf("Adding suspicious range %d with colFamilyLen: %d\n", rangeID, colFamilyLen)
				rangesToMerge = append(rangesToMerge, rangeToMerge{
					rangeID: roachpb.RangeID(rangeID),
					storeID: roachpb.StoreID(storeID),
					key:     startKey,
					endKey:  endKey,
				})
			}
		}
	} else {
		return nil, err
	}
	return rangesToMerge, nil
}

func mergeRanges(ctx context.Context, rangesToMerge []rangeToMerge) error {
	cc, _, finish, err := getClientGRPCConn(ctx, serverCfg)
	if err != nil {
		return err
	}
	defer finish()

	client := roachpb.NewInternalClient(cc)
	for _, toMerge := range rangesToMerge {
		fmt.Printf("sending AdminMerge request for range %d to store %d\n", toMerge.rangeID, toMerge.storeID)
		_, err = client.Batch(ctx, &roachpb.BatchRequest{
			Header: roachpb.Header{
				RangeID: toMerge.rangeID,
				Replica: roachpb.ReplicaDescriptor{
					StoreID: toMerge.storeID,
				},
			},
			Requests: []roachpb.RequestUnion{
				{
					&roachpb.RequestUnion_AdminMerge{
						AdminMerge: &roachpb.AdminMergeRequest{
							RequestHeader: roachpb.RequestHeader{
								Key: toMerge.key,
							},
						},
					},
				},
			},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func SplitAtKey(ctx context.Context, key roachpb.Key) error {
	cc, _, finish, err := getClientGRPCConn(ctx, serverCfg)
	if err != nil {
		return err
	}
	defer finish()

	client := roachpb.NewInternalClient(cc)
	for _, s := range []int{1, 2, 3} {
		h := roachpb.RequestHeaderFromSpan(roachpb.Span{Key: key})
		_, err = client.Batch(ctx, &roachpb.BatchRequest{
			Header: roachpb.Header{
				RangeID: 45,
				Replica: roachpb.ReplicaDescriptor{
					StoreID: roachpb.StoreID(s),
				},
			},
			Requests: []roachpb.RequestUnion{
				{
					&roachpb.RequestUnion_AdminSplit{
						AdminSplit: &roachpb.AdminSplitRequest{
							RequestHeader: h,
							SplitKey:      key,
						},
					},
				},
			},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// decodeColumnFamily returns the length of the column family of the given key.
// It returns an error if they key isn't a valid table key.
//
// See keys.GrowRowPrefixLength
func decodeColumnFamilyLength(key roachpb.Key) (int, error) {
	// Strip tenant ID prefix to get a "SQL key" starting with a table ID.
	tableKey, _, err := keys.SystemSQLCodec.DecodeTablePrefix(key)
	if err != nil {
		return 0, errors.Errorf("%s: not a valid table key", key)
	}
	if len(tableKey) == 0 {
		return 0, nil
	}
	// The column family ID length is encoded as a varint and we take advantage
	// of the fact that the column family ID itself will be encoded in 0-9 bytes
	// and thus the length of the column family ID data will fit in a single
	// byte.
	colFamIDLenByte := tableKey[len(tableKey)-1:]
	if encoding.PeekType(colFamIDLenByte) != encoding.Int {
		// The last byte is not a valid column family ID suffix.
		return 0, errors.Errorf("%s: not a valid table key", key)
	}

	_, colFamilyIDLen, err := encoding.DecodeUvarintAscending(colFamIDLenByte)
	if err != nil {
		return 0, err
	}
	return int(colFamilyIDLen), nil
}

var debugMakeBadSplitsCmd = &cobra.Command{
	Use:   "make-bad-split",
	Short: "nogood",
	RunE:  clierrorplus.MaybeDecorateError(runDebugMakeBadSplit),
}

func runDebugMakeBadSplit(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, err := makeSQLClient("cockroach debug make-bad-splits", useSystemDb)
	if err != nil {
		return errors.Wrap(err, "could not establish connection to cluster")
	}
	defer c.Close()

	vals, err := c.QueryRow(ctx, `SELECT descriptor FROM system.descriptor WHERE id = 104`)
	if err != nil {
		return err
	}
	descProto := vals[0].([]byte)
	desc := &descpb.Descriptor{}
	if err := protoutil.Unmarshal(descProto, desc); err != nil {
		return err
	}
	tableDesc := tabledesc.NewBuilder(desc.GetTable()).BuildImmutableTable()
	index := tableDesc.GetPrimaryIndex()
	var colIDtoRowIndex catalog.TableColMap
	for i, c := range tableDesc.PublicColumns() {
		colIDtoRowIndex.Set(c.GetID(), i)
	}
	row := []tree.Datum{tree.NewDInt(20000), tree.NewDString("b")}
	indexEntries, err := rowenc.EncodeSecondaryIndex(
		keys.SystemSQLCodec, tableDesc, index, colIDtoRowIndex, row, true /* includeEmpty */)
	if err != nil {
		return err
	}
	splitKey := indexEntries[1].Key
	return SplitAtKey(ctx, splitKey)
}
