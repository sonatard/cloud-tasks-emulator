package main

import (
	"bytes"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"

	"github.com/golang/protobuf/proto"
	ptypes "github.com/golang/protobuf/ptypes"
	pduration "github.com/golang/protobuf/ptypes/duration"
	ptimestamp "github.com/golang/protobuf/ptypes/timestamp"
	tasks "google.golang.org/genproto/googleapis/cloud/tasks/v2beta3"
)

// Task holds all internals for a task
type Task struct {
	queue *Queue

	state *tasks.Task

	cancel chan bool

	onDone func(*Task)

	stateMutex sync.Mutex

	cancelOnce sync.Once
}

// NewTask creates a new task for the specified queue
func NewTask(queue *Queue, taskState *tasks.Task, onDone func(task *Task)) *Task {
	setInitialTaskState(taskState, queue.name)

	task := &Task{
		queue:  queue,
		state:  taskState,
		onDone: onDone,
		cancel: make(chan bool, 1), // Buffered in case cancel comes when task is not scheduled
	}

	return task
}

func setInitialTaskState(taskState *tasks.Task, queueName string) {
	// TODO: more header stuff like X-Appengine-* setting

	if taskState.GetName() == "" {
		taskID := strconv.FormatUint(uint64(rand.Uint64()), 10)
		taskState.Name = queueName + "/tasks/" + taskID
	}

	taskState.CreateTime = ptypes.TimestampNow()
	// For some reason the cloud does not set nanos
	taskState.CreateTime.Nanos = 0

	if taskState.GetScheduleTime() == nil {
		taskState.ScheduleTime = ptypes.TimestampNow()
	}
	if taskState.GetDispatchDeadline() == nil {
		taskState.DispatchDeadline = &pduration.Duration{Seconds: 600}
	}

	// This should probably be set somewhere else?
	taskState.View = tasks.Task_BASIC

	httpRequest := taskState.GetHttpRequest()

	if httpRequest != nil {
		if httpRequest.GetHttpMethod() == tasks.HttpMethod_HTTP_METHOD_UNSPECIFIED {
			httpRequest.HttpMethod = tasks.HttpMethod_POST
		}
		if httpRequest.GetHeaders() == nil {
			httpRequest.Headers = make(map[string]string)
		}
		// Override
		httpRequest.Headers["User-Agent"] = "Google-Cloud-Tasks"
	}

	appEngineHTTPRequest := taskState.GetAppEngineHttpRequest()

	if appEngineHTTPRequest != nil {
		if appEngineHTTPRequest.GetHttpMethod() == tasks.HttpMethod_HTTP_METHOD_UNSPECIFIED {
			appEngineHTTPRequest.HttpMethod = tasks.HttpMethod_POST
		}
		if appEngineHTTPRequest.GetHeaders() == nil {
			appEngineHTTPRequest.Headers = make(map[string]string)
		}

		appEngineHTTPRequest.Headers["User-Agent"] = "AppEngine-Google; (+http://code.google.com/appengine)"

		if appEngineHTTPRequest.GetBody() != nil {
			if _, ok := appEngineHTTPRequest.GetHeaders()["Content-Type"]; !ok {
				appEngineHTTPRequest.Headers["Content-Type"] = "application/octet-stream"
			}
		}

		if appEngineHTTPRequest.GetAppEngineRouting() == nil {
			appEngineHTTPRequest.AppEngineRouting = &tasks.AppEngineRouting{}
		}

		r := regexp.MustCompile("projects/([a-z0-9-]+)/locations/[a-z0-9-]+/queues/[a-z0-9-]+/tasks/[0-9]+")
		project := r.FindStringSubmatch(taskState.GetName())[1]

		host := project + ".appspot.com"
		emulatorHost := os.Getenv("APP_ENGINE_EMULATOR_HOST")
		if emulatorHost != "" {
			host = emulatorHost
		}

		if appEngineHTTPRequest.GetAppEngineRouting().GetService() != "" {
			host = appEngineHTTPRequest.GetAppEngineRouting().GetService() + "." + host
		}
		if appEngineHTTPRequest.GetAppEngineRouting().GetVersion() != "" {
			host = appEngineHTTPRequest.GetAppEngineRouting().GetVersion() + "." + host
		}
		if appEngineHTTPRequest.GetAppEngineRouting().GetInstance() != "" {
			host = appEngineHTTPRequest.GetAppEngineRouting().GetInstance() + "." + host
		}

		appEngineHTTPRequest.GetAppEngineRouting().Host = host

		if appEngineHTTPRequest.GetRelativeUri() == "" {
			appEngineHTTPRequest.RelativeUri = "/"
		}
	}
}

