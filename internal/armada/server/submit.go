package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/gogo/protobuf/types"
	"github.com/gogo/status"
	pool "github.com/jolestar/go-commons-pool"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"k8s.io/utils/strings/slices"

	"github.com/armadaproject/armada/internal/armada/configuration"
	"github.com/armadaproject/armada/internal/armada/permissions"
	"github.com/armadaproject/armada/internal/armada/repository"
	servervalidation "github.com/armadaproject/armada/internal/armada/validation"
	"github.com/armadaproject/armada/internal/common/armadacontext"
	"github.com/armadaproject/armada/internal/common/armadaerrors"
	"github.com/armadaproject/armada/internal/common/auth/authorization"
	"github.com/armadaproject/armada/internal/common/compress"
	"github.com/armadaproject/armada/internal/common/util"
	"github.com/armadaproject/armada/internal/common/validation"
	"github.com/armadaproject/armada/pkg/api"
	"github.com/armadaproject/armada/pkg/client/queue"
)

type SubmitServer struct {
	authorizer               ActionAuthorizer
	jobRepository            repository.JobRepository
	queueRepository          repository.QueueRepository
	eventStore               repository.EventStore
	schedulingInfoRepository repository.SchedulingInfoRepository
	cancelJobsBatchSize      int
	queueManagementConfig    *configuration.QueueManagementConfig
	schedulingConfig         *configuration.SchedulingConfig
	compressorPool           *pool.ObjectPool
}

type JobSubmitError struct {
	JobErrorsDetails []*api.JobSubmitResponseItem
	Err              error
}

func (e *JobSubmitError) Error() string {
	output := ""
	for _, jobError := range e.JobErrorsDetails {
		output += fmt.Sprintf("Error - Job %s: %s\n", jobError.JobId, jobError.Error)
	}

	output += fmt.Sprintf("\nError - %s", e.Err.Error())
	return output
}

func NewSubmitServer(
	authorizer ActionAuthorizer,
	jobRepository repository.JobRepository,
	queueRepository repository.QueueRepository,
	eventStore repository.EventStore,
	schedulingInfoRepository repository.SchedulingInfoRepository,
	cancelJobsBatchSize int,
	queueManagementConfig *configuration.QueueManagementConfig,
	schedulingConfig *configuration.SchedulingConfig,
) *SubmitServer {
	poolConfig := pool.ObjectPoolConfig{
		MaxTotal:                 100,
		MaxIdle:                  50,
		MinIdle:                  10,
		BlockWhenExhausted:       true,
		MinEvictableIdleTime:     30 * time.Minute,
		SoftMinEvictableIdleTime: math.MaxInt64,
		TimeBetweenEvictionRuns:  0,
		NumTestsPerEvictionRun:   10,
	}

	compressorPool := pool.NewObjectPool(armadacontext.Background(), pool.NewPooledObjectFactorySimple(
		func(context.Context) (interface{}, error) {
			return compress.NewZlibCompressor(512)
		}), &poolConfig)

	return &SubmitServer{
		authorizer:               authorizer,
		jobRepository:            jobRepository,
		queueRepository:          queueRepository,
		eventStore:               eventStore,
		schedulingInfoRepository: schedulingInfoRepository,
		cancelJobsBatchSize:      cancelJobsBatchSize,
		queueManagementConfig:    queueManagementConfig,
		schedulingConfig:         schedulingConfig,
		compressorPool:           compressorPool,
	}
}

func (server *SubmitServer) Health(ctx context.Context, _ *types.Empty) (*api.HealthCheckResponse, error) {
	// For now, lets make the health check really simple.
	return &api.HealthCheckResponse{Status: api.HealthCheckResponse_SERVING}, nil
}

func (server *SubmitServer) GetQueueInfo(grpcCtx context.Context, req *api.QueueInfoRequest) (*api.QueueInfo, error) {
	ctx := armadacontext.FromGrpcCtx(grpcCtx)
	q, err := server.queueRepository.GetQueue(req.Name)
	var expected *repository.ErrQueueNotFound
	if errors.Is(err, expected) {
		return nil, status.Errorf(codes.NotFound, "[GetQueueInfo] Queue %s does not exist", req.Name)
	}
	if err != nil {
		return nil, err
	}

	err = server.authorizer.AuthorizeQueueAction(ctx, q, permissions.WatchAllEvents, queue.PermissionVerbWatch)
	var permErr *armadaerrors.ErrUnauthorized
	if errors.As(err, &permErr) {
		return nil, status.Errorf(codes.PermissionDenied, "[GetQueueInfo] error getting info for queue %s: %s", req.Name, permErr)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[GetQueueInfo] error checking permissions: %s", err)
	}

	jobSets, e := server.jobRepository.GetQueueActiveJobSets(req.Name)
	if e != nil {
		return nil, status.Errorf(codes.Unavailable, "[GetQueueInfo] error getting job sets for queue %s: %s", req.Name, err)
	}

	return &api.QueueInfo{
		Name:          req.Name,
		ActiveJobSets: jobSets,
	}, nil
}

