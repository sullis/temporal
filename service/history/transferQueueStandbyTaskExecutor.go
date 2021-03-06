// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package history

import (
	"time"

	"go.temporal.io/temporal-proto/serviceerror"
	taskqueuepb "go.temporal.io/temporal-proto/taskqueue/v1"

	enumsgenpb "github.com/temporalio/temporal/.gen/proto/enums/v1"
	"github.com/temporalio/temporal/.gen/proto/persistenceblobs/v1"
	"github.com/temporalio/temporal/common"
	"github.com/temporalio/temporal/common/log"
	"github.com/temporalio/temporal/common/log/tag"
	"github.com/temporalio/temporal/common/metrics"
	"github.com/temporalio/temporal/common/xdc"
)

type (
	transferQueueStandbyTaskExecutor struct {
		*transferQueueTaskExecutorBase

		clusterName         string
		historyRereplicator xdc.HistoryRereplicator
		nDCHistoryResender  xdc.NDCHistoryResender
	}
)

func newTransferQueueStandbyTaskExecutor(
	shard ShardContext,
	historyService *historyEngineImpl,
	historyRereplicator xdc.HistoryRereplicator,
	nDCHistoryResender xdc.NDCHistoryResender,
	logger log.Logger,
	metricsClient metrics.Client,
	clusterName string,
	config *Config,
) queueTaskExecutor {
	return &transferQueueStandbyTaskExecutor{
		transferQueueTaskExecutorBase: newTransferQueueTaskExecutorBase(
			shard,
			historyService,
			logger,
			metricsClient,
			config,
		),
		clusterName:         clusterName,
		historyRereplicator: historyRereplicator,
		nDCHistoryResender:  nDCHistoryResender,
	}
}

func (t *transferQueueStandbyTaskExecutor) execute(
	taskInfo queueTaskInfo,
	shouldProcessTask bool,
) error {

	transferTask, ok := taskInfo.(*persistenceblobs.TransferTaskInfo)
	if !ok {
		return errUnexpectedQueueTask
	}

	if !shouldProcessTask &&
		transferTask.TaskType != enumsgenpb.TASK_TYPE_TRANSFER_CLOSE_EXECUTION {
		// guarantee the processing of workflow execution close
		return nil
	}

	switch transferTask.TaskType {
	case enumsgenpb.TASK_TYPE_TRANSFER_ACTIVITY_TASK:
		return t.processActivityTask(transferTask)
	case enumsgenpb.TASK_TYPE_TRANSFER_DECISION_TASK:
		return t.processDecisionTask(transferTask)
	case enumsgenpb.TASK_TYPE_TRANSFER_CLOSE_EXECUTION:
		return t.processCloseExecution(transferTask)
	case enumsgenpb.TASK_TYPE_TRANSFER_CANCEL_EXECUTION:
		return t.processCancelExecution(transferTask)
	case enumsgenpb.TASK_TYPE_TRANSFER_SIGNAL_EXECUTION:
		return t.processSignalExecution(transferTask)
	case enumsgenpb.TASK_TYPE_TRANSFER_START_CHILD_EXECUTION:
		return t.processStartChildExecution(transferTask)
	case enumsgenpb.TASK_TYPE_TRANSFER_RECORD_WORKFLOW_STARTED:
		return t.processRecordWorkflowStarted(transferTask)
	case enumsgenpb.TASK_TYPE_TRANSFER_RESET_WORKFLOW:
		// no reset needed for standby
		// TODO: add error logs
		return nil
	case enumsgenpb.TASK_TYPE_TRANSFER_UPSERT_WORKFLOW_SEARCH_ATTRIBUTES:
		return t.processUpsertWorkflowSearchAttributes(transferTask)
	default:
		return errUnknownTransferTask
	}
}

