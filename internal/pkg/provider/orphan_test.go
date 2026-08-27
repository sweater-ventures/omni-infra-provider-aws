package provider

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"go.uber.org/zap"
)

func TestEvaluateOrphans(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	grace := time.Hour
	providerID := "aws-prod"
	oldLaunch := now.Add(-2 * time.Hour)

	orphan := taggedInstance{ID: "i-orphan", ProviderID: providerID, LaunchTime: oldLaunch}
	tracked := taggedInstance{ID: "i-tracked", ProviderID: providerID, LaunchTime: oldLaunch}
	otherProvider := taggedInstance{ID: "i-other", ProviderID: "aws-dev", LaunchTime: oldLaunch}
	young := taggedInstance{ID: "i-young", ProviderID: providerID, LaunchTime: now.Add(-10 * time.Minute)}
	legacy := taggedInstance{ID: "i-legacy", LaunchTime: oldLaunch}

	t.Run("first sighting is not eligible", func(t *testing.T) {
		eligible, next := evaluateOrphans(now, grace, providerID, map[string]struct{}{}, []taggedInstance{orphan}, nil)
		if len(eligible) != 0 {
			t.Fatalf("eligible=%v, want none", eligible)
		}
		if next["i-orphan"].hits != 1 {
			t.Fatalf("hits=%d, want 1", next["i-orphan"].hits)
		}
	})

	t.Run("tracked instance is dropped", func(t *testing.T) {
		prev := map[string]orphanSighting{"i-tracked": {firstSeen: now.Add(-2 * time.Hour), hits: 10}}
		eligible, next := evaluateOrphans(now, grace, providerID, map[string]struct{}{"i-tracked": {}}, []taggedInstance{tracked}, prev)
		if len(eligible) != 0 {
			t.Fatalf("eligible=%v, want none", eligible)
		}
		if _, ok := next["i-tracked"]; ok {
			t.Fatal("tracked instance should leave the sighting map")
		}
	})

	t.Run("other provider is ignored", func(t *testing.T) {
		eligible, next := evaluateOrphans(now, grace, providerID, map[string]struct{}{}, []taggedInstance{otherProvider}, nil)
		if len(eligible) != 0 || len(next) != 0 {
			t.Fatalf("eligible=%v next=%v", eligible, next)
		}
	})

	t.Run("untagged instances are ignored", func(t *testing.T) {
		prev := map[string]orphanSighting{"i-legacy": {firstSeen: now.Add(-time.Hour), hits: 1}}
		eligible, next := evaluateOrphans(now, grace, providerID, map[string]struct{}{}, []taggedInstance{legacy}, prev)
		if len(eligible) != 0 {
			t.Fatalf("eligible=%v, want none", eligible)
		}
		if _, ok := next["i-legacy"]; ok {
			t.Fatal("untagged instance should not enter the sighting map")
		}
	})

	t.Run("young instance is not terminated even after streak", func(t *testing.T) {
		prev := map[string]orphanSighting{"i-young": {firstSeen: now.Add(-time.Hour), hits: 20}}
		eligible, next := evaluateOrphans(now, grace, providerID, map[string]struct{}{}, []taggedInstance{young}, prev)
		if len(eligible) != 0 {
			t.Fatalf("eligible=%v, want none", eligible)
		}
		if next["i-young"].hits != 21 {
			t.Fatalf("hits=%d, want 21", next["i-young"].hits)
		}
	})

	t.Run("consecutive successful untracked observations become eligible", func(t *testing.T) {
		prev := map[string]orphanSighting{"i-orphan": {firstSeen: now.Add(-time.Hour), hits: 1}}
		eligible, next := evaluateOrphans(now, grace, providerID, map[string]struct{}{}, []taggedInstance{orphan}, prev)
		if len(eligible) != 1 || eligible[0] != "i-orphan" {
			t.Fatalf("eligible=%v, want i-orphan", eligible)
		}
		if next["i-orphan"].hits != 2 {
			t.Fatalf("hits=%d, want 2", next["i-orphan"].hits)
		}
	})

	t.Run("oldest launch time is terminated first", func(t *testing.T) {
		a := taggedInstance{ID: "i-a", ProviderID: providerID, LaunchTime: now.Add(-3 * time.Hour)}
		b := taggedInstance{ID: "i-b", ProviderID: providerID, LaunchTime: now.Add(-2 * time.Hour)}
		prev := map[string]orphanSighting{
			"i-a": {firstSeen: now.Add(-time.Hour), hits: 5},
			"i-b": {firstSeen: now.Add(-time.Hour), hits: 5},
		}
		eligible, _ := evaluateOrphans(now, grace, providerID, map[string]struct{}{}, []taggedInstance{b, a}, prev)
		if len(eligible) != 2 || eligible[0] != "i-a" || eligible[1] != "i-b" {
			t.Fatalf("eligible=%v, want i-a then i-b", eligible)
		}
	})
}