func (server *SubmitServer) GetQueue(grpcCtx context.Context, req *api.QueueGetRequest) (*api.Queue, error) {
	queue, err := server.queueRepository.GetQueue(req.Name)
	var e *repository.ErrQueueNotFound
	if errors.As(err, &e) {
		return nil, status.Errorf(codes.NotFound, "[GetQueue] error: %s", err)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[GetQueue] error getting queue %q: %s", req.Name, err)
	}
	return queue.ToAPI(), nil
}

func (server *SubmitServer) GetQueues(req *api.StreamingQueueGetRequest, stream api.Submit_GetQueuesServer) error {
	// Receive once to get information about the number of queues to return
	numToReturn := req.GetNum()
	if numToReturn < 1 {
		numToReturn = math.MaxUint32
	}

	queues, err := server.queueRepository.GetAllQueues()
	if err != nil {
		return err
	}
	for i, queue := range queues {
		if uint32(i) < numToReturn {
			err := stream.Send(&api.StreamingQueueMessage{
				Event: &api.StreamingQueueMessage_Queue{Queue: queue.ToAPI()},
			})
			if err != nil {
				return err
			}
		}
	}
	err = stream.Send(&api.StreamingQueueMessage{
		Event: &api.StreamingQueueMessage_End{
			End: &api.EndMarker{},
		},
	})
	if err != nil {
		return err
	}
	return nil
}

func (server *SubmitServer) CreateQueue(grpcCtx context.Context, request *api.Queue) (*types.Empty, error) {
	ctx := armadacontext.FromGrpcCtx(grpcCtx)
	err := server.authorizer.AuthorizeAction(ctx, permissions.CreateQueue)
	var ep *armadaerrors.ErrUnauthorized
	if errors.As(err, &ep) {
		return nil, status.Errorf(codes.PermissionDenied, "[CreateQueue] error creating queue %s: %s", request.Name, ep)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[CreateQueue] error checking permissions: %s", err)
	}

	if len(request.UserOwners) == 0 {
		principal := authorization.GetPrincipal(ctx)
		request.UserOwners = []string{principal.GetName()}
	}

	queue, err := queue.NewQueue(request)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "[CreateQueue] error validating queue: %s", err)
	}

	err = server.queueRepository.CreateQueue(queue)
	var eq *repository.ErrQueueAlreadyExists
	if errors.As(err, &eq) {
		return nil, status.Errorf(codes.AlreadyExists, "[CreateQueue] error creating queue: %s", err)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[CreateQueue] error creating queue: %s", err)
	}

	return &types.Empty{}, nil
}

func (server *SubmitServer) CreateQueues(grpcCtx context.Context, request *api.QueueList) (*api.BatchQueueCreateResponse, error) {
	ctx := armadacontext.FromGrpcCtx(grpcCtx)
	var failedQueues []*api.QueueCreateResponse
	// Create a queue for each element of the request body and return the failures.
	for _, queue := range request.Queues {
		_, err := server.CreateQueue(ctx, queue)
		if err != nil {
			failedQueues = append(failedQueues, &api.QueueCreateResponse{
				Queue: queue,
				Error: err.Error(),
			})
		}
	}

	return &api.BatchQueueCreateResponse{
		FailedQueues: failedQueues,
	}, nil
}

func (server *SubmitServer) UpdateQueue(grpcCtx context.Context, request *api.Queue) (*types.Empty, error) {
	ctx := armadacontext.FromGrpcCtx(grpcCtx)
	err := server.authorizer.AuthorizeAction(ctx, permissions.CreateQueue)
	var ep *armadaerrors.ErrUnauthorized
	if errors.As(err, &ep) {
		return nil, status.Errorf(codes.PermissionDenied, "[UpdateQueue] error updating queue %s: %s", request.Name, ep)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[UpdateQueue] error checking permissions: %s", err)
	}

	queue, err := queue.NewQueue(request)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "[UpdateQueue] error: %s", err)
	}

	err = server.queueRepository.UpdateQueue(queue)
	var e *repository.ErrQueueNotFound
	if errors.As(err, &e) {
		return nil, status.Errorf(codes.NotFound, "[UpdateQueue] error: %s", err)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[UpdateQueue] error getting queue %q: %s", queue.Name, err)
	}

	return &types.Empty{}, nil
}

