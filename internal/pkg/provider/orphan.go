package provider

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-aws/internal/pkg/provider/resources"
)

const (
	// DefaultOrphanSweepInterval is how often the sweeper lists Omni and EC2.
	DefaultOrphanSweepInterval = 5 * time.Minute
	// DefaultOrphanSweepGrace is how long Omni must successfully report an instance
	// as unknown before it can be terminated.
	DefaultOrphanSweepGrace = 60 * time.Minute
	// DefaultOrphanSweepMaxDeletes is the maximum number of instances terminated
	// in a single successful sweep.
	DefaultOrphanSweepMaxDeletes = 1
	minOrphanHits                = 2
)

type orphanSighting struct {
	firstSeen time.Time
	hits      int
}

type taggedInstance struct {
	ID         string
	ProviderID string
	LaunchTime time.Time
}

// OrphanSweeper terminates EC2 instances this provider created that Omni does
// not track, after a conservative streak of successful observations.
type OrphanSweeper struct {
	ec2           EC2API
	listTracked   func(context.Context) (map[string]struct{}, error)
	logger        *zap.Logger
	now           func() time.Time
	seen          map[string]orphanSighting
	mu            sync.Mutex
	providerID    string
	interval      time.Duration
	grace         time.Duration
	maxDeletes    int
	gen           uint64
	lastGoodCount int
	sawNonEmpty   bool
}

// OrphanSweeperConfig is the production constructor input.
type OrphanSweeperConfig struct {
	EC2        EC2API
	State      state.CoreState
	Logger     *zap.Logger
	ProviderID string
	Interval   time.Duration
	Grace      time.Duration
	MaxDeletes int
}

func NewOrphanSweeper(cfg OrphanSweeperConfig) *OrphanSweeper {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	maxDeletes := cfg.MaxDeletes
	if maxDeletes < 1 {
		maxDeletes = DefaultOrphanSweepMaxDeletes
	}

	return &OrphanSweeper{
		ec2: cfg.EC2,
		listTracked: func(ctx context.Context) (map[string]struct{}, error) {
			return trackedInstanceIDs(ctx, cfg.State)
		},
		logger:     logger,
		now:        time.Now,
		seen:       map[string]orphanSighting{},
		providerID: cfg.ProviderID,
		interval:   cfg.Interval,
		grace:      cfg.Grace,
		maxDeletes: maxDeletes,
	}
}

func (s *OrphanSweeper) Run(ctx context.Context) error {
	s.logger.Info("starting orphan instance sweeper",
		zap.Duration("interval", s.interval),
		zap.Duration("grace", s.grace),
		zap.Int("max-deletes-per-sweep", s.maxDeletes),
	)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.sweep(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}

				s.logger.Error("orphan sweep failed", zap.Error(err))
			}
		}
	}
}

// Reset clears every in-progress orphan streak. Call this when Omni asks
// the provider to create a machine so a mistaken delete cannot keep
// draining more instances while the cluster is being repaired.
func (s *OrphanSweeper) Reset(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	s.resetSightingsLocked(reason)
}

func (s *OrphanSweeper) currentGen() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

func (s *OrphanSweeper) sweep(ctx context.Context) error {
	genStart := s.currentGen()

	tracked, err := s.listTracked(ctx)
	if err != nil {
		s.Reset("omni list failed")
		return fmt.Errorf("listing Omni machines: %w", err)
	}

	instances, err := taggedLiveInstances(ctx, s.ec2)
	if err != nil {
		s.Reset("ec2 list failed")
		return fmt.Errorf("listing EC2 instances: %w", err)
	}

	s.mu.Lock()
	if !s.sawNonEmpty && len(tracked) == 0 {
		s.mu.Unlock()
		s.logger.Info("orphan sweep waiting for Omni to list at least one instance")
		return nil
	}
	if implausibleTrackedDrop(s.lastGoodCount, len(tracked)) {
		prev := s.lastGoodCount
		s.gen++
		s.resetSightingsLocked("tracked set dropped implausibly")
		s.mu.Unlock()
		s.logger.Warn("skipping orphan deletes after implausible Omni list",
			zap.Int("previous-tracked", prev),
			zap.Int("current-tracked", len(tracked)),
		)
		return nil
	}
	if len(tracked) > 0 {
		s.sawNonEmpty = true
	}
	s.lastGoodCount = len(tracked)
	s.mu.Unlock()

	if err := s.backfillProviderTags(ctx, instances, tracked); err != nil {
		s.logger.Error("backfill provider tags failed", zap.Error(err))
	}

	now := s.now()
	s.mu.Lock()
	eligible, next := evaluateOrphans(now, s.grace, s.providerID, tracked, toTaggedInstances(instances), s.seen)
	s.seen = next
	s.mu.Unlock()

	if len(eligible) == 0 {
		return nil
	}

	if s.currentGen() != genStart {
		s.logger.Info("skipping orphan deletes, provision reset during sweep")
		return nil
	}

	trackedNow, err := s.listTracked(ctx)
	if err != nil {
		s.Reset("omni list failed")
		return fmt.Errorf("re-listing Omni machines before delete: %w", err)
	}

	filtered := eligible[:0]
	for _, id := range eligible {
		if _, ok := trackedNow[id]; ok {
			continue
		}
		filtered = append(filtered, id)
	}
	eligible = filtered

	if s.currentGen() != genStart {
		s.logger.Info("skipping orphan deletes, provision reset during sweep")
		return nil
	}

	if len(eligible) == 0 {
		return nil
	}

	terminate := eligible
	if len(terminate) > s.maxDeletes {
		terminate = terminate[:s.maxDeletes]
		s.logger.Info("rate-limiting orphan terminations",
			zap.Int("eligible", len(eligible)),
			zap.Int("terminating", len(terminate)),
			zap.Int("max-deletes-per-sweep", s.maxDeletes),
		)
	}

	s.logger.Warn("terminating orphan instances",
		zap.Strings("instance-ids", terminate),
		zap.Int("tracked-omni-instances", len(trackedNow)),
	)

	return terminateInstanceIDs(ctx, s.ec2, s.logger, terminate)
}