func (t *transferQueueStandbyTaskExecutor) processActivityTask(
	transferTask *persistenceblobs.TransferTaskInfo,
) error {

	processTaskIfClosed := false
	actionFn := func(context workflowExecutionContext, mutableState mutableState) (interface{}, error) {

		activityInfo, ok := mutableState.GetActivityInfo(transferTask.GetScheduleId())
		if !ok {
			return nil, nil
		}

		ok, err := verifyTaskVersion(t.shard, t.logger, transferTask.GetNamespaceId(), activityInfo.Version, transferTask.Version, transferTask)
		if err != nil || !ok {
			return nil, err
		}

		if activityInfo.StartedID == common.EmptyEventID {
			return newPushActivityToMatchingInfo(
				activityInfo.ScheduleToStartTimeout,
			), nil
		}

		return nil, nil
	}

	return t.processTransfer(
		processTaskIfClosed,
		transferTask,
		actionFn,
		getStandbyPostActionFn(
			transferTask,
			t.getCurrentTime,
			t.config.StandbyTaskMissingEventsResendDelay(),
			t.config.StandbyTaskMissingEventsDiscardDelay(),
			t.pushActivity,
			t.pushActivity,
		),
	)
}

func (t *transferQueueStandbyTaskExecutor) processDecisionTask(
	transferTask *persistenceblobs.TransferTaskInfo,
) error {

	processTaskIfClosed := false
	actionFn := func(context workflowExecutionContext, mutableState mutableState) (interface{}, error) {

		decisionInfo, ok := mutableState.GetDecisionInfo(transferTask.GetScheduleId())
		if !ok {
			return nil, nil
		}

		executionInfo := mutableState.GetExecutionInfo()
		workflowTimeout := executionInfo.WorkflowRunTimeout
		decisionTimeout := common.MinInt32(workflowTimeout, common.MaxTaskTimeout)

		ok, err := verifyTaskVersion(t.shard, t.logger, transferTask.GetNamespaceId(), decisionInfo.Version, transferTask.Version, transferTask)
		if err != nil || !ok {
			return nil, err
		}

		if decisionInfo.StartedID == common.EmptyEventID {
			return newPushDecisionToMatchingInfo(
				decisionTimeout,
				taskqueuepb.TaskQueue{Name: transferTask.TaskQueue},
			), nil
		}

		return nil, nil
	}

	return t.processTransfer(
		processTaskIfClosed,
		transferTask,
		actionFn,
		getStandbyPostActionFn(
			transferTask,
			t.getCurrentTime,
			t.config.StandbyTaskMissingEventsResendDelay(),
			t.config.StandbyTaskMissingEventsDiscardDelay(),
			t.pushDecision,
			t.pushDecision,
		),
	)
}

func (t *transferQueueStandbyTaskExecutor) processCloseExecution(
	transferTask *persistenceblobs.TransferTaskInfo,
) error {

	processTaskIfClosed := true
	actionFn := func(context workflowExecutionContext, mutableState mutableState) (interface{}, error) {

		if mutableState.IsWorkflowExecutionRunning() {
			// this can happen if workflow is reset.
			return nil, nil
		}

		completionEvent, err := mutableState.GetCompletionEvent()
		if err != nil {
			return nil, err
		}
		wfCloseTime := completionEvent.GetTimestamp()

		executionInfo := mutableState.GetExecutionInfo()
		workflowTypeName := executionInfo.WorkflowTypeName
		workflowCloseTimestamp := wfCloseTime
		workflowStatus := executionInfo.Status
		workflowHistoryLength := mutableState.GetNextEventID() - 1
		startEvent, err := mutableState.GetStartEvent()
		if err != nil {
			return nil, err
		}
		workflowStartTimestamp := startEvent.GetTimestamp()
		workflowExecutionTimestamp := getWorkflowExecutionTimestamp(mutableState, startEvent)
		visibilityMemo := getWorkflowMemo(executionInfo.Memo)
		searchAttr := executionInfo.SearchAttributes

		lastWriteVersion, err := mutableState.GetLastWriteVersion()
		if err != nil {
			return nil, err
		}
		ok, err := verifyTaskVersion(t.shard, t.logger, transferTask.GetNamespaceId(), lastWriteVersion, transferTask.Version, transferTask)
		if err != nil || !ok {
			return nil, err
		}

		// DO NOT REPLY TO PARENT
		// since event replication should be done by active cluster
		return nil, t.recordWorkflowClosed(
			transferTask.GetNamespaceId(),
			transferTask.GetWorkflowId(),
			transferTask.GetRunId(),
			workflowTypeName,
			workflowStartTimestamp,
			workflowExecutionTimestamp.UnixNano(),
			workflowCloseTimestamp,
			workflowStatus,
			workflowHistoryLength,
			transferTask.GetTaskId(),
			visibilityMemo,
			executionInfo.TaskQueue,
			searchAttr,
		)
	}

	return t.processTransfer(
		processTaskIfClosed,
		transferTask,
		actionFn,
		standbyTaskPostActionNoOp,
	) // no op post action, since the entire workflow is finished
}

