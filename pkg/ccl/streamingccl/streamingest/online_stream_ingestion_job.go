// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package streamingest

import (
	"context"
	"net/url"
	"time"

	"github.com/cockroachdb/cockroach/pkg/ccl/streamingccl"
	"github.com/cockroachdb/cockroach/pkg/ccl/streamingccl/streamclient"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobsprofiler"
	"github.com/cockroachdb/cockroach/pkg/repstream/streampb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/physicalplan"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/retry"
	"github.com/cockroachdb/cockroach/pkg/util/span"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/jackc/pgx/v4"
)

type onlineStreamIngestionResumer struct {
	job *jobs.Job
}

var _ jobs.Resumer = (*onlineStreamIngestionResumer)(nil)

// Resume is part of the jobs.Resumer interface.
func (r *onlineStreamIngestionResumer) Resume(ctx context.Context, execCtx interface{}) error {
	jobExecCtx := execCtx.(sql.JobExecContext)

	progress := r.job.Progress().Details.(*jobspb.Progress_ActiveReplicationProgress).ActiveReplicationProgress
	payload := r.job.Details().(jobspb.ActiveReplicationDetails)

	conn, err := connectToSource(ctx, payload.TargetClusterConnStr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	if len(progress.SourceSpans) == 0 {
		spec, err := startForTables(ctx, conn, &streampb.ReplicationProducerRequest{
			TableNames: payload.TableNames,
		})
		if err != nil {
			return err
		}
		log.Infof(ctx, "ReplicationProducerSpec: %#+v", spec)

		if err := r.job.NoTxn().Update(ctx, func(txn isql.Txn, md jobs.JobMetadata, ju *jobs.JobUpdater) error {
			prog := md.Progress.GetActiveReplicationProgress()
			prog.StreamID = uint64(spec.StreamID)
			prog.SourceClusterID = spec.SourceClusterID
			prog.SourceSpans = spec.TableSpans
			prog.ReplicationStartTime = spec.ReplicationStartTime
			prog.TableDescriptors = spec.TableDescriptors
			ju.UpdateProgress(md.Progress)
			return nil
		}); err != nil {
			return err
		}
	}
	return r.ingestWithRetries(ctx, jobExecCtx)
}

func (r *onlineStreamIngestionResumer) ingest(
	ctx context.Context, jobExecCtx sql.JobExecContext,
) error {

	var (
		execCfg        = jobExecCtx.ExecCfg()
		distSQLPlanner = jobExecCtx.DistSQLPlanner()
		evalCtx        = jobExecCtx.ExtendedEvalContext()

		progress = r.job.Progress().Details.(*jobspb.Progress_ActiveReplicationProgress).ActiveReplicationProgress
		payload  = r.job.Details().(jobspb.ActiveReplicationDetails)

		replicatedTimeAtStart = progress.ReplicatedTime
		sourceSpans           = progress.SourceSpans
		streamID              = progress.StreamID
	)
	conn, err := connectToSource(ctx, payload.TargetClusterConnStr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	frontier, err := span.MakeFrontierAt(replicatedTimeAtStart, sourceSpans...)
	if err != nil {
		return err
	}
	for _, resolvedSpan := range progress.Checkpoint.ResolvedSpans {
		if _, err := frontier.Forward(resolvedSpan.Span, resolvedSpan.Timestamp); err != nil {
			return err
		}
	}

	spans := [][]byte{}
	for _, sourceSpan := range sourceSpans {
		spanBytes, err := protoutil.Marshal(&sourceSpan)
		if err != nil {
			return err
		}
		spans = append(spans, spanBytes)
	}
	row := conn.QueryRow(ctx, "SELECT crdb_internal.partition_spans($1)", spans)
	streamSpecBytes := []byte{}
	streamSpec := streampb.ReplicationStreamSpec{}

	if err := row.Scan(&streamSpecBytes); err != nil {
		return err
	}

	if err := protoutil.Unmarshal(streamSpecBytes, &streamSpec); err != nil {
		return err
	}

	streamURL, err := url.Parse(payload.TargetClusterConnStr)
	if err != nil {
		return err
	}

	topology, err := streamclient.TopologyFromSpec(streamSpec, *streamURL)
	if err != nil {
		return err
	}

	planCtx, nodes, err := distSQLPlanner.SetupAllNodesPlanning(ctx, evalCtx, execCfg)
	if err != nil {
		return err
	}
	destNodeLocalities, err := getDestNodeLocalities(ctx, distSQLPlanner, nodes)
	if err != nil {
		return err
	}

	jobID := r.job.ID()
	specs := constructOnlineStreamIngestionDataSpecs(ctx,
		streamingccl.StreamAddress(payload.TargetClusterConnStr),
		topology,
		destNodeLocalities,
		progress.ReplicationStartTime,
		progress.ReplicatedTime,
		progress.Checkpoint,
		jobID,
		streampb.StreamID(streamID))

	// Setup a one-stage plan with one proc per input spec.
	processorCorePlacements := make([]physicalplan.ProcessorCorePlacement, 0, len(streamSpec.Partitions))

	for nodeID, parts := range specs {
		for _, part := range parts {
			processorCorePlacements = append(processorCorePlacements, physicalplan.ProcessorCorePlacement{
				SQLInstanceID: nodeID,
				Core: execinfrapb.ProcessorCoreUnion{
					Replication: &execinfrapb.ReplicationSpec{
						JobID:                       part.JobID,
						StreamID:                    part.StreamID,
						StreamAddress:               part.StreamAddress,
						PartitionSpecs:              part.PartitionSpecs,
						PreviousReplicatedTimestamp: part.PreviousReplicatedTimestamp,
						InitialScanTimestamp:        part.InitialScanTimestamp,
						Checkpoint:                  part.Checkpoint,
						TableDescriptors:            progress.TableDescriptors,
					},
				},
			})
		}
	}

	physicalPlan := planCtx.NewPhysicalPlan()
	physicalPlan.AddNoInputStage(
		processorCorePlacements,
		execinfrapb.PostProcessSpec{},
		onlineStreamIngestionResultType,
		execinfrapb.Ordering{},
	)
	physicalPlan.PlanToStreamColMap = []int{0}
	sql.FinalizePlan(ctx, planCtx, physicalPlan)

	jobsprofiler.StorePlanDiagram(ctx,
		execCfg.DistSQLSrv.Stopper,
		physicalPlan,
		execCfg.InternalDB,
		jobID)

	metaFn := func(_ context.Context, meta *execinfrapb.ProducerMetadata) error {
		log.Infof(ctx, "GOT META: %v", meta)
		return nil
	}

	var (
		lastPartitionUpdate time.Time
	)
	rowResultWriter := sql.NewCallbackResultWriter(func(ctx context.Context, row tree.Datums) error {
		raw, ok := row[0].(*tree.DBytes)
		if !ok {
			return errors.AssertionFailedf(`unexpected datum type %T`, row[0])
		}
		var resolvedSpans jobspb.ResolvedSpans
		if err := protoutil.Unmarshal([]byte(*raw), &resolvedSpans); err != nil {
			return errors.NewAssertionErrorWithWrappedErrf(err,
				`unmarshalling resolved timestamp: %x`, raw)
		}

		advanced := false
		for _, sp := range resolvedSpans.ResolvedSpans {
			adv, err := frontier.Forward(sp.Span, sp.Timestamp)
			if err != nil {
				return err
			}
			advanced = advanced || adv
		}

		if advanced {
			updateFreq := streamingccl.JobCheckpointFrequency.Get(&execCfg.Settings.SV)
			if updateFreq == 0 || timeutil.Since(lastPartitionUpdate) < updateFreq {
				return nil
			}

			frontierResolvedSpans := make([]jobspb.ResolvedSpan, 0)
			frontier.Entries(func(sp roachpb.Span, ts hlc.Timestamp) (done span.OpResult) {
				frontierResolvedSpans = append(frontierResolvedSpans, jobspb.ResolvedSpan{Span: sp, Timestamp: ts})
				return span.ContinueMatch
			})
			replicatedTime := frontier.Frontier()

			lastPartitionUpdate = timeutil.Now()
			log.VInfof(ctx, 2, "persisting replicated time of %s", replicatedTime)
			if err := r.job.NoTxn().Update(ctx,
				func(txn isql.Txn, md jobs.JobMetadata, ju *jobs.JobUpdater) error {
					if err := md.CheckRunningOrReverting(); err != nil {
						return err
					}
					progress := md.Progress
					prog := progress.Details.(*jobspb.Progress_ActiveReplicationProgress).ActiveReplicationProgress
					prog.Checkpoint.ResolvedSpans = frontierResolvedSpans
					if replicatedTimeAtStart.Less(replicatedTime) {
						prog.ReplicatedTime = replicatedTime
						// The HighWater is for informational purposes
						// only.
						progress.Progress = &jobspb.Progress_HighWater{
							HighWater: &replicatedTime,
						}
					}
					ju.UpdateProgress(progress)
					if md.RunStats != nil && md.RunStats.NumRuns > 1 {
						ju.UpdateRunStats(1, md.RunStats.LastRun)
					}
					return nil
				}); err != nil {
				return err
			}
		}
		return nil
	})

	distSQLReceiver := sql.MakeDistSQLReceiver(
		ctx,
		sql.NewMetadataCallbackWriter(rowResultWriter, metaFn),
		tree.Rows,
		execCfg.RangeDescriptorCache,
		nil, /* txn */
		nil, /* clockUpdater */
		evalCtx.Tracing,
	)
	defer distSQLReceiver.Release()
	// Copy the evalCtx, as dsp.Run() might change it.
	evalCtxCopy := *evalCtx
	distSQLPlanner.Run(
		ctx,
		planCtx,
		nil, /* txn */
		physicalPlan,
		distSQLReceiver,
		&evalCtxCopy,
		nil, /* finishedSetupFn */
	)

	return rowResultWriter.Err()
}

func (r *onlineStreamIngestionResumer) ingestWithRetries(
	ctx context.Context, execCtx sql.JobExecContext,
) error {
	ingestionJob := r.job
	ro := getRetryPolicy(execCtx.ExecCfg().StreamingTestingKnobs)
	var err error
	var lastReplicatedTime hlc.Timestamp
	for retrier := retry.Start(ro); retrier.Next(); {
		err = r.ingest(ctx, execCtx)
		if err == nil {
			break
		}
		// By default, all errors are retryable unless it's marked as
		// permanent job error in which case we pause the job.
		// We also stop the job when this is a context cancellation error
		// as requested pause or cancel will trigger a context cancellation.
		if jobs.IsPermanentJobError(err) || ctx.Err() != nil {
			break
		}

		log.Infof(ctx, "hit retryable error %s", err)
		newReplicatedTime := loadOnlineReplicatedTime(ctx, execCtx.ExecCfg().InternalDB, ingestionJob)
		if lastReplicatedTime.Less(newReplicatedTime) {
			retrier.Reset()
			lastReplicatedTime = newReplicatedTime
		}
		if knobs := execCtx.ExecCfg().StreamingTestingKnobs; knobs != nil && knobs.AfterRetryIteration != nil {
			knobs.AfterRetryIteration(err)
		}
	}
	return err
}

func loadOnlineReplicatedTime(
	ctx context.Context, db isql.DB, ingestionJob *jobs.Job,
) hlc.Timestamp {
	progress, err := jobs.LoadJobProgress(ctx, db, ingestionJob.ID())
	if err != nil {
		log.Warningf(ctx, "error loading job progress: %s", err)
		return hlc.Timestamp{}
	}
	if progress == nil {
		log.Warningf(ctx, "no job progress yet: %s", err)
		return hlc.Timestamp{}
	}
	return progress.Details.(*jobspb.Progress_ActiveReplicationProgress).ActiveReplicationProgress.ReplicatedTime
}

func connectToSource(ctx context.Context, address string) (*pgx.Conn, error) {
	log.Infof(ctx, "connecting to %s", address)
	streamAddr, err := url.Parse(address)
	if err != nil {
		return nil, err
	}

	conn, _, err := streamclient.NewPgConnectionForStream(ctx, streamAddr)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		if err := conn.Close(ctx); err != nil {
			log.Warningf(ctx, "error on close: %v", err)
		}
		return nil, err
	}
	return conn, nil
}

func startForTables(
	ctx context.Context, conn *pgx.Conn, req *streampb.ReplicationProducerRequest,
) (*streampb.ReplicationProducerSpec, error) {
	reqBytes, err := protoutil.Marshal(req)
	if err != nil {
		return nil, err
	}
	r := conn.QueryRow(ctx, "SELECT crdb_internal.start_replication_stream_for_tables($1)", reqBytes)
	specBytes := []byte{}
	if err := r.Scan(&specBytes); err != nil {
		return nil, err
	}

	spec := &streampb.ReplicationProducerSpec{}
	if err := protoutil.Unmarshal(specBytes, spec); err != nil {
		return nil, err
	}
	return spec, nil
}

// OnFailOrCancel implements jobs.Resumer interface
func (h *onlineStreamIngestionResumer) OnFailOrCancel(
	ctx context.Context, execCtx interface{}, _ error,
) error {
	return nil
}

// CollectProfile implements jobs.Resumer interface
func (h *onlineStreamIngestionResumer) CollectProfile(_ context.Context, _ interface{}) error {
	return nil
}

func init() {
	jobs.RegisterConstructor(
		jobspb.TypeActiveReplication,
		func(job *jobs.Job, _ *cluster.Settings) jobs.Resumer {
			return &onlineStreamIngestionResumer{
				job: job,
			}
		},
		jobs.UsesTenantCostControl,
	)
}