func TestSweepOmniUnavailableDoesNotDelete(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := now
	orphan := types.Instance{
		InstanceId: aws.String("i-orphan"),
		LaunchTime: aws.Time(now.Add(-3 * time.Hour)),
		State:      &types.InstanceState{Name: types.InstanceStateNameRunning},
		Tags: []types.Tag{
			{Key: aws.String(tagRequestID), Value: aws.String("req-1")},
			{Key: aws.String(tagProviderID), Value: aws.String("aws-prod")},
		},
	}
	fake := &fakeEC2{instances: []types.Instance{orphan}}

	var omniErr error
	s := &OrphanSweeper{
		ec2: fake,
		listTracked: func(context.Context) (map[string]struct{}, error) {
			if omniErr != nil {
				return nil, omniErr
			}

			return map[string]struct{}{}, nil
		},
		logger:        zap.NewNop(),
		now:           func() time.Time { return clock },
		seen:          map[string]orphanSighting{},
		providerID:    "aws-prod",
		grace:         time.Hour,
		maxDeletes:    1,
		sawNonEmpty:   true,
		lastGoodCount: 0,
	}

	ctx := context.Background()

	if err := s.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.terminated != nil {
		t.Fatalf("terminated on first sighting: %v", fake.terminated)
	}

	clock = clock.Add(30 * time.Minute)
	if err := s.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.terminated != nil {
		t.Fatalf("terminated before grace: %v", fake.terminated)
	}

	omniErr = errors.New("omni unavailable")
	clock = clock.Add(45 * time.Minute)
	if err := s.sweep(ctx); err == nil {
		t.Fatal("expected omni list error")
	}
	if fake.terminated != nil {
		t.Fatalf("terminated while omni was down: %v", fake.terminated)
	}
	if len(s.seen) != 0 {
		t.Fatalf("sightings should reset on omni failure, got %v", s.seen)
	}

	omniErr = nil
	clock = clock.Add(5 * time.Minute)
	if err := s.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.terminated != nil {
		t.Fatalf("terminated after outage on a fresh streak: %v", fake.terminated)
	}
	if s.seen["i-orphan"].hits != 1 {
		t.Fatalf("hits=%d, want 1 after reset", s.seen["i-orphan"].hits)
	}

	clock = clock.Add(time.Hour)
	if err := s.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.terminated) != 1 || fake.terminated[0] != "i-orphan" {
		t.Fatalf("terminated=%v, want i-orphan after a full healthy streak", fake.terminated)
	}
}

func TestSweepProvisionRequestResetsTimers(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := now
	orphan := types.Instance{
		InstanceId: aws.String("i-orphan"),
		LaunchTime: aws.Time(now.Add(-3 * time.Hour)),
		State:      &types.InstanceState{Name: types.InstanceStateNameRunning},
		Tags: []types.Tag{
			{Key: aws.String(tagRequestID), Value: aws.String("req-1")},
			{Key: aws.String(tagProviderID), Value: aws.String("aws-prod")},
		},
	}
	fake := &fakeEC2{instances: []types.Instance{orphan}}
	s := &OrphanSweeper{
		ec2: fake,
		listTracked: func(context.Context) (map[string]struct{}, error) {
			return map[string]struct{}{}, nil
		},
		logger:        zap.NewNop(),
		now:           func() time.Time { return clock },
		seen:          map[string]orphanSighting{},
		providerID:    "aws-prod",
		grace:         time.Hour,
		maxDeletes:    1,
		sawNonEmpty:   true,
		lastGoodCount: 0,
	}

	ctx := context.Background()
	if err := s.sweep(ctx); err != nil {
		t.Fatal(err)
	}

	clock = clock.Add(time.Hour)
	s.Reset("omni requested machine creation")
	if err := s.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.terminated != nil {
		t.Fatalf("terminated after provision-request reset: %v", fake.terminated)
	}
	if s.seen["i-orphan"].hits != 1 {
		t.Fatalf("hits=%d, want 1 after reset", s.seen["i-orphan"].hits)
	}

	clock = clock.Add(time.Hour)
	if err := s.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.terminated) != 1 || fake.terminated[0] != "i-orphan" {
		t.Fatalf("terminated=%v, want i-orphan after a full streak following the reset", fake.terminated)
	}
}