func updateStateForReschedule(task *Task) *tasks.Task {
	// The lock is to ensure a consistent state when updating
	task.stateMutex.Lock()
	taskState := task.state
	queueState := task.queue.state

	retryConfig := queueState.GetRetryConfig()

	minBackoff, _ := ptypes.Duration(retryConfig.GetMinBackoff())
	maxBackoff, _ := ptypes.Duration(retryConfig.GetMaxBackoff())

	doubling := taskState.GetDispatchCount() - 1
	if doubling > retryConfig.MaxDoublings {
		doubling = retryConfig.MaxDoublings
	}
	backoff := minBackoff * time.Duration(1<<uint32(doubling))
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	protoBackoff := ptypes.DurationProto(backoff)
	prevScheduleTime := taskState.GetScheduleTime()

	// Avoid int32 nanos overflow
	scheduleNanos := int64(prevScheduleTime.GetNanos()) + int64(protoBackoff.GetNanos())
	scheduleSeconds := prevScheduleTime.GetSeconds() + protoBackoff.GetSeconds()
	if scheduleNanos >= 1e9 {
		scheduleSeconds++
		scheduleNanos -= 1e9
	}

	taskState.ScheduleTime = &ptimestamp.Timestamp{
		Nanos:   int32(scheduleNanos),
		Seconds: scheduleSeconds,
	}

	frozenTaskState := proto.Clone(taskState).(*tasks.Task)
	task.stateMutex.Unlock()

	return frozenTaskState
}

func updateStateForDispatch(task *Task) *tasks.Task {
	task.stateMutex.Lock()
	taskState := task.state

	dispatchTime := ptypes.TimestampNow()

	taskState.LastAttempt = &tasks.Attempt{
		ScheduleTime: &ptimestamp.Timestamp{
			Nanos:   taskState.GetScheduleTime().GetNanos(),
			Seconds: taskState.GetScheduleTime().GetSeconds(),
		},
		DispatchTime: dispatchTime,
	}

	taskState.DispatchCount++

	if taskState.GetFirstAttempt() == nil {
		taskState.FirstAttempt = &tasks.Attempt{
			DispatchTime: dispatchTime,
		}
	}

	frozenTaskState := proto.Clone(taskState).(*tasks.Task)
	task.stateMutex.Unlock()

	return frozenTaskState
}

func updateStateAfterDispatch(task *Task, statusCode int) *tasks.Task {
	task.stateMutex.Lock()

	taskState := task.state

	rpcCode := toRPCStatusCode(statusCode)
	rpcCodeName := toCodeName(rpcCode)

	lastAttempt := taskState.GetLastAttempt()

	lastAttempt.ResponseTime = ptypes.TimestampNow()
	lastAttempt.ResponseStatus = &rpcstatus.Status{
		Code:    rpcCode,
		Message: fmt.Sprintf("%s(%d): HTTP status code %d", rpcCodeName, rpcCode, statusCode),
	}

	taskState.ResponseCount++

	frozenTaskState := proto.Clone(taskState).(*tasks.Task)
	task.stateMutex.Unlock()

	return frozenTaskState
}

func (task *Task) reschedule(retry bool, statusCode int) {
	if statusCode >= 200 && statusCode <= 299 {
		log.Println("Task done")
		task.onDone(task)
	} else {
		log.Println("Task exec error with status " + strconv.Itoa(statusCode))
		if retry {
			retryConfig := task.queue.state.GetRetryConfig()

			if task.state.DispatchCount >= retryConfig.GetMaxAttempts() {
				log.Println("Ran out of attempts")
			} else {
				updateStateForReschedule(task)
				task.Schedule()
			}
		}
	}
}

func dispatch(retry bool, taskState *tasks.Task) int {
	client := &http.Client{}
	client.Timeout, _ = ptypes.Duration(taskState.GetDispatchDeadline())

	var req *http.Request
	var headers map[string]string

	httpRequest := taskState.GetHttpRequest()
	appEngineHTTPRequest := taskState.GetAppEngineHttpRequest()

	if httpRequest != nil {
		method := toHTTPMethod(httpRequest.GetHttpMethod())

		req, _ = http.NewRequest(method, httpRequest.GetUrl(), bytes.NewBuffer(httpRequest.GetBody()))

		headers = httpRequest.GetHeaders()
	} else if appEngineHTTPRequest != nil {
		method := toHTTPMethod(appEngineHTTPRequest.GetHttpMethod())

		host := appEngineHTTPRequest.GetAppEngineRouting().GetHost()

		url := host + appEngineHTTPRequest.GetRelativeUri()

		req, _ = http.NewRequest(method, url, bytes.NewBuffer(appEngineHTTPRequest.GetBody()))

		headers = appEngineHTTPRequest.GetHeaders()
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, _ := client.Do(req)

	if resp != nil {
		// Don't need the response
		resp.Body.Close()
		return resp.StatusCode
	}

	return -1
}

func (task *Task) doDispatch(retry bool) {
	respCode := dispatch(retry, task.state)

	updateStateAfterDispatch(task, respCode)
	task.reschedule(retry, respCode)
}

// Attempt tries to execute a task
func (task *Task) Attempt() {
	updateStateForDispatch(task)

	task.doDispatch(true)
}

// Run runs the task outside of the normal queueing mechanism.
// This method is called directly by request.
func (task *Task) Run() *tasks.Task {
	taskState := updateStateForDispatch(task)

	go task.doDispatch(false)

	return taskState
}

// Delete cancels the task if it is queued for execution.
// This method is called directly by request.
func (task *Task) Delete() {
	task.cancelOnce.Do(func() {
		task.cancel <- true
	})
}

// Schedule schedules the task for execution.
// It is initially called by the queue, later by the task reschedule.
func (task *Task) Schedule() {
	scheduled, _ := ptypes.Timestamp(task.state.GetScheduleTime())

	fromNow := scheduled.Sub(time.Now())

	go func() {
		select {
		case <-time.After(fromNow):
			task.queue.fire <- task
			return
		case <-task.cancel:
			task.onDone(task)
			return
		}
	}()
}
