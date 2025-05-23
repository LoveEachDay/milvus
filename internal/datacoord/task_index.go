// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package datacoord

import (
	"context"
	"path"
	"time"

	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/internal/types"
	"github.com/milvus-io/milvus/internal/util/vecindexmgr"
	"github.com/milvus-io/milvus/pkg/v2/common"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/proto/indexpb"
	"github.com/milvus-io/milvus/pkg/v2/proto/workerpb"
	"github.com/milvus-io/milvus/pkg/v2/util/indexparams"
	"github.com/milvus-io/milvus/pkg/v2/util/merr"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
	"github.com/milvus-io/milvus/pkg/v2/util/typeutil"
)

type indexBuildTask struct {
	taskID   int64
	nodeID   int64
	taskInfo *workerpb.IndexTaskInfo
	taskSlot int64

	queueTime time.Time
	startTime time.Time
	endTime   time.Time

	req *workerpb.CreateJobRequest
}

var _ Task = (*indexBuildTask)(nil)

func newIndexBuildTask(taskID int64, taskSlot int64) *indexBuildTask {
	return &indexBuildTask{
		taskID:   taskID,
		taskSlot: taskSlot,
		taskInfo: &workerpb.IndexTaskInfo{
			BuildID: taskID,
			State:   commonpb.IndexState_Unissued,
		},
	}
}

func (it *indexBuildTask) GetTaskID() int64 {
	return it.taskID
}

func (it *indexBuildTask) GetNodeID() int64 {
	return it.nodeID
}

func (it *indexBuildTask) ResetTask(mt *meta) {
}

func (it *indexBuildTask) SetQueueTime(t time.Time) {
	it.queueTime = t
}

func (it *indexBuildTask) GetQueueTime() time.Time {
	return it.queueTime
}

func (it *indexBuildTask) SetStartTime(t time.Time) {
	it.startTime = t
}

func (it *indexBuildTask) GetStartTime() time.Time {
	return it.startTime
}

func (it *indexBuildTask) SetEndTime(t time.Time) {
	it.endTime = t
}

func (it *indexBuildTask) GetEndTime() time.Time {
	return it.endTime
}

func (it *indexBuildTask) GetTaskType() string {
	return indexpb.JobType_JobTypeIndexJob.String()
}

func (it *indexBuildTask) CheckTaskHealthy(mt *meta) bool {
	job, exist := mt.indexMeta.GetIndexJob(it.GetTaskID())
	return exist && !job.IsDeleted
}

func (it *indexBuildTask) SetState(state indexpb.JobState, failReason string) {
	it.taskInfo.State = commonpb.IndexState(state)
	it.taskInfo.FailReason = failReason
}

func (it *indexBuildTask) GetState() indexpb.JobState {
	return indexpb.JobState(it.taskInfo.GetState())
}

func (it *indexBuildTask) GetFailReason() string {
	return it.taskInfo.FailReason
}

func (it *indexBuildTask) GetTaskSlot() int64 {
	return it.taskSlot
}

func (it *indexBuildTask) UpdateVersion(ctx context.Context, nodeID int64, meta *meta, compactionHandler compactionPlanContext) error {
	if err := meta.indexMeta.UpdateVersion(it.taskID, nodeID); err != nil {
		return err
	}
	it.nodeID = nodeID
	return nil
}

func (it *indexBuildTask) UpdateMetaBuildingState(meta *meta) error {
	return meta.indexMeta.BuildIndex(it.taskID)
}