func (t *transferQueueStandbyTaskExecutor) processCancelExecution(
	transferTask *persistenceblobs.TransferTaskInfo,
) error {

	processTaskIfClosed := false
	actionFn := func(context workflowExecutionContext, mutableState mutableState) (interface{}, error) {

		requestCancelInfo, ok := mutableState.GetRequestCancelInfo(transferTask.GetScheduleId())
		if !ok {
			return nil, nil
		}

		ok, err := verifyTaskVersion(t.shard, t.logger, transferTask.GetNamespaceId(), requestCancelInfo.Version, transferTask.Version, transferTask)
		if err != nil || !ok {
			return nil, err
		}

		return getHistoryResendInfo(mutableState)
	}

	return t.processTransfer(
		processTaskIfClosed,
		transferTask,
		actionFn,
		getStandbyPostActionFn(
			transferTask,
			t.getCurrentTime,
			t.config.StandbyTaskMissingEventsResendDelay(),
			t.config.StandbyTaskMissingEventsDiscardDelay(),
			t.fetchHistoryFromRemote,
			standbyTransferTaskPostActionTaskDiscarded,
		),
	)
}

func (t *transferQueueStandbyTaskExecutor) processSignalExecution(
	transferTask *persistenceblobs.TransferTaskInfo,
) error {

	processTaskIfClosed := false
	actionFn := func(context workflowExecutionContext, mutableState mutableState) (interface{}, error) {

		signalInfo, ok := mutableState.GetSignalInfo(transferTask.GetScheduleId())
		if !ok {
			return nil, nil
		}

		ok, err := verifyTaskVersion(t.shard, t.logger, transferTask.GetNamespaceId(), signalInfo.Version, transferTask.Version, transferTask)
		if err != nil || !ok {
			return nil, err
		}

		return getHistoryResendInfo(mutableState)
	}

	return t.processTransfer(
		processTaskIfClosed,
		transferTask,
		actionFn,
		getStandbyPostActionFn(
			transferTask,
			t.getCurrentTime,
			t.config.StandbyTaskMissingEventsResendDelay(),
			t.config.StandbyTaskMissingEventsDiscardDelay(),
			t.fetchHistoryFromRemote,
			standbyTransferTaskPostActionTaskDiscarded,
		),
	)
}

func (t *transferQueueStandbyTaskExecutor) processStartChildExecution(
	transferTask *persistenceblobs.TransferTaskInfo,
) error {

	processTaskIfClosed := false
	actionFn := func(context workflowExecutionContext, mutableState mutableState) (interface{}, error) {

		childWorkflowInfo, ok := mutableState.GetChildExecutionInfo(transferTask.GetScheduleId())
		if !ok {
			return nil, nil
		}

		ok, err := verifyTaskVersion(t.shard, t.logger, transferTask.GetNamespaceId(), childWorkflowInfo.Version, transferTask.Version, transferTask)
		if err != nil || !ok {
			return nil, err
		}

		if childWorkflowInfo.StartedID != common.EmptyEventID {
			return nil, nil
		}

		return getHistoryResendInfo(mutableState)
	}

	return t.processTransfer(
		processTaskIfClosed,
		transferTask,
		actionFn,
		getStandbyPostActionFn(
			transferTask,
			t.getCurrentTime,
			t.config.StandbyTaskMissingEventsResendDelay(),
			t.config.StandbyTaskMissingEventsDiscardDelay(),
			t.fetchHistoryFromRemote,
			standbyTransferTaskPostActionTaskDiscarded,
		),
	)
}