func (server *SubmitServer) UpdateQueues(grpcCtx context.Context, request *api.QueueList) (*api.BatchQueueUpdateResponse, error) {
	ctx := armadacontext.FromGrpcCtx(grpcCtx)
	var failedQueues []*api.QueueUpdateResponse

	// Create a queue for each element of the request body and return the failures.
	for _, queue := range request.Queues {
		_, err := server.UpdateQueue(ctx, queue)
		if err != nil {
			failedQueues = append(failedQueues, &api.QueueUpdateResponse{
				Queue: queue,
				Error: err.Error(),
			})
		}
	}

	return &api.BatchQueueUpdateResponse{
		FailedQueues: failedQueues,
	}, nil
}

func (server *SubmitServer) DeleteQueue(grpcCtx context.Context, request *api.QueueDeleteRequest) (*types.Empty, error) {
	ctx := armadacontext.FromGrpcCtx(grpcCtx)
	err := server.authorizer.AuthorizeAction(ctx, permissions.DeleteQueue)
	var ep *armadaerrors.ErrUnauthorized
	if errors.As(err, &ep) {
		return nil, status.Errorf(codes.PermissionDenied, "[DeleteQueue] error deleting queue %s: %s", request.Name, ep)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[DeleteQueue] error checking permissions: %s", err)
	}

	active, err := server.jobRepository.GetQueueActiveJobSets(request.Name)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "[DeleteQueue] error getting active job sets for queue %s: %s", request.Name, err)
	}
	if len(active) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "[DeleteQueue] error deleting queue %s: queue is not empty", request.Name)
	}

	err = server.queueRepository.DeleteQueue(request.Name)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "[DeleteQueue] error deleting queue %s: %s", request.Name, err)
	}

	return &types.Empty{}, nil
}

