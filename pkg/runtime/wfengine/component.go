/*
Copyright 2023 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package wfengine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/microsoft/durabletask-go/api"
	"github.com/microsoft/durabletask-go/backend"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dapr/components-contrib/workflows"
	componentsV1alpha1 "github.com/dapr/dapr/pkg/apis/components/v1alpha1"
	"github.com/dapr/kit/logger"
)

var ComponentDefinition = componentsV1alpha1.Component{
	TypeMeta: metav1.TypeMeta{
		Kind: "Component",
	},
	ObjectMeta: metav1.ObjectMeta{
		Name: "dapr",
	},
	Spec: componentsV1alpha1.ComponentSpec{
		Type:     "workflow.dapr",
		Version:  "v1",
		Metadata: []componentsV1alpha1.MetadataItem{},
	},
}

func BuiltinWorkflowFactory(engine *WorkflowEngine) func(logger.Logger) workflows.Workflow {
	return func(logger logger.Logger) workflows.Workflow {
		return &workflowEngineComponent{
			logger: logger,
			client: backend.NewTaskHubClient(engine.backend),
		}
	}
}

type workflowEngineComponent struct {
	workflows.Workflow
	logger logger.Logger
	client backend.TaskHubClient
}

func (c *workflowEngineComponent) Init(metadata workflows.Metadata) error {
	c.logger.Info("initializing Dapr workflow component")
	return nil
}

func (c *workflowEngineComponent) Start(ctx context.Context, req *workflows.StartRequest) (*workflows.WorkflowReference, error) {
	if req.WorkflowName == "" {
		return nil, errors.New("a workflow name is required")
	}

	// Specifying the ID is optional - if not specified, a random ID will be generated by the client.
	var opts []api.NewOrchestrationOptions
	if req.WorkflowReference.InstanceID != "" {
		opts = append(opts, api.WithInstanceID(api.InstanceID(req.WorkflowReference.InstanceID)))
	}

	// Input is also optional. However, inputs are expected to be JSON-serializable.
	if req.Input != nil {
		opts = append(opts, api.WithInput(req.Input))
	}

	// Start time is also optional and must be in the RFC3339 format (e.g. 2009-11-10T23:00:00Z).
	if req.Options != nil {
		if startTimeRFC3339, ok := req.Options["dapr.workflow.start_time"]; ok {
			if startTime, err := time.Parse(time.RFC3339, startTimeRFC3339); err != nil {
				return nil, fmt.Errorf("start times must be in RFC3339 format (e.g. \"2009-11-10T23:00:00Z\")")
			} else {
				opts = append(opts, api.WithStartTime(startTime))
			}
		}
	}

	var workflowID api.InstanceID
	var err error

	workflowID, err = c.client.ScheduleNewOrchestration(ctx, req.WorkflowName, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to start workflow: %w", err)
	}

	c.logger.Infof("created new workflow instance with ID '%s'", workflowID)
	wfRef := &workflows.WorkflowReference{
		InstanceID: string(workflowID),
	}
	return wfRef, nil
}

func (c *workflowEngineComponent) Terminate(ctx context.Context, req *workflows.WorkflowReference) error {
	if req.InstanceID == "" {
		return fmt.Errorf("a workflow instance ID is required")
	}

	if err := c.client.TerminateOrchestration(ctx, api.InstanceID(req.InstanceID), ""); err != nil {
		return fmt.Errorf("failed to terminate workflow %s: %w", req.InstanceID, err)
	}

	c.logger.Infof("scheduled termination for workflow instance '%s'", req.InstanceID)
	return nil
}

func (c *workflowEngineComponent) Get(ctx context.Context, req *workflows.WorkflowReference) (*workflows.StateResponse, error) {
	if req.InstanceID == "" {
		return nil, fmt.Errorf("a workflow instance ID is required")
	}

	if metadata, err := c.client.FetchOrchestrationMetadata(ctx, api.InstanceID(req.InstanceID)); err != nil {
		return nil, fmt.Errorf("failed to get workflow metadata for '%s': %w", req.InstanceID, err)
	} else {
		res := &workflows.StateResponse{
			WFInfo: workflows.WorkflowReference{
				InstanceID: req.InstanceID,
			},
			StartTime: metadata.CreatedAt.Format(time.RFC3339),
			Metadata: map[string]string{
				"dapr.workflow.name":           metadata.Name,
				"dapr.workflow.runtime_status": getStatusString(int32(metadata.RuntimeStatus)),
				"dapr.workflow.input":          metadata.SerializedInput,
				"dapr.workflow.custom_status":  metadata.SerializedCustomStatus,
				"dapr.workflow.last_updated":   metadata.LastUpdatedAt.Format(time.RFC3339),
			},
		}

		// Status-specific fields
		if metadata.FailureDetails != nil {
			res.Metadata["dapr.workflow.failure.error_type"] = metadata.FailureDetails.ErrorType
			res.Metadata["dapr.workflow.failure.error_message"] = metadata.FailureDetails.ErrorMessage
		} else if metadata.IsComplete() {
			res.Metadata["dapr.workflow.output"] = metadata.SerializedOutput
		}

		return res, nil
	}
}

// Status values are defined at: https://github.com/microsoft/durabletask-go/blob/119b361079c45e368f83b223888d56a436ac59b9/internal/protos/orchestrator_service.pb.go#L42-L64
func getStatusString(status int32) string {
	switch status {
	case 0:
		return "RUNNING"
	case 1:
		return "COMPLETED"
	case 2:
		return "CONTINUED_AS_NEW"
	case 3:
		return "FAILED"
	case 4:
		return "CANCELED"
	case 5:
		return "TERMINATED"
	case 6:
		return "PENDING"
	case 7:
		return "SUSPENDED"
	default:
		return "UNKNOWN"
	}
}