func (it *indexBuildTask) PreCheck(ctx context.Context, dependency *taskScheduler) bool {
	segIndex, exist := dependency.meta.indexMeta.GetIndexJob(it.taskID)
	if !exist || segIndex == nil {
		log.Ctx(ctx).Info("index task has not exist in meta table, remove task", zap.Int64("taskID", it.taskID))
		it.SetState(indexpb.JobState_JobStateNone, "index task has not exist in meta table")
		return false
	}

	segment := dependency.meta.GetSegment(ctx, segIndex.SegmentID)
	if !isSegmentHealthy(segment) || !dependency.meta.indexMeta.IsIndexExist(segIndex.CollectionID, segIndex.IndexID) {
		log.Ctx(ctx).Info("task is no need to build index, remove it", zap.Int64("taskID", it.taskID))
		it.SetState(indexpb.JobState_JobStateNone, "task is no need to build index")
		return false
	}
	indexParams := dependency.meta.indexMeta.GetIndexParams(segIndex.CollectionID, segIndex.IndexID)
	indexType := GetIndexType(indexParams)
	if isNoTrainIndex(indexType) || segIndex.NumRows < Params.DataCoordCfg.MinSegmentNumRowsToEnableIndex.GetAsInt64() {
		log.Ctx(ctx).Info("segment does not need index really", zap.Int64("taskID", it.taskID),
			zap.Int64("segmentID", segIndex.SegmentID), zap.Int64("num rows", segIndex.NumRows))
		it.SetStartTime(time.Now())
		it.SetEndTime(time.Now())
		it.SetState(indexpb.JobState_JobStateFinished, "fake finished index success")
		return false
	}

	typeParams := dependency.meta.indexMeta.GetTypeParams(segIndex.CollectionID, segIndex.IndexID)

	fieldID := dependency.meta.indexMeta.GetFieldIDByIndexID(segIndex.CollectionID, segIndex.IndexID)

	binlogIDs := getBinLogIDs(segment, fieldID)
	totalRows := getTotalBinlogRows(segment, fieldID)

	// When new index parameters are added, these parameters need to be updated to ensure they are included during the index-building process.
	if vecindexmgr.GetVecIndexMgrInstance().IsVecIndex(indexType) && Params.KnowhereConfig.Enable.GetAsBool() {
		var ret error
		indexParams, ret = Params.KnowhereConfig.UpdateIndexParams(GetIndexType(indexParams), paramtable.BuildStage, indexParams)

		if ret != nil {
			log.Ctx(ctx).Warn("failed to update index build params defined in yaml", zap.Int64("taskID", it.taskID), zap.Error(ret))
			it.SetState(indexpb.JobState_JobStateInit, ret.Error())
			return false
		}
	}

	if isDiskANNIndex(GetIndexType(indexParams)) {
		var err error
		indexParams, err = indexparams.UpdateDiskIndexBuildParams(Params, indexParams)
		if err != nil {
			log.Ctx(ctx).Warn("failed to append index build params", zap.Int64("taskID", it.taskID), zap.Error(err))
			it.SetState(indexpb.JobState_JobStateInit, err.Error())
			return false
		}
	}

	collectionInfo, err := dependency.handler.GetCollection(ctx, segment.GetCollectionID())
	if err != nil {
		log.Ctx(ctx).Info("index builder get collection info failed", zap.Int64("collectionID", segment.GetCollectionID()), zap.Error(err))
		return false
	}

	schema := collectionInfo.Schema
	var field *schemapb.FieldSchema

	for _, f := range schema.Fields {
		if f.FieldID == fieldID {
			field = f
			break
		}
	}

	// Extract dim only for vector types to avoid unnecessary warnings
	dim := -1
	if typeutil.IsFixDimVectorType(field.GetDataType()) {
		if dimVal, err := storage.GetDimFromParams(field.GetTypeParams()); err != nil {
			log.Ctx(ctx).Warn("failed to get dim from field type params",
				zap.String("field type", field.GetDataType().String()), zap.Error(err))
		} else {
			dim = dimVal
		}
	}

	// vector index build needs information of optional scalar fields data
	optionalFields := make([]*indexpb.OptionalFieldInfo, 0)
	partitionKeyIsolation := false
	isVectorTypeSupported := typeutil.IsDenseFloatVectorType(field.DataType) || typeutil.IsBinaryVectorType(field.DataType)
	if Params.CommonCfg.EnableMaterializedView.GetAsBool() && isVectorTypeSupported && isMvSupported(indexType) {
		if collectionInfo == nil {
			log.Ctx(ctx).Warn("get collection failed", zap.Int64("collID", segIndex.CollectionID), zap.Error(err))
			it.SetState(indexpb.JobState_JobStateInit, err.Error())
			return true
		}
		partitionKeyField, _ := typeutil.GetPartitionKeyFieldSchema(schema)
		if partitionKeyField != nil && typeutil.IsFieldDataTypeSupportMaterializedView(partitionKeyField) {
			optionalFields = append(optionalFields, &indexpb.OptionalFieldInfo{
				FieldID:   partitionKeyField.FieldID,
				FieldName: partitionKeyField.Name,
				FieldType: int32(partitionKeyField.DataType),
				DataIds:   getBinLogIDs(segment, partitionKeyField.FieldID),
			})
			iso, isoErr := common.IsPartitionKeyIsolationPropEnabled(collectionInfo.Properties)
			if isoErr != nil {
				log.Ctx(ctx).Warn("failed to parse partition key isolation", zap.Error(isoErr))
			}
			if iso {
				partitionKeyIsolation = true
			}
		}
	}
	indexNonEncoding := "false"
	if dependency.indexEngineVersionManager.GetIndexNonEncoding() {
		indexNonEncoding = "true"
	}
	indexParams = append(indexParams, &commonpb.KeyValuePair{
		Key:   common.IndexNonEncoding,
		Value: indexNonEncoding,
	})
	it.req = &workerpb.CreateJobRequest{
		ClusterID:                 Params.CommonCfg.ClusterPrefix.GetValue(),
		IndexFilePrefix:           path.Join(dependency.chunkManager.RootPath(), common.SegmentIndexPath),
		BuildID:                   it.taskID,
		IndexVersion:              segIndex.IndexVersion + 1,
		StorageConfig:             createStorageConfig(),
		IndexParams:               indexParams,
		TypeParams:                typeParams,
		NumRows:                   segIndex.NumRows,
		CurrentIndexVersion:       dependency.indexEngineVersionManager.GetCurrentIndexEngineVersion(),
		CurrentScalarIndexVersion: dependency.indexEngineVersionManager.GetCurrentScalarIndexEngineVersion(),
		CollectionID:              segment.GetCollectionID(),
		PartitionID:               segment.GetPartitionID(),
		SegmentID:                 segment.GetID(),
		FieldID:                   fieldID,
		FieldName:                 field.GetName(),
		FieldType:                 field.GetDataType(),
		Dim:                       int64(dim),
		DataIds:                   binlogIDs,
		OptionalScalarFields:      optionalFields,
		Field:                     field,
		PartitionKeyIsolation:     partitionKeyIsolation,
		StorageVersion:            segment.StorageVersion,
		TaskSlot:                  it.taskSlot,
	}

	it.req.LackBinlogRows = it.req.NumRows - totalRows

	log.Ctx(ctx).Info("index task pre check successfully", zap.Int64("taskID", it.GetTaskID()),
		zap.Int64("segID", segment.GetID()),
		zap.Int32("CurrentIndexVersion", it.req.GetCurrentIndexVersion()),
		zap.Int32("CurrentScalarIndexVersion", it.req.GetCurrentScalarIndexVersion()),
		zap.String("IndexNonEncoding", indexNonEncoding),
		zap.Int64("segID", segment.GetID()))
	return true
}