func (t *transferQueueStandbyTaskExecutor) processRecordWorkflowStarted(
	transferTask *persistenceblobs.TransferTaskInfo,
) error {

	processTaskIfClosed := false
	return t.processTransfer(
		processTaskIfClosed,
		transferTask,
		func(context workflowExecutionContext, mutableState mutableState) (interface{}, error) {
			return nil, t.processRecordWorkflowStartedOrUpsertHelper(transferTask, mutableState, true)
		},
		standbyTaskPostActionNoOp,
	)
}

func (t *transferQueueStandbyTaskExecutor) processUpsertWorkflowSearchAttributes(
	transferTask *persistenceblobs.TransferTaskInfo,
) error {

	processTaskIfClosed := false
	return t.processTransfer(
		processTaskIfClosed,
		transferTask,
		func(context workflowExecutionContext, mutableState mutableState) (interface{}, error) {
			return nil, t.processRecordWorkflowStartedOrUpsertHelper(transferTask, mutableState, false)
		},
		standbyTaskPostActionNoOp,
	)
}

func (t *transferQueueStandbyTaskExecutor) processRecordWorkflowStartedOrUpsertHelper(
	transferTask *persistenceblobs.TransferTaskInfo,
	mutableState mutableState,
	isRecordStart bool,
) error {

	// verify task version for RecordWorkflowStarted.
	// upsert doesn't require verifyTask, because it is just a sync of mutableState.
	if isRecordStart {
		startVersion, err := mutableState.GetStartVersion()
		if err != nil {
			return err
		}
		ok, err := verifyTaskVersion(t.shard, t.logger, transferTask.GetNamespaceId(), startVersion, transferTask.Version, transferTask)
		if err != nil || !ok {
			return err
		}
	}

	executionInfo := mutableState.GetExecutionInfo()
	workflowTimeout := executionInfo.WorkflowRunTimeout
	wfTypeName := executionInfo.WorkflowTypeName
	startEvent, err := mutableState.GetStartEvent()
	if err != nil {
		return err
	}
	startTimestamp := startEvent.GetTimestamp()
	executionTimestamp := getWorkflowExecutionTimestamp(mutableState, startEvent)
	visibilityMemo := getWorkflowMemo(executionInfo.Memo)
	searchAttr := copySearchAttributes(executionInfo.SearchAttributes)

	if isRecordStart {
		return t.recordWorkflowStarted(
			transferTask.GetNamespaceId(),
			transferTask.GetWorkflowId(),
			transferTask.GetRunId(),
			wfTypeName,
			startTimestamp,
			executionTimestamp.UnixNano(),
			workflowTimeout,
			transferTask.GetTaskId(),
			executionInfo.TaskQueue,
			visibilityMemo,
			searchAttr,
		)
	}
	return t.upsertWorkflowExecution(
		transferTask.GetNamespaceId(),
		transferTask.GetWorkflowId(),
		transferTask.GetRunId(),
		wfTypeName,
		startTimestamp,
		executionTimestamp.UnixNano(),
		workflowTimeout,
		transferTask.GetTaskId(),
		executionInfo.TaskQueue,
		visibilityMemo,
		searchAttr,
	)

}

func (t *transferQueueStandbyTaskExecutor) processTransfer(
	processTaskIfClosed bool,
	taskInfo queueTaskInfo,
	actionFn standbyActionFn,
	postActionFn standbyPostActionFn,
) (retError error) {

	transferTask := taskInfo.(*persistenceblobs.TransferTaskInfo)
	context, release, err := t.cache.getOrCreateWorkflowExecutionForBackground(
		t.getNamespaceIDAndWorkflowExecution(transferTask),
	)
	if err != nil {
		return err
	}
	defer func() {
		if retError == ErrTaskRetry {
			release(nil)
		} else {
			release(retError)
		}
	}()

	mutableState, err := loadMutableStateForTransferTask(context, transferTask, t.metricsClient, t.logger)
	if err != nil || mutableState == nil {
		return err
	}

	if !mutableState.IsWorkflowExecutionRunning() && !processTaskIfClosed {
		// workflow already finished, no need to process the timer
		return nil
	}

	historyResendInfo, err := actionFn(context, mutableState)
	if err != nil {
		return err
	}

	release(nil)
	return postActionFn(taskInfo, historyResendInfo, t.logger)
}