func (server *SubmitServer) SubmitJobs(grpcCtx context.Context, req *api.JobSubmitRequest) (*api.JobSubmitResponse, error) {
	ctx := armadacontext.FromGrpcCtx(grpcCtx)
	principal := authorization.GetPrincipal(ctx)

	const maxResponseItems = 5
	var lastIdx int

	jobs, responseItems, e := server.createJobs(req, principal.GetName(), principal.GetGroupNames())
	if e != nil {
		if len(responseItems) > maxResponseItems {
			lastIdx = maxResponseItems
		} else {
			lastIdx = len(responseItems)
		}

		reqJson, _ := json.Marshal(req)
		createJobsErrFmt := "[SubmitJobs] error creating %d of %d job(s) submitted; %s for user %s; first %d errors:%v"
		numFails := len(responseItems)
		numSubmitted := numFails + len(jobs)
		details := &api.JobSubmitResponse{JobResponseItems: responseItems[:lastIdx]}

		st, err := status.Newf(codes.InvalidArgument, createJobsErrFmt, numFails, numSubmitted, reqJson,
			principal.GetName(), maxResponseItems, e).WithDetails(details)
		if err != nil {
			subJobUserFmt := "[SubmitJobs] error submitting job %s for user %s; : %v"
			return nil, status.Errorf(codes.InvalidArgument, subJobUserFmt, reqJson, principal.GetName(), e)
		}
		return nil, st.Err()
	}

	if responseItems, err := validation.ValidateApiJobs(jobs, *server.schedulingConfig); err != nil {
		reqJson, _ := json.Marshal(req)
		numFails := len(responseItems)
		numSubmitted := len(jobs)
		if len(responseItems) > maxResponseItems {
			lastIdx = maxResponseItems
		} else {
			lastIdx = len(responseItems)
		}

		details := &api.JobSubmitResponse{JobResponseItems: responseItems[:lastIdx]}
		validJobsErrFmt := "[SubmitJobs] error validating %d of %d job(s) submitted; %s for user %s; first %d errors:%v"
		st, err := status.Newf(codes.InvalidArgument, validJobsErrFmt, numFails, numSubmitted, reqJson,
			principal.GetName(), e).WithDetails(details)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, validJobsErrFmt, numFails, numSubmitted, reqJson,
				principal.GetName(), e)
		}
		return nil, st.Err()
	}

	q, err := server.getQueueOrCreate(ctx, req.Queue)
	if err != nil {
		return nil, status.Errorf(armadaerrors.CodeFromError(err), "couldn't get/make queue: %s", err)
	}

	err = server.submittingJobsWouldSurpassLimit(*q, req)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "[SubmitJobs] error checking queue limit: %s", err)
	}

	err = server.authorizer.AuthorizeQueueAction(ctx, *q, permissions.SubmitAnyJobs, queue.PermissionVerbSubmit)
	var permError *armadaerrors.ErrUnauthorized
	if errors.As(err, &permError) {
		return nil, status.Errorf(codes.PermissionDenied, "[SubmitJobs] error submitting job in queue %s: %s", req.Queue, permError)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[SubmitJobs] error checking permissions: %s", err)
	}

	// Check if the job would fit on any executor,
	// to avoid having users wait for a job that may never be scheduled
	allClusterSchedulingInfo, err := server.schedulingInfoRepository.GetClusterSchedulingInfo()
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "error getting scheduling info: %s", err)
	}

	if ok, responseItems, err := validateJobsCanBeScheduled(jobs, allClusterSchedulingInfo); !ok {
		if err != nil {
			numFails := len(responseItems)
			numSubmitted := len(jobs)
			if len(responseItems) > maxResponseItems {
				lastIdx = maxResponseItems
			} else {
				lastIdx = len(responseItems)
			}
			details := &api.JobSubmitResponse{JobResponseItems: responseItems[:lastIdx]}
			validJobsErrFmt := "[SubmitJobs] error validating %d of %d job(s) submitted for user %s; first %d errors:%v"

			st, e := status.Newf(codes.InvalidArgument, validJobsErrFmt, numFails, numSubmitted,
				principal.GetName(), maxResponseItems, err).WithDetails(details)
			if e != nil {
				return nil, status.Errorf(codes.InvalidArgument, "[SubmitJobs] error validating jobs: %s", err)
			}
			return nil, st.Err()
		}
		return nil, errors.Errorf("can't schedule job for user %s", principal.GetName())
	}

	// Create events marking the jobs as submitted
	err = reportSubmitted(server.eventStore, jobs)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "[SubmitJobs] error getting submitted report: %s", err)
	}

	// Submit the jobs by writing them to the database
	submissionResults, err := server.jobRepository.AddJobs(jobs)
	if err != nil {
		jobFailures := createJobFailuresWithReason(jobs, fmt.Sprintf("Failed to save job in Armada: %s", e))
		reportErr := reportFailed(server.eventStore, "", jobFailures)
		if reportErr != nil {
			return nil, status.Errorf(codes.Internal, "[SubmitJobs] error reporting failure event: %v", reportErr)
		}
		return nil, status.Errorf(codes.Aborted, "[SubmitJobs] error saving jobs in Armada: %s", err)
	}

	// Create the response to send to the client
	result := &api.JobSubmitResponse{
		JobResponseItems: make([]*api.JobSubmitResponseItem, 0, len(submissionResults)),
	}

	var createdJobs []*api.Job
	var jobFailures []*jobFailure
	var doubleSubmits []*repository.SubmitJobResult

	for i, submissionResult := range submissionResults {
		jobResponse := &api.JobSubmitResponseItem{JobId: submissionResult.JobId}

		if submissionResult.Error != nil {
			jobResponse.Error = submissionResult.Error.Error()
			jobFailures = append(jobFailures, &jobFailure{
				job:    jobs[i],
				reason: fmt.Sprintf("Failed to save job in Armada: %s", submissionResult.Error.Error()),
			})
		} else if submissionResult.DuplicateDetected {
			doubleSubmits = append(doubleSubmits, submissionResult)
		} else {
			createdJobs = append(createdJobs, jobs[i])
		}

		result.JobResponseItems = append(result.JobResponseItems, jobResponse)
	}

	err = reportFailed(server.eventStore, "", jobFailures)
	if err != nil {
		return result, status.Errorf(codes.Internal, fmt.Sprintf("[SubmitJobs] error reporting failed jobs: %s", err))
	}

	err = reportDuplicateDetected(server.eventStore, doubleSubmits)
	if err != nil {
		return result, status.Errorf(codes.Internal, fmt.Sprintf("[SubmitJobs] error reporting duplicate jobs: %s", err))
	}

	err = reportQueued(server.eventStore, createdJobs)
	if err != nil {
		return result, status.Errorf(codes.Internal, fmt.Sprintf("[SubmitJobs] error reporting queued jobs: %s", err))
	}

	if len(jobFailures) > 0 {
		return result, status.Errorf(codes.Internal, fmt.Sprintf("[SubmitJobs] error submitting some or all jobs: %s", err))
	}

	return result, nil
}

func (server *SubmitServer) submittingJobsWouldSurpassLimit(q queue.Queue, jobSubmitRequest *api.JobSubmitRequest) error {
	limit := server.queueManagementConfig.DefaultQueuedJobsLimit
	if limit <= 0 {
		return nil
	}

	queued, err := server.countQueuedJobs(q)
	if err != nil {
		return err
	}

	queuedAfterSubmission := queued + int64(len(jobSubmitRequest.JobRequestItems))
	if queuedAfterSubmission > int64(limit) {
		return errors.Errorf(
			"too many queued jobs: currently have %d, would have %d with new submission, limit is %d",
			queued, queuedAfterSubmission, limit)
	}

	return nil
}

func (server *SubmitServer) countQueuedJobs(q queue.Queue) (int64, error) {
	sizes, err := server.jobRepository.GetQueueSizes(queue.QueuesToAPI([]queue.Queue{q}))
	if err != nil {
		return 0, err
	}
	if len(sizes) == 0 {
		return 0, errors.Errorf("no value for number of queued jobs returned from job repository")
	}
	return sizes[0], nil
}