func (it *indexBuildTask) AssignTask(ctx context.Context, client types.DataNodeClient, meta *meta) bool {
	ctx, cancel := context.WithTimeout(ctx, Params.DataCoordCfg.RequestTimeoutSeconds.GetAsDuration(time.Second))
	defer cancel()
	resp, err := client.CreateJobV2(ctx, &workerpb.CreateJobV2Request{
		ClusterID: it.req.GetClusterID(),
		TaskID:    it.req.GetBuildID(),
		JobType:   indexpb.JobType_JobTypeIndexJob,
		Request: &workerpb.CreateJobV2Request_IndexRequest{
			IndexRequest: it.req,
		},
	})
	if err == nil {
		err = merr.Error(resp)
	}
	if err != nil {
		log.Ctx(ctx).Warn("assign index task to indexNode failed", zap.Int64("taskID", it.taskID), zap.Error(err))
		it.SetState(indexpb.JobState_JobStateRetry, err.Error())
		return false
	}

	log.Ctx(ctx).Info("index task assigned successfully", zap.Int64("taskID", it.taskID))
	it.SetState(indexpb.JobState_JobStateInProgress, "")
	return true
}

func (it *indexBuildTask) setResult(info *workerpb.IndexTaskInfo) {
	it.taskInfo = info
}