func TestSweepRateLimit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	instances := make([]types.Instance, 3)
	for i := range instances {
		instances[i] = types.Instance{
			InstanceId: aws.String(fmt.Sprintf("i-%d", i)),
			LaunchTime: aws.Time(now.Add(-time.Duration(3-i) * time.Hour)),
			State:      &types.InstanceState{Name: types.InstanceStateNameRunning},
			Tags: []types.Tag{
				{Key: aws.String(tagRequestID), Value: aws.String("req")},
				{Key: aws.String(tagProviderID), Value: aws.String("aws-prod")},
			},
		}
	}

	fake := &fakeEC2{instances: instances}
	s := &OrphanSweeper{
		ec2: fake,
		listTracked: func(context.Context) (map[string]struct{}, error) {
			return map[string]struct{}{}, nil
		},
		logger:        zap.NewNop(),
		now:           func() time.Time { return now },
		seen:          map[string]orphanSighting{},
		providerID:    "aws-prod",
		grace:         time.Hour,
		maxDeletes:    1,
		sawNonEmpty:   true,
		lastGoodCount: 0,
	}

	// Seed a completed streak so this tick is eligible.
	s.seen = map[string]orphanSighting{
		"i-0": {firstSeen: now.Add(-time.Hour), hits: 5},
		"i-1": {firstSeen: now.Add(-time.Hour), hits: 5},
		"i-2": {firstSeen: now.Add(-time.Hour), hits: 5},
	}

	if err := s.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.terminated) != 1 {
		t.Fatalf("terminated=%v, want exactly one instance", fake.terminated)
	}
}

func TestImplausibleTrackedDrop(t *testing.T) {
	tests := []struct {
		prev, curr int
		want       bool
	}{
		{0, 0, false},
		{1, 0, false},
		{2, 1, false},
		{2, 0, true},
		{10, 6, false},
		{10, 5, false},
		{10, 4, true},
		{10, 10, false},
		{3, 10, false},
		{5, 2, true},
	}
	for _, tt := range tests {
		if got := implausibleTrackedDrop(tt.prev, tt.curr); got != tt.want {
			t.Errorf("implausibleTrackedDrop(%d, %d)=%v, want %v", tt.prev, tt.curr, got, tt.want)
		}
	}
}

func TestSweepWarmupEmptyList(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	orphan := types.Instance{
		InstanceId: aws.String("i-orphan"),
		LaunchTime: aws.Time(now.Add(-3 * time.Hour)),
		State:      &types.InstanceState{Name: types.InstanceStateNameRunning},
		Tags: []types.Tag{
			{Key: aws.String(tagRequestID), Value: aws.String("req-1")},
			{Key: aws.String(tagProviderID), Value: aws.String("aws-prod")},
		},
	}
	s := &OrphanSweeper{
		ec2: &fakeEC2{instances: []types.Instance{orphan}},
		listTracked: func(context.Context) (map[string]struct{}, error) {
			return map[string]struct{}{}, nil
		},
		logger:     zap.NewNop(),
		now:        func() time.Time { return now },
		seen:       map[string]orphanSighting{},
		providerID: "aws-prod",
		grace:      time.Hour,
		maxDeletes: 1,
	}
	if err := s.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.seen) != 0 {
		t.Fatalf("warmup should not start clocks, seen=%v", s.seen)
	}
}

func TestSweepImplausibleDropSkipsDeletes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	orphan := types.Instance{
		InstanceId: aws.String("i-orphan"),
		LaunchTime: aws.Time(now.Add(-3 * time.Hour)),
		State:      &types.InstanceState{Name: types.InstanceStateNameRunning},
		Tags: []types.Tag{
			{Key: aws.String(tagRequestID), Value: aws.String("req-1")},
			{Key: aws.String(tagProviderID), Value: aws.String("aws-prod")},
		},
	}
	fake := &fakeEC2{instances: []types.Instance{orphan}}
	s := &OrphanSweeper{
		ec2: fake,
		listTracked: func(context.Context) (map[string]struct{}, error) {
			return map[string]struct{}{"i-a": {}, "i-b": {}, "i-c": {}}, nil
		},
		logger: zap.NewNop(),
		now:    func() time.Time { return now },
		seen: map[string]orphanSighting{
			"i-orphan": {firstSeen: now.Add(-time.Hour), hits: 5},
		},
		providerID:    "aws-prod",
		grace:         time.Hour,
		maxDeletes:    1,
		sawNonEmpty:   true,
		lastGoodCount: 10,
	}
	if err := s.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.terminated != nil {
		t.Fatalf("terminated after implausible drop: %v", fake.terminated)
	}
	if len(s.seen) != 0 {
		t.Fatalf("sightings should reset, got %v", s.seen)
	}
	if s.lastGoodCount != 10 {
		t.Fatalf("lastGoodCount=%d, want previous good count 10", s.lastGoodCount)
	}
}

