package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"regexp"

	tasks "google.golang.org/genproto/googleapis/cloud/tasks/v2beta3"
	v1 "google.golang.org/genproto/googleapis/iam/v1"

	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc"
)

// NewServer creates a new emulator server with its own task and queue bookkeeping
func NewServer() *Server {
	return &Server{
		qs: make(map[string]*Queue),
		ts: make(map[string]*Task),
	}
}

// Server represents the emulator server
type Server struct {
	qs map[string]*Queue
	ts map[string]*Task
}

// ListQueues lists the existing queues
func (s *Server) ListQueues(ctx context.Context, in *tasks.ListQueuesRequest) (*tasks.ListQueuesResponse, error) {
	// TODO: Implement pageing

	var queueStates []*tasks.Queue

	for _, queue := range s.qs {
		if queue != nil {
			queueStates = append(queueStates, queue.state)
		}
	}

	return &tasks.ListQueuesResponse{
		Queues: queueStates,
	}, nil
}

// GetQueue returns the requested queue
func (s *Server) GetQueue(ctx context.Context, in *tasks.GetQueueRequest) (*tasks.Queue, error) {
	queue := s.qs[in.GetName()]

	// TODO: handle not found

	return queue.state, nil
}

// CreateQueue creates a new queue
func (s *Server) CreateQueue(ctx context.Context, in *tasks.CreateQueueRequest) (*tasks.Queue, error) {
	queueState := in.GetQueue()

	name := queueState.GetName()
	nameMatched, _ := regexp.MatchString("projects/[A-Za-z0-9-]+/locations/[A-Za-z0-9-]+/queues/[A-Za-z0-9-]+", name)
	if !nameMatched {
		return nil, status.Errorf(codes.InvalidArgument, "Queue name must be formatted: \"projects/<PROJECT_ID>/locations/<LOCATION_ID>/queues/<QUEUE_ID>\"")
	}
	parent := in.GetParent()
	parentMatched, _ := regexp.MatchString("projects/[A-Za-z0-9-]+/locations/[A-Za-z0-9-]+", parent)
	if !parentMatched {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid resource field value in the request.")
	}
	queue, ok := s.qs[name]
	if ok {
		if queue != nil {
			return nil, status.Errorf(codes.AlreadyExists, "Queue already exists")
		}

		return nil, status.Errorf(codes.FailedPrecondition, "The queue cannot be created because a queue with this name existed too recently.")
	}

	// Make a deep copy so that the original is frozen for the http response
	queue, queueState = NewQueue(
		name,
		proto.Clone(queueState).(*tasks.Queue),
		func(task *Task) {
			// TODO: sync
			s.ts[task.state.GetName()] = nil
		},
	)
	s.qs[name] = queue
	queue.Run()

	return queueState, nil
}

// UpdateQueue updates an existing queue (not implemented yet)
func (s *Server) UpdateQueue(ctx context.Context, in *tasks.UpdateQueueRequest) (*tasks.Queue, error) {
	return nil, status.Errorf(codes.Unimplemented, "Not yet implemented")
}

// DeleteQueue removes an existing queue.
func (s *Server) DeleteQueue(ctx context.Context, in *tasks.DeleteQueueRequest) (*empty.Empty, error) {
	queue, ok := s.qs[in.GetName()]

	// Cloud responds with same error for recently deleted queue
	if !ok || queue == nil {
		return nil, status.Errorf(codes.NotFound, "Requested entity was not found.")
	}

	queue.Delete()

	// TODO: Sync
	s.qs[in.GetName()] = nil

	return &empty.Empty{}, nil
}

// PurgeQueue purges the specified queue
func (s *Server) PurgeQueue(ctx context.Context, in *tasks.PurgeQueueRequest) (*tasks.Queue, error) {
	queue, _ := s.qs[in.GetName()]

	queue.Purge()

	return queue.state, nil
}

// PauseQueue pauses queue execution
func (s *Server) PauseQueue(ctx context.Context, in *tasks.PauseQueueRequest) (*tasks.Queue, error) {
	queue, _ := s.qs[in.GetName()]

	queue.Pause()

	return queue.state, nil
}