// CancelJobs cancels jobs identified by the request.
// If the request contains a job ID, only the job with that ID is cancelled.
// If the request contains a queue name and a job set ID, all jobs matching those are cancelled.
func (server *SubmitServer) CancelJobs(grpcCtx context.Context, request *api.JobCancelRequest) (*api.CancellationResult, error) {
	ctx := armadacontext.FromGrpcCtx(grpcCtx)
	if request.JobId != "" {
		return server.cancelJobsById(ctx, request.JobId, request.Reason)
	} else if request.JobSetId != "" && request.Queue != "" {
		return server.cancelJobsByQueueAndSet(ctx, request.Queue, request.JobSetId, nil, request.Reason)
	}
	return nil, status.Errorf(codes.InvalidArgument, "[CancelJobs] specify either job ID or both queue name and job set ID")
}

func (server *SubmitServer) CancelJobSet(grpcCtx context.Context, request *api.JobSetCancelRequest) (*types.Empty, error) {
	ctx := armadacontext.FromGrpcCtx(grpcCtx)
	err := servervalidation.ValidateJobSetFilter(request.Filter)
	if err != nil {
		return nil, err
	}
	_, err = server.cancelJobsByQueueAndSet(ctx, request.Queue, request.JobSetId, createJobSetFilter(request.Filter), request.Reason)
	return &types.Empty{}, err
}

func createJobSetFilter(filter *api.JobSetFilter) *repository.JobSetFilter {
	if filter == nil {
		return nil
	}
	jobSetFilter := &repository.JobSetFilter{
		IncludeQueued: false,
		IncludeLeased: false,
	}

	for _, state := range filter.States {
		if state == api.JobState_QUEUED {
			jobSetFilter.IncludeQueued = true
		}
		if state == api.JobState_PENDING || state == api.JobState_RUNNING {
			jobSetFilter.IncludeLeased = true
		}
	}

	return jobSetFilter
}

// cancels a job with a given ID
func (server *SubmitServer) cancelJobsById(ctx *armadacontext.Context, jobId string, reason string) (*api.CancellationResult, error) {
	jobs, err := server.jobRepository.GetExistingJobsByIds([]string{jobId})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[cancelJobsById] error getting job with ID %s: %s", jobId, err)
	}
	if len(jobs) != 1 {
		return nil, status.Errorf(codes.Internal, "[cancelJobsById] error getting job with ID %s: expected exactly one result, but got %v", jobId, jobs)
	}

	result, err := server.cancelJobs(ctx, jobs, reason)
	var e *armadaerrors.ErrUnauthorized
	if errors.As(err, &e) {
		return nil, status.Errorf(codes.PermissionDenied, "[cancelJobsById] error canceling job with ID %s: %s", jobId, e)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[cancelJobsById] error checking permissions: %s", err)
	}

	return result, nil
}

// cancels all jobs part of a particular job set and queue
func (server *SubmitServer) cancelJobsByQueueAndSet(
	ctx *armadacontext.Context,
	queue string,
	jobSetId string,
	filter *repository.JobSetFilter,
	reason string,
) (*api.CancellationResult, error) {
	ids, err := server.jobRepository.GetJobSetJobIds(queue, jobSetId, filter)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[cancelJobsBySetAndQueue] error getting job IDs: %s", err)
	}

	// Split IDs into batches and process one batch at a time
	// To reduce the number of jobs stored in memory
	batches := util.Batch(ids, server.cancelJobsBatchSize)
	var cancelledIds []string
	for _, batch := range batches {
		jobs, err := server.jobRepository.GetExistingJobsByIds(batch)
		if err != nil {
			result := &api.CancellationResult{CancelledIds: cancelledIds}
			return result, status.Errorf(codes.Internal, "[cancelJobsBySetAndQueue] error getting jobs: %s", err)
		}

		result, err := server.cancelJobs(ctx, jobs, reason)
		var e *armadaerrors.ErrUnauthorized
		if errors.As(err, &e) {
			return nil, status.Errorf(codes.PermissionDenied, "[cancelJobsBySetAndQueue] error canceling jobs: %s", e)
		} else if err != nil {
			result := &api.CancellationResult{CancelledIds: cancelledIds}
			return result, status.Errorf(codes.Unavailable, "[cancelJobsBySetAndQueue] error checking permissions: %s", err)
		}
		cancelledIds = append(cancelledIds, result.CancelledIds...)

		// TODO I think the right way to do this is to include a timeout with the call to Redis
		// Then, we can check for a deadline exceeded error here
		if util.CloseToDeadline(ctx, time.Second*1) {
			result := &api.CancellationResult{CancelledIds: cancelledIds}
			return result, status.Errorf(codes.DeadlineExceeded, "[cancelJobsBySetAndQueue] deadline exceeded")
		}
	}

	return &api.CancellationResult{CancelledIds: cancelledIds}, nil
}