func (it *indexBuildTask) QueryResult(ctx context.Context, node types.DataNodeClient) {
	ctx, cancel := context.WithTimeout(ctx, Params.DataCoordCfg.RequestTimeoutSeconds.GetAsDuration(time.Second))
	defer cancel()
	resp, err := node.QueryJobsV2(ctx, &workerpb.QueryJobsV2Request{
		ClusterID: Params.CommonCfg.ClusterPrefix.GetValue(),
		TaskIDs:   []UniqueID{it.GetTaskID()},
		JobType:   indexpb.JobType_JobTypeIndexJob,
	})
	if err == nil {
		err = merr.Error(resp.GetStatus())
	}
	if err != nil {
		log.Ctx(ctx).Warn("get jobs info from IndexNode failed", zap.Int64("taskID", it.GetTaskID()),
			zap.Int64("nodeID", it.GetNodeID()), zap.Error(err))
		it.SetState(indexpb.JobState_JobStateRetry, err.Error())
		return
	}

	// indexInfos length is always one.
	for _, info := range resp.GetIndexJobResults().GetResults() {
		if info.GetBuildID() == it.GetTaskID() {
			if info.GetState() == commonpb.IndexState_Finished || info.GetState() == commonpb.IndexState_Failed ||
				info.GetState() == commonpb.IndexState_Retry {
				log.Ctx(ctx).Info("query task index info successfully",
					zap.Int64("taskID", it.GetTaskID()), zap.String("result state", info.GetState().String()),
					zap.String("failReason", info.GetFailReason()))
				// state is retry or finished or failed
				it.setResult(info)
			} else if info.GetState() == commonpb.IndexState_IndexStateNone {
				log.Ctx(ctx).Info("query task index info successfully",
					zap.Int64("taskID", it.GetTaskID()), zap.String("result state", info.GetState().String()),
					zap.String("failReason", info.GetFailReason()))
				it.SetState(indexpb.JobState_JobStateRetry, "index state is none in info response")
			}
			// inProgress or unissued, keep InProgress state
			return
		}
	}
	it.SetState(indexpb.JobState_JobStateRetry, "index is not in info response")
}

func (it *indexBuildTask) DropTaskOnWorker(ctx context.Context, client types.DataNodeClient) bool {
	ctx, cancel := context.WithTimeout(ctx, Params.DataCoordCfg.RequestTimeoutSeconds.GetAsDuration(time.Second))
	defer cancel()
	resp, err := client.DropJobsV2(ctx, &workerpb.DropJobsV2Request{
		ClusterID: Params.CommonCfg.ClusterPrefix.GetValue(),
		TaskIDs:   []UniqueID{it.GetTaskID()},
		JobType:   indexpb.JobType_JobTypeIndexJob,
	})
	if err == nil {
		err = merr.Error(resp)
	}
	if err != nil {
		log.Ctx(ctx).Warn("notify worker drop the index task fail", zap.Int64("taskID", it.GetTaskID()),
			zap.Int64("nodeID", it.GetNodeID()), zap.Error(err))
		return false
	}
	log.Ctx(ctx).Info("drop index task on worker success", zap.Int64("taskID", it.GetTaskID()),
		zap.Int64("nodeID", it.GetNodeID()))
	return true
}

func (it *indexBuildTask) SetJobInfo(meta *meta) error {
	return meta.indexMeta.FinishTask(it.taskInfo)
}

func (it *indexBuildTask) DropTaskMeta(ctx context.Context, meta *meta) error {
	return meta.indexMeta.RemoveSegmentIndexByID(ctx, it.taskID)
}