func (t *transferQueueStandbyTaskExecutor) pushActivity(
	task queueTaskInfo,
	postActionInfo interface{},
	logger log.Logger,
) error {

	if postActionInfo == nil {
		return nil
	}

	pushActivityInfo := postActionInfo.(*pushActivityToMatchingInfo)
	timeout := common.MinInt32(pushActivityInfo.activityScheduleToStartTimeout, common.MaxTaskTimeout)
	return t.transferQueueTaskExecutorBase.pushActivity(
		task.(*persistenceblobs.TransferTaskInfo),
		timeout,
	)
}

func (t *transferQueueStandbyTaskExecutor) pushDecision(
	task queueTaskInfo,
	postActionInfo interface{},
	logger log.Logger,
) error {

	if postActionInfo == nil {
		return nil
	}

	pushDecisionInfo := postActionInfo.(*pushDecisionToMatchingInfo)
	timeout := common.MinInt32(pushDecisionInfo.decisionScheduleToStartTimeout, common.MaxTaskTimeout)
	return t.transferQueueTaskExecutorBase.pushDecision(
		task.(*persistenceblobs.TransferTaskInfo),
		&pushDecisionInfo.taskqueue,
		timeout,
	)
}

func (t *transferQueueStandbyTaskExecutor) fetchHistoryFromRemote(
	taskInfo queueTaskInfo,
	postActionInfo interface{},
	log log.Logger,
) error {

	if postActionInfo == nil {
		return nil
	}

	transferTask := taskInfo.(*persistenceblobs.TransferTaskInfo)
	resendInfo := postActionInfo.(*historyResendInfo)

	t.metricsClient.IncCounter(metrics.HistoryRereplicationByTransferTaskScope, metrics.ClientRequests)
	stopwatch := t.metricsClient.StartTimer(metrics.HistoryRereplicationByTransferTaskScope, metrics.ClientLatency)
	defer stopwatch.Stop()

	var err error
	if resendInfo.lastEventID != common.EmptyEventID && resendInfo.lastEventVersion != common.EmptyVersion {
		err = t.nDCHistoryResender.SendSingleWorkflowHistory(
			transferTask.GetNamespaceId(),
			transferTask.GetWorkflowId(),
			transferTask.GetRunId(),
			resendInfo.lastEventID,
			resendInfo.lastEventVersion,
			0,
			0,
		)
	} else if resendInfo.nextEventID != nil {
		err = t.historyRereplicator.SendMultiWorkflowHistory(
			transferTask.GetNamespaceId(),
			transferTask.GetWorkflowId(),
			transferTask.GetRunId(),
			*resendInfo.nextEventID,
			transferTask.GetRunId(),
			common.EndEventID, // use common.EndEventID since we do not know where is the end
		)
	} else {
		err = &serviceerror.Internal{
			Message: "transferQueueStandbyProcessor encounter empty historyResendInfo",
		}
	}

	if err != nil {
		t.logger.Error("Error re-replicating history from remote.",
			tag.ShardID(t.shard.GetShardID()),
			tag.WorkflowNamespaceID(transferTask.GetNamespaceId()),
			tag.WorkflowID(transferTask.GetWorkflowId()),
			tag.WorkflowRunID(transferTask.GetRunId()),
			tag.SourceCluster(t.clusterName))
	}

	// return error so task processing logic will retry
	return ErrTaskRetry
}

func (t *transferQueueStandbyTaskExecutor) getCurrentTime() time.Time {
	return t.shard.GetCurrentTime(t.clusterName)
}