func (server *SubmitServer) cancelJobs(ctx *armadacontext.Context, jobs []*api.Job, reason string) (*api.CancellationResult, error) {
	principal := authorization.GetPrincipal(ctx)

	err := server.checkCancelPerms(ctx, jobs)
	if err != nil {
		return nil, err
	}

	err = reportJobsCancelling(server.eventStore, principal.GetName(), jobs, reason)
	if err != nil {
		return nil, errors.Errorf("[cancelJobs] error reporting jobs marked as cancelled: %v", err)
	}

	deletionResult, err := server.jobRepository.DeleteJobs(jobs)
	if err != nil {
		return nil, errors.Errorf("[cancelJobs] error deleting jobs: %v", err)
	}
	var cancelled []*api.Job
	var cancelledIds []string
	for job, err := range deletionResult {
		if err != nil {
			log.Errorf("[cancelJobs] error cancelling job with ID %s: %s", job.Id, err)
		} else {
			cancelled = append(cancelled, job)
			cancelledIds = append(cancelledIds, job.Id)
		}
	}

	cancelledJobPayloads := util.Map(cancelled, func(job *api.Job) *CancelledJobPayload {
		return &CancelledJobPayload{
			job:    job,
			reason: reason,
		}
	})
	err = reportJobsCancelled(server.eventStore, principal.GetName(), cancelledJobPayloads)
	if err != nil {
		return nil, errors.Errorf("[cancelJobs] error reporting job cancellation: %v", err)
	}

	return &api.CancellationResult{CancelledIds: cancelledIds}, nil
}

func (server *SubmitServer) checkCancelPerms(ctx *armadacontext.Context, jobs []*api.Job) error {
	queueNames := make(map[string]struct{})
	for _, job := range jobs {
		queueNames[job.Queue] = struct{}{}
	}
	for queueName := range queueNames {
		q, err := server.queueRepository.GetQueue(queueName)
		if err != nil {
			return err
		}

		err = server.authorizer.AuthorizeQueueAction(ctx, q, permissions.CancelAnyJobs, queue.PermissionVerbCancel)
		var permErr *armadaerrors.ErrUnauthorized
		if errors.As(err, &permErr) {
			return permErr
		} else if err != nil {
			return err
		}
	}
	return nil
}

// ReprioritizeJobs updates the priority of one of more jobs.
// Returns a map from job ID to any error (or nil if the call succeeded).
func (server *SubmitServer) ReprioritizeJobs(grpcCtx context.Context, request *api.JobReprioritizeRequest) (*api.JobReprioritizeResponse, error) {
	ctx := armadacontext.FromGrpcCtx(grpcCtx)
	var jobs []*api.Job
	if len(request.JobIds) > 0 {
		existingJobs, err := server.jobRepository.GetExistingJobsByIds(request.JobIds)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "[ReprioritizeJobs] error getting jobs by ID: %s", err)
		}
		jobs = existingJobs
	} else if request.Queue != "" && request.JobSetId != "" {
		ids, err := server.jobRepository.GetActiveJobIds(request.Queue, request.JobSetId)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable,
				"[ReprioritizeJobs] error getting job IDs for queue %s and job set %s: %s",
				request.Queue, request.JobSetId, err)
		}

		existingJobs, err := server.jobRepository.GetExistingJobsByIds(ids)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "[ReprioritizeJobs] error getting jobs for queue %s and job set %s: %s", request.Queue, request.JobSetId, err)
		}
		jobs = existingJobs
	}

	err := server.checkReprioritizePerms(ctx, jobs)
	var e *armadaerrors.ErrUnauthorized
	if errors.As(err, &e) {
		return nil, status.Errorf(codes.PermissionDenied, "[ReprioritizeJobs] error: %s", e)
	} else if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[ReprioritizeJobs] error checking permissions: %s", err)
	}

	principalName := authorization.GetPrincipal(ctx).GetName()
	err = reportJobsReprioritizing(server.eventStore, principalName, jobs, request.NewPriority)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[ReprioritizeJobs] error reporting job re-prioritisation: %s", err)
	}

	var jobIds []string
	for _, job := range jobs {
		jobIds = append(jobIds, job.Id)
	}
	results, err := server.reprioritizeJobs(jobIds, request.NewPriority, principalName)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "[ReprioritizeJobs] error re-prioritising jobs: %s", err)
	}

	return &api.JobReprioritizeResponse{ReprioritizationResults: results}, nil
}