func TestSweepGenerationSkip(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	orphan := types.Instance{
		InstanceId: aws.String("i-orphan"),
		LaunchTime: aws.Time(now.Add(-3 * time.Hour)),
		State:      &types.InstanceState{Name: types.InstanceStateNameRunning},
		Tags: []types.Tag{
			{Key: aws.String(tagRequestID), Value: aws.String("req-1")},
			{Key: aws.String(tagProviderID), Value: aws.String("aws-prod")},
		},
	}
	fake := &fakeEC2{instances: []types.Instance{orphan}}
	s := &OrphanSweeper{
		ec2:           fake,
		logger:        zap.NewNop(),
		now:           func() time.Time { return now },
		providerID:    "aws-prod",
		grace:         time.Hour,
		maxDeletes:    1,
		sawNonEmpty:   true,
		lastGoodCount: 0,
		seen: map[string]orphanSighting{
			"i-orphan": {firstSeen: now.Add(-time.Hour), hits: 5},
		},
	}
	calls := 0
	s.listTracked = func(context.Context) (map[string]struct{}, error) {
		calls++
		if calls >= 2 {
			s.Reset("omni requested machine creation")
		}
		return map[string]struct{}{}, nil
	}
	if err := s.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.terminated != nil {
		t.Fatalf("terminated after generation bump: %v", fake.terminated)
	}
}

func TestBackfillProviderTags(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	inst := types.Instance{
		InstanceId: aws.String("i-tracked"),
		LaunchTime: aws.Time(now.Add(-time.Hour)),
		State:      &types.InstanceState{Name: types.InstanceStateNameRunning},
		Tags: []types.Tag{
			{Key: aws.String(tagRequestID), Value: aws.String("req-1")},
		},
	}
	fake := &fakeEC2{instances: []types.Instance{inst}}
	s := &OrphanSweeper{
		ec2: fake,
		listTracked: func(context.Context) (map[string]struct{}, error) {
			return map[string]struct{}{"i-tracked": {}}, nil
		},
		logger:     zap.NewNop(),
		now:        func() time.Time { return now },
		seen:       map[string]orphanSighting{},
		providerID: "aws-prod",
		grace:      time.Hour,
		maxDeletes: 1,
	}
	if err := s.sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.tagged) != 1 || fake.tagged[0] != "i-tracked" {
		t.Fatalf("tagged=%v, want i-tracked", fake.tagged)
	}
}

type fakeEC2 struct {
	instances   []types.Instance
	describeErr error
	terminated  []string
	tagged      []string
}

func (f *fakeEC2) DescribeInstances(_ context.Context, params *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}

	keep := f.instances
	if params != nil && len(params.InstanceIds) > 0 {
		want := map[string]struct{}{}
		for _, id := range params.InstanceIds {
			want[id] = struct{}{}
		}

		filtered := make([]types.Instance, 0, len(keep))
		for _, inst := range keep {
			if _, ok := want[aws.ToString(inst.InstanceId)]; ok {
				filtered = append(filtered, inst)
			}
		}

		keep = filtered
	}

	return &ec2.DescribeInstancesOutput{
		Reservations: []types.Reservation{{Instances: keep}},
	}, nil
}

func (f *fakeEC2) RunInstances(context.Context, *ec2.RunInstancesInput, ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeEC2) CreateTags(_ context.Context, params *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	if params == nil {
		return &ec2.CreateTagsOutput{}, nil
	}
	f.tagged = append(f.tagged, params.Resources...)
	for i := range f.instances {
		id := aws.ToString(f.instances[i].InstanceId)
		for _, resource := range params.Resources {
			if resource != id {
				continue
			}
			f.instances[i].Tags = append(f.instances[i].Tags, params.Tags...)
		}
	}
	return &ec2.CreateTagsOutput{}, nil
}

func (f *fakeEC2) TerminateInstances(_ context.Context, params *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	if params == nil {
		return &ec2.TerminateInstancesOutput{}, nil
	}

	f.terminated = append(f.terminated, params.InstanceIds...)
	drop := map[string]struct{}{}
	for _, id := range params.InstanceIds {
		drop[id] = struct{}{}
	}

	kept := f.instances[:0]
	for _, inst := range f.instances {
		if _, ok := drop[aws.ToString(inst.InstanceId)]; ok {
			continue
		}

		kept = append(kept, inst)
	}
	f.instances = kept

	return &ec2.TerminateInstancesOutput{}, nil
}
