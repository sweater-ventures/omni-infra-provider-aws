package provider

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"go.uber.org/zap"
)

const (
	tagRequestID  = "omni-request-id"
	tagProviderID = "omni-provider-id"
	tagName       = "Name"
)

var liveInstanceStateNames = []string{
	string(types.InstanceStateNamePending),
	string(types.InstanceStateNameRunning),
	string(types.InstanceStateNameStopping),
	string(types.InstanceStateNameStopped),
}

// EC2API is the subset of the EC2 client used by the provisioner and orphan sweeper.
type EC2API interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	RunInstances(ctx context.Context, params *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	CreateTags(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
}

func describeAll(ctx context.Context, api EC2API, input *ec2.DescribeInstancesInput) ([]types.Instance, error) {
	if input == nil {
		input = &ec2.DescribeInstancesInput{}
	}

	var (
		instances []types.Instance
		token     *string
	)

	for {
		req := *input
		req.NextToken = token

		out, err := api.DescribeInstances(ctx, &req)
		if err != nil {
			return nil, err
		}

		for _, reservation := range out.Reservations {
			instances = append(instances, reservation.Instances...)
		}

		if out.NextToken == nil || *out.NextToken == "" {
			break
		}

		token = out.NextToken
	}

	return instances, nil
}

func instancesByRequestID(ctx context.Context, api EC2API, requestID string) ([]types.Instance, error) {
	return describeAll(ctx, api, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag:" + tagRequestID),
				Values: []string{requestID},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: liveInstanceStateNames,
			},
		},
	})
}

func taggedLiveInstances(ctx context.Context, api EC2API) ([]types.Instance, error) {
	return describeAll(ctx, api, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag-key"),
				Values: []string{tagRequestID},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: liveInstanceStateNames,
			},
		},
	})
}

func terminateInstanceIDs(ctx context.Context, api EC2API, logger *zap.Logger, ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	live := make([]string, 0, len(ids))

	for _, id := range ids {
		instances, err := describeAll(ctx, api, &ec2.DescribeInstancesInput{
			InstanceIds: []string{id},
		})
		if err != nil {
			if isInstanceNotFound(err) {
				logger.Warn("instance already terminated or does not exist", zap.String("instance-id", id))
				continue
			}

			return err
		}

		if len(instances) == 0 {
			continue
		}

		state := instances[0].State
		if state == nil || state.Name == types.InstanceStateNameTerminated || state.Name == types.InstanceStateNameShuttingDown {
			continue
		}

		live = append(live, id)
	}

	if len(live) == 0 {
		return nil
	}

	if _, err := api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: live,
	}); err != nil {
		if isInstanceNotFound(err) {
			logger.Warn("instance already terminated or does not exist", zap.Strings("instance-ids", live))
			return nil
		}

		return err
	}

	logger.Info("instance termination triggered", zap.Strings("instance-ids", live))

	return nil
}

func tagValue(inst types.Instance, key string) string {
	for _, tag := range inst.Tags {
		if aws.ToString(tag.Key) == key {
			return aws.ToString(tag.Value)
		}
	}

	return ""
}

func instanceLaunchTime(inst types.Instance) time.Time {
	if inst.LaunchTime == nil {
		return time.Time{}
	}

	return *inst.LaunchTime
}

func isLiveInstance(inst types.Instance) bool {
	if inst.State == nil {
		return false
	}

	switch inst.State.Name {
	case types.InstanceStateNamePending, types.InstanceStateNameRunning, types.InstanceStateNameStopping, types.InstanceStateNameStopped:
		return true
	default:
		return false
	}
}

func isInstanceNotFound(err error) bool {
	var apiErr smithy.APIError

	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidInstanceID.NotFound"
}
