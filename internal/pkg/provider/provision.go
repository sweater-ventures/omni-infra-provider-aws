package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-aws/internal/pkg/provider/meta"
	"github.com/siderolabs/omni-infra-provider-aws/internal/pkg/provider/resources"
)

const (
	instanceCreatedAtAnnotation = "aws.omni.sidero.dev/instance-created-at"
	clientTokenAnnotation       = "aws.omni.sidero.dev/client-token"
	launchAttemptAnnotation     = "aws.omni.sidero.dev/launch-attempt"
	waitRunningRetryInterval    = 10 * time.Second
	instanceNotFoundGracePeriod = 2 * time.Minute
)

type Provisioner struct {
	ec2                EC2API
	onProvisionRequest func()
	region             string
}

func NewProvisioner(ec2Client EC2API, region string) *Provisioner {
	return &Provisioner{
		ec2:    ec2Client,
		region: region,
	}
}

// SetOnProvisionRequest registers a hook invoked when Omni asks this provider
// to create a machine. The orphan sweeper uses this to reset observation streaks.
func (p *Provisioner) SetOnProvisionRequest(fn func()) {
	p.onProvisionRequest = fn
}

func (p *Provisioner) notifyProvisionRequest() {
	if p.onProvisionRequest != nil {
		p.onProvisionRequest()
	}
}

func (p *Provisioner) ProvisionSteps() []provision.Step[*resources.Machine] {
	return []provision.Step[*resources.Machine]{
		provision.NewStep("lookupAMI", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			var data Data
			if err := pctx.UnmarshalProviderData(&data); err != nil {
				return err
			}

			arch := data.Arch
			if arch == "" {
				arch = "amd64"
			}

			version := pctx.GetTalosVersion()
			if version == "" {
				return fmt.Errorf("talos version is not set in machine request")
			}

			amiID, err := LookupAMI(ctx, p.region, arch, version)
			if err != nil {
				return err
			}

			pctx.State.TypedSpec().Value.AmiId = amiID
			pctx.State.TypedSpec().Value.Region = p.region
			logger.Info("looked up AMI", zap.String("ami", amiID), zap.String("version", version))
			return nil
		}),
		provision.NewStep("runInstance", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			hadID := pctx.State.TypedSpec().Value.InstanceId != ""
			if err := p.createInstance(ctx, logger, pctx); err != nil {
				return err
			}
			if !hadID && pctx.State.TypedSpec().Value.InstanceId != "" {
				return provision.NewRetryInterval(waitRunningRetryInterval)
			}

			return nil
		}),
		provision.NewStep("waitRunning", func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
			spec := pctx.State.TypedSpec().Value
			hadID := spec.InstanceId != ""
			if !hadID {
				if err := p.createInstance(ctx, logger, pctx); err != nil {
					return err
				}

				return provision.NewRetryInterval(waitRunningRetryInterval)
			}

			instances, err := describeAll(ctx, p.ec2, &ec2.DescribeInstancesInput{
				InstanceIds: []string{spec.InstanceId},
			})
			if err != nil {
				if isInstanceNotFound(err) {
					return p.handleMissingInstance(ctx, logger, pctx)
				}

				return err
			}

			if len(instances) == 0 {
				return p.handleMissingInstance(ctx, logger, pctx)
			}

			instance := instances[0]
			if instance.State != nil && instance.State.Name == types.InstanceStateNameRunning {
				if err := p.terminateSiblings(ctx, logger, pctx.GetRequestID(), spec.InstanceId); err != nil {
					return err
				}

				return nil
			}

			if instance.State != nil && (instance.State.Name == types.InstanceStateNameTerminated || instance.State.Name == types.InstanceStateNameShuttingDown) {
				logger.Warn("instance is terminated, creating a new one", zap.String("instance-id", spec.InstanceId))
				spec.InstanceId = ""
				bumpLaunchToken(pctx.State, pctx.GetRequestID())

				return provision.NewRetryInterval(waitRunningRetryInterval)
			}

			return provision.NewRetryInterval(waitRunningRetryInterval)
		}),
	}
}