func (server *SubmitServer) reprioritizeJobs(jobIds []string, newPriority float64, principalName string) (map[string]string, error) {
	// TODO There's a bug here.
	// The function passed to UpdateJobs is called under an optimistic lock.
	// If the jobs to be updated are mutated by another thread concurrently,
	// the changes are not written to Redis. However, this function has side effects
	// (creating reprioritized events) that would not be rolled back.
	updateJobResults, err := server.jobRepository.UpdateJobs(jobIds, func(jobs []*api.Job) {
		for _, job := range jobs {
			job.Priority = newPriority
		}
		err := server.reportReprioritizedJobEvents(jobs, newPriority, principalName)
		if err != nil {
			log.Warnf("Failed to report events for reprioritize of jobs %s: %v", strings.Join(jobIds, ", "), err)
		}
	})
	if err != nil {
		return nil, errors.Errorf("[reprioritizeJobs] error updating jobs: %s", err)
	}

	results := map[string]string{}
	for _, r := range updateJobResults {
		if r.Error == nil {
			results[r.JobId] = ""
		} else {
			results[r.JobId] = r.Error.Error()
		}
	}
	return results, nil
}

func (server *SubmitServer) reportReprioritizedJobEvents(reprioritizedJobs []*api.Job, newPriority float64, principalName string) error {
	err := reportJobsUpdated(server.eventStore, principalName, reprioritizedJobs)
	if err != nil {
		return errors.Errorf("[reportReprioritizedJobEvents] error reporting jobs updated: %v", err)
	}

	err = reportJobsReprioritized(server.eventStore, principalName, reprioritizedJobs, newPriority)
	if err != nil {
		return errors.Errorf("[reportReprioritizedJobEvents] error reporting jobs reprioritized: %v", err)
	}

	return nil
}

func (server *SubmitServer) checkReprioritizePerms(ctx *armadacontext.Context, jobs []*api.Job) error {
	queueNames := make(map[string]struct{})
	for _, job := range jobs {
		queueNames[job.Queue] = struct{}{}
	}
	for queueName := range queueNames {
		q, err := server.queueRepository.GetQueue(queueName)
		if err != nil {
			return err
		}

		err = server.authorizer.AuthorizeQueueAction(ctx, q, permissions.ReprioritizeAnyJobs, queue.PermissionVerbReprioritize)
		var permErr *armadaerrors.ErrUnauthorized
		if errors.As(err, &permErr) {
			return permErr
		} else if err != nil {
			return err
		}
	}
	return nil
}

func (server *SubmitServer) getQueueOrCreate(ctx *armadacontext.Context, queueName string) (*queue.Queue, error) {
	q, e := server.queueRepository.GetQueue(queueName)
	if e == nil {
		return &q, nil
	}
	var expected *repository.ErrQueueNotFound

	if errors.As(e, &expected) {

		if !server.queueManagementConfig.AutoCreateQueues {
			return nil, status.Errorf(
				codes.Aborted,
				"Queue %s not found; refusing to make it automatically (server setting autoCreateQueues is false)",
				queueName,
			)
		}
		if server.authorizer.AuthorizeAction(ctx, permissions.SubmitAnyJobs) != nil {
			return nil, status.Errorf(codes.PermissionDenied, "Queue %s not found; won't create because user lacks SubmitAnyJobs permission", queueName)
		}

		principal := authorization.GetPrincipal(ctx)
		groupNames := slices.Filter(nil, principal.GetGroupNames(),
			func(s string) bool { return s != authorization.EveryoneGroup },
		)
		q = queue.Queue{
			Name:           queueName,
			PriorityFactor: queue.PriorityFactor(server.queueManagementConfig.DefaultPriorityFactor),
			Permissions: []queue.Permissions{
				queue.NewPermissionsFromOwners([]string{principal.GetName()}, groupNames),
			},
		}

		if err := server.queueRepository.CreateQueue(q); err != nil {
			return nil, status.Errorf(codes.Aborted, "Couldn't find or create queue %s: %s", queueName, err.Error())
		}
		return &q, nil
	}

	return nil, status.Errorf(codes.Unavailable, "Couldn't load queue %s: %s", queueName, e.Error())
}

// createJobs returns a list of objects representing the jobs in a JobSubmitRequest.
// This function validates the jobs in the request and the pod specs. in each job.
// If any job or pod in invalid, an error is returned.
func (server *SubmitServer) createJobs(request *api.JobSubmitRequest, owner string, ownershipGroups []string) ([]*api.Job, []*api.JobSubmitResponseItem, error) {
	return server.createJobsObjects(request, owner, ownershipGroups, time.Now, util.NewULID)
}