// ResumeQueue resumes a paused queue
func (s *Server) ResumeQueue(ctx context.Context, in *tasks.ResumeQueueRequest) (*tasks.Queue, error) {
	queue, _ := s.qs[in.GetName()]

	queue.Resume()

	return queue.state, nil
}

// GetIamPolicy doesn't do anything
func (s *Server) GetIamPolicy(ctx context.Context, in *v1.GetIamPolicyRequest) (*v1.Policy, error) {
	return nil, status.Errorf(codes.Unimplemented, "Not yet implemented")
}

// SetIamPolicy doesn't do anything
func (s *Server) SetIamPolicy(ctx context.Context, in *v1.SetIamPolicyRequest) (*v1.Policy, error) {
	return nil, status.Errorf(codes.Unimplemented, "Not yet implemented")
}

// TestIamPermissions doesn't do anything
func (s *Server) TestIamPermissions(ctx context.Context, in *v1.TestIamPermissionsRequest) (*v1.TestIamPermissionsResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "Not yet implemented")
}

// ListTasks lists the tasks in the specified queue
func (s *Server) ListTasks(ctx context.Context, in *tasks.ListTasksRequest) (*tasks.ListTasksResponse, error) {
	// TODO: Implement pageing of some sort
	queue, _ := s.qs[in.GetParent()]

	var taskStates []*tasks.Task

	for _, task := range queue.ts {
		if task != nil {
			taskStates = append(taskStates, task.state)
		}
	}

	return &tasks.ListTasksResponse{
		Tasks: taskStates,
	}, nil
}

// GetTask returns the specified task
func (s *Server) GetTask(ctx context.Context, in *tasks.GetTaskRequest) (*tasks.Task, error) {
	task, ok := s.ts[in.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Task does not exist.")
	}
	if task == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "The task no longer exists,  though a task with this name existed recently. The task either successfully completed or was deleted.")
	}

	return task.state, nil
}

// CreateTask creates a new task
func (s *Server) CreateTask(ctx context.Context, in *tasks.CreateTaskRequest) (*tasks.Task, error) {
	// TODO: task name validation

	queueName := in.GetParent()
	queue, ok := s.qs[queueName]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Queue does not exist.")
	}
	if queue == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "The queue no longer exists, though a queue with this name existed recently.")
	}

	task, taskState := queue.NewTask(in.GetTask())
	s.ts[taskState.GetName()] = task

	return taskState, nil
}

// DeleteTask removes an existing task
func (s *Server) DeleteTask(ctx context.Context, in *tasks.DeleteTaskRequest) (*empty.Empty, error) {
	task, ok := s.ts[in.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Task does not exist.")
	}
	if task == nil {
		return nil, status.Errorf(codes.NotFound, "The task no longer exists, though a task with this name existed recently. The task either successfully completed or was deleted.")
	}

	task.Delete()

	return &empty.Empty{}, nil
}

// RunTask executes an existing task immediately
func (s *Server) RunTask(ctx context.Context, in *tasks.RunTaskRequest) (*tasks.Task, error) {
	task, ok := s.ts[in.GetName()]

	if !ok {
		return nil, status.Errorf(codes.NotFound, "Task does not exist.")
	}
	if task == nil {
		return nil, status.Errorf(codes.NotFound, "The task no longer exists, though a task with this name existed recently. The task either successfully completed or was deleted.")
	}

	taskState := task.Run()

	return taskState, nil
}

func main() {
	host := flag.String("host", "localhost", "The host name")
	port := flag.String("port", "8123", "The port")

	flag.Parse()

	lis, err := net.Listen("tcp", fmt.Sprintf("%v:%v", *host, *port))
	if err != nil {
		panic(err)
	}

	print(fmt.Sprintf("Starting cloud tasks emulator, listening on %v:%v", *host, *port))

	grpcServer := grpc.NewServer()
	tasks.RegisterCloudTasksServer(grpcServer, NewServer())
	grpcServer.Serve(lis)
}