func (p *Provisioner) handleMissingInstance(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
	spec := pctx.State.TypedSpec().Value
	now := time.Now()
	createdAt := createdAtFrom(pctx.State)

	retry, stamp := missingInstanceShouldRetry(createdAt, now, instanceNotFoundGracePeriod)
	if stamp {
		setCreatedAt(pctx.State, now)
	}

	if retry {
		logger.Warn("instance not visible yet, retrying",
			zap.String("instance-id", spec.InstanceId),
		)

		return provision.NewRetryInterval(waitRunningRetryInterval)
	}

	logger.Warn("instance still missing after grace period, creating a replacement",
		zap.String("instance-id", spec.InstanceId),
		zap.Duration("age", now.Sub(createdAt)),
	)
	spec.InstanceId = ""
	bumpLaunchToken(pctx.State, pctx.GetRequestID())

	return provision.NewRetryInterval(waitRunningRetryInterval)
}

func missingInstanceShouldRetry(createdAt, now time.Time, grace time.Duration) (retry, stampCreatedAt bool) {
	if createdAt.IsZero() {
		return true, true
	}

	return now.Sub(createdAt) < grace, false
}

func createdAtFrom(machine *resources.Machine) time.Time {
	raw, ok := machine.Metadata().Annotations().Get(instanceCreatedAtAnnotation)
	if !ok {
		return time.Time{}
	}

	unix, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}
	}

	return time.Unix(unix, 0)
}

func setCreatedAt(machine *resources.Machine, t time.Time) {
	machine.Metadata().Annotations().Set(instanceCreatedAtAnnotation, strconv.FormatInt(t.Unix(), 10))
}

func clientTokenFrom(machine *resources.Machine) string {
	raw, _ := machine.Metadata().Annotations().Get(clientTokenAnnotation)
	return raw
}

func launchAttempt(machine *resources.Machine) int {
	raw, ok := machine.Metadata().Annotations().Get(launchAttemptAnnotation)
	if !ok {
		return 0
	}

	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}

	return n
}

func setLaunchToken(machine *resources.Machine, requestID string, attempt int) {
	machine.Metadata().Annotations().Set(launchAttemptAnnotation, strconv.Itoa(attempt))
	machine.Metadata().Annotations().Set(clientTokenAnnotation, makeClientToken(requestID, attempt))
}

func bumpLaunchToken(machine *resources.Machine, requestID string) {
	setLaunchToken(machine, requestID, launchAttempt(machine)+1)
}

func makeClientToken(requestID string, attempt int) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, requestID)
	token := fmt.Sprintf("%s-%d", safe, attempt)
	if len(token) <= 64 {
		return token
	}

	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x-%d", sum[:16], attempt)
}

