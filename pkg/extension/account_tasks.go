package extension

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode"
)

const (
	MethodAccountTaskSchedule = "accountTasks.schedule"
	MethodAccountTaskCancel   = "accountTasks.cancel"
	EventAccountTaskUsage     = "accountTask.usage"
	MaxAccountTasks           = 32
)

type AccountTaskDescriptor struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}

type AccountTaskScheduleRequest struct {
	Task  string `json:"task"`
	Key   string `json:"key"`
	Job   string `json:"job"`
	DueAt int64  `json:"dueAt"`
}

type AccountTaskScheduleResult struct {
	OperationID string `json:"operationId"`
}

type AccountTaskCancelRequest struct {
	Task string `json:"task"`
	Key  string `json:"key"`
}

type AccountTaskCancelResult struct {
	Canceled bool `json:"canceled"`
}

type AccountTaskRequest struct {
	Task  string `json:"task"`
	Key   string `json:"key"`
	Job   string `json:"job"`
	DueAt int64  `json:"dueAt"`
	Model string `json:"model"`
	Proxy string `json:"proxy,omitempty"`
}

type AccountTaskResult struct {
	Outcome string   `json:"outcome"`
	Call    *OwnCall `json:"call,omitempty"`
}

func IsAccountTaskHostMethod(method string) bool {
	return method == MethodAccountTaskSchedule || method == MethodAccountTaskCancel
}

func (manifest Manifest) AccountTask(provider, id string) (AccountTaskDescriptor, bool) {
	for _, descriptor := range manifest.Providers {
		if descriptor.ID == provider {
			for _, task := range descriptor.AccountTasks {
				if task.ID == id {
					return task, true
				}
			}
		}
	}
	return AccountTaskDescriptor{}, false
}

func (manifest Manifest) validateAccountTasks() error {
	for _, provider := range manifest.Providers {
		if len(provider.AccountTasks) == 0 {
			continue
		}
		if manifest.Contract.Version.Minor < 9 || !manifest.Contract.Has(AccountAuth) ||
			!manifest.Contract.Has(OutboundExecutor) || !manifest.Contract.Has(ModuleRPC) ||
			!manifest.Contract.Has(UsageReporter) || manifest.Component != nil || len(provider.AccountTasks) > 16 {
			return errors.New("account tasks require SDK API 1.9, account auth, execution, module writes and usage")
		}
		seen := map[string]bool{}
		for _, task := range provider.AccountTasks {
			action, found := manifest.ResolveAction(task.Action)
			if !ownCallKind(task.ID) || seen[task.ID] || !found || action.Effect != "write" {
				return errors.New("account tasks require unique bounded IDs and declared write actions")
			}
			seen[task.ID] = true
		}
	}
	return nil
}

func (manifest Manifest) accountTaskAction(id string) bool {
	action, found := manifest.ResolveAction(id)
	if manifest.Contract.Version.Minor < 9 || !manifest.Contract.Has(ModuleRPC) ||
		!found || action.Effect != "write" || actionOwner(action) != componentID(manifest) {
		return false
	}
	if manifest.Component != nil {
		return slices.Contains(manifest.Component.AccountTaskActions, id)
	}
	if !manifest.Contract.Has(UsageReporter) {
		return false
	}
	for _, provider := range manifest.Providers {
		for _, task := range provider.AccountTasks {
			if task.Action == id {
				return true
			}
		}
	}
	return false
}

func validateAccountTaskCall(manifest Manifest, call OwnCall) error {
	allowed := manifest.Contract.Has(UsageReporter)
	if manifest.Component != nil {
		allowed = len(manifest.Component.AccountTaskActions) != 0
	}
	if manifest.Contract.Version.Minor < 9 || !allowed || !ownCallKind(call.Kind) || !validCallFacts(manifest, call) {
		return errors.New("account task usage requires SDK API 1.9, task authority and bounded usage facts")
	}
	return nil
}

func DecodeAccountTaskSchedule(raw json.RawMessage) (AccountTaskScheduleRequest, error) {
	var request AccountTaskScheduleRequest
	if len(raw) > 4096 || decodeParams(raw, &request) != nil ||
		!ownCallKind(request.Task) || request.Key == "" || !validExecutionText(request.Key, 256) ||
		request.Job == "" || !validExecutionText(request.Job, 256) ||
		request.DueAt < 946684800000 || request.DueAt > 1<<53-1 {
		return request, errors.New("account tasks require a bounded opaque key, job and due time")
	}
	return request, nil
}

func DecodeAccountTaskCancel(raw json.RawMessage) (AccountTaskCancelRequest, error) {
	var request AccountTaskCancelRequest
	if len(raw) > 4096 || decodeParams(raw, &request) != nil || !ownCallKind(request.Task) ||
		request.Key == "" || !validExecutionText(request.Key, 256) {
		return request, errors.New("account task cancellation requires its bounded task and key")
	}
	return request, nil
}

func DecodeAccountTaskRequest(raw json.RawMessage) (AccountTaskRequest, error) {
	var request AccountTaskRequest
	if len(raw) > 8192 || decodeParams(raw, &request) != nil ||
		!ownCallKind(request.Task) || request.Key == "" || !validExecutionText(request.Key, 256) ||
		request.Job == "" || !validExecutionText(request.Job, 256) ||
		request.DueAt < 946684800000 || request.DueAt > 1<<53-1 ||
		request.Model == "" || !validExecutionText(request.Model, 300) || strings.ContainsFunc(request.Model, unicode.IsSpace) ||
		!validExecutionText(request.Proxy, 4096) {
		return request, errors.New("invalid selected-account task input")
	}
	return request, nil
}

func DecodeAccountTaskResult(manifest Manifest, raw json.RawMessage) (AccountTaskResult, error) {
	var result AccountTaskResult
	if len(raw) > MaxChunkBytes || decodeParams(raw, &result) != nil {
		return result, errors.New("invalid account task result")
	}
	if result.Outcome != "completed" && result.Outcome != "not-started" && result.Outcome != "uncertain" ||
		result.Call != nil && validateAccountTaskCall(manifest, *result.Call) != nil {
		return result, errors.New("invalid account task outcome or usage evidence")
	}
	return result, nil
}

func (stream *Stream) ScheduleAccountTask(ctx context.Context, request AccountTaskScheduleRequest) (AccountTaskScheduleResult, error) {
	var result AccountTaskScheduleResult
	raw, err := json.Marshal(request)
	if err == nil {
		_, err = DecodeAccountTaskSchedule(raw)
	}
	if err == nil {
		err = stream.HostCall(ctx, MethodAccountTaskSchedule, request, &result)
	}
	return result, err
}

func (stream *Stream) CancelAccountTask(ctx context.Context, request AccountTaskCancelRequest) (AccountTaskCancelResult, error) {
	var result AccountTaskCancelResult
	raw, err := json.Marshal(request)
	if err == nil {
		_, err = DecodeAccountTaskCancel(raw)
	}
	if err == nil {
		err = stream.HostCall(ctx, MethodAccountTaskCancel, request, &result)
	}
	return result, err
}

func (stream *Stream) ReportAccountTaskUsage(ctx context.Context, call OwnCall) error {
	return stream.Emit(ctx, EventAccountTaskUsage, call)
}