func implausibleTrackedDrop(prev, curr int) bool {
	if prev <= 0 {
		return false
	}

	drop := prev - curr
	if drop <= 1 {
		return false
	}

	return drop*2 > prev
}

func (s *OrphanSweeper) backfillProviderTags(ctx context.Context, instances []types.Instance, tracked map[string]struct{}) error {
	if len(tracked) == 0 {
		return nil
	}

	var resources []string
	for _, inst := range instances {
		id := aws.ToString(inst.InstanceId)
		if _, ok := tracked[id]; !ok {
			continue
		}
		if tagValue(inst, tagProviderID) != "" {
			continue
		}

		resources = append(resources, id)
	}

	if len(resources) == 0 {
		return nil
	}

	_, err := s.ec2.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: resources,
		Tags: []types.Tag{
			{Key: aws.String(tagProviderID), Value: aws.String(s.providerID)},
		},
	})
	if err != nil {
		return err
	}

	s.logger.Info("backfilled omni-provider-id tags", zap.Strings("instance-ids", resources))
	return nil
}

func (s *OrphanSweeper) resetSightingsLocked(reason string) {
	if len(s.seen) == 0 {
		return
	}

	s.logger.Warn("resetting orphan observation streaks",
		zap.String("reason", reason),
		zap.Int("candidates", len(s.seen)),
	)
	s.seen = map[string]orphanSighting{}
}

func trackedInstanceIDs(ctx context.Context, st state.CoreState) (map[string]struct{}, error) {
	machines, err := safe.StateListAll[*resources.Machine](ctx, st)
	if err != nil {
		return nil, err
	}

	ids := make(map[string]struct{})
	for machine := range machines.All() {
		spec := machine.TypedSpec().Value
		if spec == nil {
			continue
		}

		id := spec.GetInstanceId()
		if id == "" {
			continue
		}

		ids[id] = struct{}{}
	}

	return ids, nil
}

func toTaggedInstances(instances []types.Instance) []taggedInstance {
	out := make([]taggedInstance, 0, len(instances))
	for _, inst := range instances {
		id := aws.ToString(inst.InstanceId)
		if id == "" {
			continue
		}

		out = append(out, taggedInstance{
			ID:         id,
			ProviderID: tagValue(inst, tagProviderID),
			LaunchTime: instanceLaunchTime(inst),
		})
	}

	return out
}

// evaluateOrphans records instances that a successful Omni list did not track.
//
// Termination requires a consecutive streak of successful Omni reads spanning
// at least `grace`, with at least two observations. A failed Omni or EC2 list
// is handled by the caller resetting sightings; downtime is not treated as
// "Omni has no record of this instance".
func evaluateOrphans(
	now time.Time,
	grace time.Duration,
	providerID string,
	tracked map[string]struct{},
	instances []taggedInstance,
	prev map[string]orphanSighting,
) (eligible []string, next map[string]orphanSighting) {
	next = make(map[string]orphanSighting, len(prev))

	type candidate struct {
		id         string
		launchTime time.Time
	}

	var ready []candidate

	for _, inst := range instances {
		if inst.ProviderID != providerID {
			continue
		}

		if _, ok := tracked[inst.ID]; ok {
			continue
		}

		sighting, ok := prev[inst.ID]
		if !ok {
			next[inst.ID] = orphanSighting{firstSeen: now, hits: 1}
			continue
		}

		sighting.hits++
		next[inst.ID] = sighting

		if sighting.hits < minOrphanHits {
			continue
		}

		if now.Sub(sighting.firstSeen) < grace {
			continue
		}

		if inst.LaunchTime.IsZero() || now.Sub(inst.LaunchTime) < grace {
			continue
		}

		ready = append(ready, candidate{id: inst.ID, launchTime: inst.LaunchTime})
	}

	sort.Slice(ready, func(i, j int) bool {
		if !ready[i].launchTime.Equal(ready[j].launchTime) {
			return ready[i].launchTime.Before(ready[j].launchTime)
		}

		return ready[i].id < ready[j].id
	})

	eligible = make([]string, len(ready))
	for i, c := range ready {
		eligible[i] = c.id
	}

	return eligible, next
}