func (server *SubmitServer) createJobsObjects(request *api.JobSubmitRequest, owner string, ownershipGroups []string,
	getTime func() time.Time, getUlid func() string,
) ([]*api.Job, []*api.JobSubmitResponseItem, error) {
	compressor, err := server.compressorPool.BorrowObject(armadacontext.Background())
	if err != nil {
		return nil, nil, err
	}
	defer func(compressorPool *pool.ObjectPool, ctx *armadacontext.Context, object interface{}) {
		err := compressorPool.ReturnObject(ctx, object)
		if err != nil {
			log.WithError(err).Errorf("Error returning compressor to pool")
		}
	}(server.compressorPool, armadacontext.Background(), compressor)
	compressedOwnershipGroups, err := compress.CompressStringArray(ownershipGroups, compressor.(compress.Compressor))
	if err != nil {
		return nil, nil, err
	}

	jobs := make([]*api.Job, 0, len(request.JobRequestItems))

	if request.JobSetId == "" {
		return nil, nil, errors.Errorf("[createJobs] job set not specified")
	}

	if request.Queue == "" {
		return nil, nil, errors.Errorf("[createJobs] queue not specified")
	}

	responseItems := make([]*api.JobSubmitResponseItem, 0, len(request.JobRequestItems))
	for i, item := range request.JobRequestItems {
		jobId := getUlid()

		if item.PodSpec != nil && len(item.PodSpecs) > 0 {
			response := &api.JobSubmitResponseItem{
				JobId: jobId,
				Error: fmt.Sprintf("[createJobs] job %d in job set %s contains both podSpec and podSpecs, but may only contain either", i, request.JobSetId),
			}
			responseItems = append(responseItems, response)
		}
		podSpec := item.GetMainPodSpec()
		if podSpec == nil {
			response := &api.JobSubmitResponseItem{
				JobId: jobId,
				Error: fmt.Sprintf("[createJobs] job %d in job set %s contains no podSpec", i, request.JobSetId),
			}
			responseItems = append(responseItems, response)
			continue // Safety check, to avoid possible nil pointer dereference below
		}
		if err := validation.ValidateJobSubmitRequestItem(item); err != nil {
			response := &api.JobSubmitResponseItem{
				JobId: jobId,
				Error: fmt.Sprintf("[createJobs] error validating the %d-th job of job set %s: %v", i, request.JobSetId, err),
			}
			responseItems = append(responseItems, response)
		}
		namespace := item.Namespace
		if namespace == "" {
			namespace = "default"
		}
		fillContainerRequestsAndLimits(podSpec.Containers)
		applyDefaultsToAnnotations(item.Annotations, *server.schedulingConfig)
		applyDefaultsToPodSpec(podSpec, *server.schedulingConfig)
		if err := validation.ValidatePodSpec(podSpec, server.schedulingConfig); err != nil {
			response := &api.JobSubmitResponseItem{
				JobId: jobId,
				Error: fmt.Sprintf("[createJobs] error validating the %d-th job of job set %s: %v", i, request.JobSetId, err),
			}
			responseItems = append(responseItems, response)
		}

		// TODO: remove, RequiredNodeLabels is deprecated and will be removed in future versions
		for k, v := range item.RequiredNodeLabels {
			if podSpec.NodeSelector == nil {
				podSpec.NodeSelector = map[string]string{}
			}
			podSpec.NodeSelector[k] = v
		}

		enrichText(item.Labels, jobId)
		enrichText(item.Annotations, jobId)
		j := &api.Job{
			Id:       jobId,
			ClientId: item.ClientId,
			Queue:    request.Queue,
			JobSetId: request.JobSetId,

			Namespace:   namespace,
			Labels:      item.Labels,
			Annotations: item.Annotations,

			RequiredNodeLabels: item.RequiredNodeLabels,
			Ingress:            item.Ingress,
			Services:           item.Services,

			Priority: item.Priority,

			Scheduler:                          item.Scheduler,
			PodSpec:                            item.PodSpec,
			PodSpecs:                           item.PodSpecs,
			Created:                            getTime(), // Replaced with now for mocking unit test
			Owner:                              owner,
			QueueOwnershipUserGroups:           nil,
			CompressedQueueOwnershipUserGroups: compressedOwnershipGroups,
			QueueTtlSeconds:                    item.QueueTtlSeconds,
		}
		jobs = append(jobs, j)
	}

	if len(responseItems) > 0 {
		return nil, responseItems, errors.New("[createJobs] error creating jobs, check JobSubmitResponse for details")
	}
	return jobs, nil, nil
}

func enrichText(labels map[string]string, jobId string) {
	for key, value := range labels {
		value := strings.ReplaceAll(value, "{{JobId}}", ` \z`) // \z cannot be entered manually, hence its use
		value = strings.ReplaceAll(value, "{JobId}", jobId)
		labels[key] = strings.ReplaceAll(value, ` \z`, "JobId")
	}
}

func createJobFailuresWithReason(jobs []*api.Job, reason string) []*jobFailure {
	jobFailures := make([]*jobFailure, len(jobs), len(jobs))
	for i, job := range jobs {
		jobFailures[i] = &jobFailure{
			job:    job,
			reason: reason,
		}
	}
	return jobFailures
}