func (p *Provisioner) createInstance(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
	spec := pctx.State.TypedSpec().Value
	if spec.InstanceId != "" {
		return nil
	}

	requestID := pctx.GetRequestID()
	token := clientTokenFrom(pctx.State)
	if token == "" {
		setLaunchToken(pctx.State, requestID, launchAttempt(pctx.State))
		return provision.NewRetryInterval(time.Second)
	}

	existing, err := instancesByRequestID(ctx, p.ec2, requestID)
	if err != nil {
		return err
	}

	if inst := newestLiveInstance(existing); inst != nil {
		p.notifyProvisionRequest()
		id := aws.ToString(inst.InstanceId)
		spec.InstanceId = id
		pctx.SetMachineInfraID(id)
		if launched := instanceLaunchTime(*inst); !launched.IsZero() {
			setCreatedAt(pctx.State, launched)
		} else {
			setCreatedAt(pctx.State, time.Now())
		}
		logger.Info("adopted existing instance", zap.String("instance-id", id))

		return nil
	}

	var data Data
	if err := pctx.UnmarshalProviderData(&data); err != nil {
		return err
	}

	subnetID := data.GetSubnetID(requestID)
	if subnetID != "" && len(data.SecurityGroupIDs) == 0 {
		return fmt.Errorf("security_group_ids must be specified when subnet_id or subnet_ids is provided")
	}

	p.notifyProvisionRequest()

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String(spec.AmiId),
		InstanceType: types.InstanceType(data.InstanceType),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		ClientToken:  aws.String(token),
		UserData:     aws.String(base64.StdEncoding.EncodeToString([]byte(pctx.ConnectionParams.JoinConfig))),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags: []types.Tag{
					{
						Key:   aws.String(tagRequestID),
						Value: aws.String(requestID),
					},
					{
						Key:   aws.String(tagProviderID),
						Value: aws.String(meta.ProviderID),
					},
					{
						Key:   aws.String(tagName),
						Value: aws.String(fmt.Sprintf("omni-%s", requestID)),
					},
				},
			},
		},
	}

	if subnetID != "" {
		input.SubnetId = aws.String(subnetID)
		logger.Info("selected subnet", zap.String("subnet-id", subnetID))
	}

	if len(data.SecurityGroupIDs) > 0 {
		input.SecurityGroupIds = data.SecurityGroupIDs
	}

	if data.IamInstanceProfile != "" {
		input.IamInstanceProfile = &types.IamInstanceProfileSpecification{
			Name: aws.String(data.IamInstanceProfile),
		}
	}

	if data.VolumeSize > 0 {
		input.BlockDeviceMappings = []types.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/xvda"),
				Ebs: &types.EbsBlockDevice{
					VolumeSize: aws.Int32(int32(data.VolumeSize)),
				},
			},
		}
	}

	out, err := p.ec2.RunInstances(ctx, input)
	if err != nil {
		return err
	}

	if len(out.Instances) == 0 {
		return fmt.Errorf("no instances created")
	}

	instanceID := aws.ToString(out.Instances[0].InstanceId)
	spec.InstanceId = instanceID
	pctx.SetMachineInfraID(instanceID)
	setCreatedAt(pctx.State, time.Now())

	logger.Info("instance created", zap.String("instance-id", instanceID))

	return nil
}

func newestLiveInstance(instances []types.Instance) *types.Instance {
	var best *types.Instance
	var bestAt time.Time

	for i := range instances {
		inst := &instances[i]
		if !isLiveInstance(*inst) {
			continue
		}

		id := aws.ToString(inst.InstanceId)
		if id == "" {
			continue
		}

		launched := instanceLaunchTime(*inst)
		if best == nil || launched.After(bestAt) {
			best = inst
			bestAt = launched
		}
	}

	return best
}

func (p *Provisioner) terminateSiblings(ctx context.Context, logger *zap.Logger, requestID, keepID string) error {
	instances, err := instancesByRequestID(ctx, p.ec2, requestID)
	if err != nil {
		return err
	}

	extras := make([]string, 0)
	for _, inst := range instances {
		id := aws.ToString(inst.InstanceId)
		if id == "" || id == keepID {
			continue
		}

		extras = append(extras, id)
	}

	if len(extras) == 0 {
		return nil
	}

	logger.Warn("terminating extra instances for request",
		zap.String("request-id", requestID),
		zap.String("keep", keepID),
		zap.Strings("instance-ids", extras),
	)

	return terminateInstanceIDs(ctx, p.ec2, logger, extras)
}

func (p *Provisioner) Deprovision(ctx context.Context, logger *zap.Logger, machine *resources.Machine, machineRequest *infra.MachineRequest) error {
	requestID := machineRequest.Metadata().ID()

	instances, err := instancesByRequestID(ctx, p.ec2, requestID)
	if err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(instances))
	ids := make([]string, 0, len(instances)+1)

	for _, inst := range instances {
		id := aws.ToString(inst.InstanceId)
		if id == "" {
			continue
		}

		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	if specID := machine.TypedSpec().Value.InstanceId; specID != "" {
		if _, ok := seen[specID]; !ok {
			ids = append(ids, specID)
		}
	}

	if len(ids) == 0 {
		logger.Warn("no instances to terminate", zap.String("request-id", requestID))
		return nil
	}

	return terminateInstanceIDs(ctx, p.ec2, logger, ids)
}
