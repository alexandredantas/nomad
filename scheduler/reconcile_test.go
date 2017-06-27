package scheduler

import (
	"fmt"
	"log"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/kr/pretty"
)

/*
Basic Tests:
√  Place when there is nothing in the cluster
√  Place remainder when there is some in the cluster
√  Scale down from n to n-m where n != m
√  Scale down from n to zero
√  Inplace upgrade test
√  Inplace upgrade and scale up test
√  Inplace upgrade and scale down test
√  Destructive upgrade
√  Destructive upgrade and scale up test
√  Destructive upgrade and scale down test
√  Handle lost nodes
√  Handle lost nodes and scale up
√  Handle lost nodes and scale down
√  Handle draining nodes
√  Handle draining nodes and scale up
√  Handle draining nodes and scale down
√  Handle task group being removed
√  Handle job being stopped both as .Stopped and nil
√  Place more that one group

Deployment Tests:
√  Stopped job cancels any active deployment
√  Stopped job doesn't cancel terminal deployment
√  JobIndex change cancels any active deployment
√  JobIndex change doens't cancels any terminal deployment
√  Destructive changes create deployment and get rolled out via max_parallelism
√  Don't create a deployment if there are no changes
√  Deployment created by all inplace updates
√  Paused or failed deployment doesn't create any more canaries
√  Paused or failed deployment doesn't do any placements
√  Paused or failed deployment doesn't do destructive updates
√  Paused does do migrations
√  Failed deployment doesn't do migrations
√  Canary that is on a draining node
√  Canary that is on a lost node
√  Stop old canaries
√  Create new canaries on job change
√  Create new canaries on job change while scaling up
√  Create new canaries on job change while scaling down
√  Fill canaries if partial placement
√  Promote canaries unblocks max_parallel
√  Promote canaries when canaries == count
√  Only place as many as are healthy in deployment
√  Limit calculation accounts for healthy allocs on migrating/lost nodes
√  Failed deployment should not place anything
√  Run after canaries have been promoted, new allocs have been rolled out and there is no deployment
*/

var (
	canaryUpdate = &structs.UpdateStrategy{
		Canary:          2,
		MaxParallel:     2,
		HealthCheck:     structs.UpdateStrategyHealthCheck_Checks,
		MinHealthyTime:  10 * time.Second,
		HealthyDeadline: 10 * time.Minute,
	}

	noCanaryUpdate = &structs.UpdateStrategy{
		MaxParallel:     4,
		HealthCheck:     structs.UpdateStrategyHealthCheck_Checks,
		MinHealthyTime:  10 * time.Second,
		HealthyDeadline: 10 * time.Minute,
	}
)

func testLogger() *log.Logger {
	return log.New(os.Stderr, "", log.LstdFlags)
}

func allocUpdateFnIgnore(*structs.Allocation, *structs.Job, *structs.TaskGroup) (bool, bool, *structs.Allocation) {
	return true, false, nil
}

func allocUpdateFnDestructive(*structs.Allocation, *structs.Job, *structs.TaskGroup) (bool, bool, *structs.Allocation) {
	return false, true, nil
}

func allocUpdateFnInplace(existing *structs.Allocation, _ *structs.Job, newTG *structs.TaskGroup) (bool, bool, *structs.Allocation) {
	// Create a shallow copy
	newAlloc := new(structs.Allocation)
	*newAlloc = *existing
	newAlloc.TaskResources = make(map[string]*structs.Resources)

	// Use the new task resources but keep the network from the old
	for _, task := range newTG.Tasks {
		r := task.Resources.Copy()
		r.Networks = existing.TaskResources[task.Name].Networks
		newAlloc.TaskResources[task.Name] = r
	}

	return false, false, newAlloc
}

func allocUpdateFnMock(handled map[string]allocUpdateType, unhandled allocUpdateType) allocUpdateType {
	return func(existing *structs.Allocation, newJob *structs.Job, newTG *structs.TaskGroup) (bool, bool, *structs.Allocation) {
		if fn, ok := handled[existing.ID]; ok {
			return fn(existing, newJob, newTG)
		}

		return unhandled(existing, newJob, newTG)
	}
}

var (
	// AllocationIndexRegex is a regular expression to find the allocation index.
	allocationIndexRegex = regexp.MustCompile(".+\\[(\\d+)\\]$")
)

// allocNameToIndex returns the index of the allocation.
func allocNameToIndex(name string) uint {
	matches := allocationIndexRegex.FindStringSubmatch(name)
	if len(matches) != 2 {
		return 0
	}

	index, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0
	}

	return uint(index)
}

func assertNamesHaveIndexes(t *testing.T, indexes []int, names []string) {
	m := make(map[uint]int)
	for _, i := range indexes {
		m[uint(i)] += 1
	}

	for _, n := range names {
		index := allocNameToIndex(n)
		val, contained := m[index]
		if !contained {
			t.Fatalf("Unexpected index %d from name %s\nAll names: %v", index, n, names)
		}

		val--
		if val < 0 {
			t.Fatalf("Index %d repeated too many times\nAll names: %v", index, names)
		}
		m[index] = val
	}

	for k, remainder := range m {
		if remainder != 0 {
			t.Fatalf("Index %d has %d remaining uses expected\nAll names: %v", k, remainder, names)
		}
	}
}

func assertNoCanariesStopped(t *testing.T, stop []allocStopResult) {
	for _, s := range stop {
		if s.alloc.Canary {
			t.Fatalf("Stopping canary alloc %q %q", s.alloc.ID, s.alloc.Name)
		}
	}
}

func assertPlaceResultsHavePreviousAllocs(t *testing.T, numPrevious int, place []allocPlaceResult) {
	names := make(map[string]struct{}, numPrevious)

	found := 0
	for _, p := range place {
		if _, ok := names[p.name]; ok {
			t.Fatalf("Name %q already placed", p.name)
		}
		names[p.name] = struct{}{}

		if p.previousAlloc == nil {
			continue
		}

		if act := p.previousAlloc.Name; p.name != act {
			t.Fatalf("Name mismatch on previous alloc; got %q; want %q", act, p.name)
		}
		found++
	}
	if numPrevious != found {
		t.Fatalf("wanted %d; got %d placements with previous allocs", numPrevious, found)
	}
}

func intRange(pairs ...int) []int {
	if len(pairs)%2 != 0 {
		return nil
	}

	var r []int
	for i := 0; i < len(pairs); i += 2 {
		for j := pairs[i]; j <= pairs[i+1]; j++ {
			r = append(r, j)
		}
	}
	return r
}

func placeResultsToNames(place []allocPlaceResult) []string {
	names := make([]string, 0, len(place))
	for _, p := range place {
		names = append(names, p.name)
	}
	return names
}

func stopResultsToNames(stop []allocStopResult) []string {
	names := make([]string, 0, len(stop))
	for _, s := range stop {
		names = append(names, s.alloc.Name)
	}
	return names
}

func allocsToNames(allocs []*structs.Allocation) []string {
	names := make([]string, 0, len(allocs))
	for _, a := range allocs {
		names = append(names, a.Name)
	}
	return names
}

type resultExpectation struct {
	createDeployment  *structs.Deployment
	deploymentUpdates []*structs.DeploymentStatusUpdate
	place             int
	inplace           int
	stop              int
	desiredTGUpdates  map[string]*structs.DesiredUpdates
}

func assertResults(t *testing.T, r *reconcileResults, exp *resultExpectation) {

	if exp.createDeployment != nil && r.createDeployment == nil {
		t.Fatalf("Expect a created deployment got none")
	} else if exp.createDeployment == nil && r.createDeployment != nil {
		t.Fatalf("Expect no created deployment; got %#v", r.createDeployment)
	} else if exp.createDeployment != nil && r.createDeployment != nil {
		// Clear the deployment ID
		r.createDeployment.ID, exp.createDeployment.ID = "", ""
		if !reflect.DeepEqual(r.createDeployment, exp.createDeployment) {
			t.Fatalf("Unexpected createdDeployment; got\n %#v\nwant\n%#v\nDiff: %v",
				r.createDeployment, exp.createDeployment, pretty.Diff(r.createDeployment, exp.createDeployment))
		}
	}

	if !reflect.DeepEqual(r.deploymentUpdates, exp.deploymentUpdates) {
		t.Fatalf("Unexpected deploymentUpdates: %v", pretty.Diff(r.deploymentUpdates, exp.deploymentUpdates))
	}
	if l := len(r.place); l != exp.place {
		t.Fatalf("Expected %d placements; got %d", exp.place, l)
	}
	if l := len(r.inplaceUpdate); l != exp.inplace {
		t.Fatalf("Expected %d inplaceUpdate; got %d", exp.inplace, l)
	}
	if l := len(r.stop); l != exp.stop {
		t.Fatalf("Expected %d stops; got %d", exp.stop, l)
	}
	if l := len(r.desiredTGUpdates); l != len(exp.desiredTGUpdates) {
		t.Fatalf("Expected %d task group desired tg updates annotations; got %d", len(exp.desiredTGUpdates), l)
	}

	// Check the desired updates happened
	for group, desired := range exp.desiredTGUpdates {
		act, ok := r.desiredTGUpdates[group]
		if !ok {
			t.Fatalf("Expected desired updates for group %q", group)
		}

		if !reflect.DeepEqual(act, desired) {
			t.Fatalf("Unexpected annotations for group %q: %v", group, pretty.Diff(act, desired))
		}
	}
}

// Tests the reconciler properly handles placements for a job that has no
// existing allocations
func TestReconciler_Place_NoExisting(t *testing.T) {
	job := mock.Job()
	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, nil, nil, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             10,
		inplace:           0,
		stop:              0,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Place: 10,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 9), placeResultsToNames(r.place))
}

// Tests the reconciler properly handles placements for a job that has some
// existing allocations
func TestReconciler_Place_Existing(t *testing.T) {
	job := mock.Job()

	// Create 3 existing allocations
	var allocs []*structs.Allocation
	for i := 0; i < 5; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             5,
		inplace:           0,
		stop:              0,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Place:  5,
				Ignore: 5,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(5, 9), placeResultsToNames(r.place))
}

// Tests the reconciler properly handles stopping allocations for a job that has
// scaled down
func TestReconciler_ScaleDown_Partial(t *testing.T) {
	// Has desired 10
	job := mock.Job()

	// Create 20 existing allocations
	var allocs []*structs.Allocation
	for i := 0; i < 20; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             0,
		inplace:           0,
		stop:              10,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Ignore: 10,
				Stop:   10,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(10, 19), stopResultsToNames(r.stop))
}

// Tests the reconciler properly handles stopping allocations for a job that has
// scaled down to zero desired
func TestReconciler_ScaleDown_Zero(t *testing.T) {
	// Set desired 0
	job := mock.Job()
	job.TaskGroups[0].Count = 0

	// Create 20 existing allocations
	var allocs []*structs.Allocation
	for i := 0; i < 20; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             0,
		inplace:           0,
		stop:              20,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Stop: 20,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 19), stopResultsToNames(r.stop))
}

// Tests the reconciler properly handles inplace upgrading allocations
func TestReconciler_Inplace(t *testing.T) {
	job := mock.Job()

	// Create 10 existing allocations
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnInplace, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             0,
		inplace:           10,
		stop:              0,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				InPlaceUpdate: 10,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 9), allocsToNames(r.inplaceUpdate))
}

// Tests the reconciler properly handles inplace upgrading allocations while
// scaling up
func TestReconciler_Inplace_ScaleUp(t *testing.T) {
	// Set desired 15
	job := mock.Job()
	job.TaskGroups[0].Count = 15

	// Create 10 existing allocations
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnInplace, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             5,
		inplace:           10,
		stop:              0,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Place:         5,
				InPlaceUpdate: 10,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 9), allocsToNames(r.inplaceUpdate))
	assertNamesHaveIndexes(t, intRange(10, 14), placeResultsToNames(r.place))
}

// Tests the reconciler properly handles inplace upgrading allocations while
// scaling down
func TestReconciler_Inplace_ScaleDown(t *testing.T) {
	// Set desired 5
	job := mock.Job()
	job.TaskGroups[0].Count = 5

	// Create 10 existing allocations
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnInplace, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             0,
		inplace:           5,
		stop:              5,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Stop:          5,
				InPlaceUpdate: 5,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 4), allocsToNames(r.inplaceUpdate))
	assertNamesHaveIndexes(t, intRange(5, 9), stopResultsToNames(r.stop))
}

// Tests the reconciler properly handles destructive upgrading allocations
func TestReconciler_Destructive(t *testing.T) {
	job := mock.Job()

	// Create 10 existing allocations
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnDestructive, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             10,
		inplace:           0,
		stop:              10,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				DestructiveUpdate: 10,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 9), placeResultsToNames(r.place))
	assertNamesHaveIndexes(t, intRange(0, 9), stopResultsToNames(r.stop))
	assertPlaceResultsHavePreviousAllocs(t, 10, r.place)
}

// Tests the reconciler properly handles destructive upgrading allocations while
// scaling up
func TestReconciler_Destructive_ScaleUp(t *testing.T) {
	// Set desired 15
	job := mock.Job()
	job.TaskGroups[0].Count = 15

	// Create 10 existing allocations
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnDestructive, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             15,
		inplace:           0,
		stop:              10,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Place:             5,
				DestructiveUpdate: 10,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 9), stopResultsToNames(r.stop))
	assertNamesHaveIndexes(t, intRange(0, 14), placeResultsToNames(r.place))
	assertPlaceResultsHavePreviousAllocs(t, 10, r.place)
}

// Tests the reconciler properly handles destructive upgrading allocations while
// scaling down
func TestReconciler_Destructive_ScaleDown(t *testing.T) {
	// Set desired 5
	job := mock.Job()
	job.TaskGroups[0].Count = 5

	// Create 10 existing allocations
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnDestructive, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             5,
		inplace:           0,
		stop:              10,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Stop:              5,
				DestructiveUpdate: 5,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 9), stopResultsToNames(r.stop))
	assertNamesHaveIndexes(t, intRange(0, 4), placeResultsToNames(r.place))
	assertPlaceResultsHavePreviousAllocs(t, 5, r.place)
}

// Tests the reconciler properly handles lost nodes with allocations
func TestReconciler_LostNode(t *testing.T) {
	job := mock.Job()

	// Create 10 existing allocations
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	// Build a map of tainted nodes
	tainted := make(map[string]*structs.Node, 2)
	for i := 0; i < 2; i++ {
		n := mock.Node()
		n.ID = allocs[i].NodeID
		n.Status = structs.NodeStatusDown
		tainted[n.ID] = n
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, nil, allocs, tainted)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             2,
		inplace:           0,
		stop:              2,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Place:  2,
				Stop:   2,
				Ignore: 8,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 1), stopResultsToNames(r.stop))
	assertNamesHaveIndexes(t, intRange(0, 1), placeResultsToNames(r.place))
}

// Tests the reconciler properly handles lost nodes with allocations while
// scaling up
func TestReconciler_LostNode_ScaleUp(t *testing.T) {
	// Set desired 15
	job := mock.Job()
	job.TaskGroups[0].Count = 15

	// Create 10 existing allocations
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	// Build a map of tainted nodes
	tainted := make(map[string]*structs.Node, 2)
	for i := 0; i < 2; i++ {
		n := mock.Node()
		n.ID = allocs[i].NodeID
		n.Status = structs.NodeStatusDown
		tainted[n.ID] = n
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, nil, allocs, tainted)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             7,
		inplace:           0,
		stop:              2,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Place:  7,
				Stop:   2,
				Ignore: 8,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 1), stopResultsToNames(r.stop))
	assertNamesHaveIndexes(t, intRange(0, 1, 10, 14), placeResultsToNames(r.place))
}

// Tests the reconciler properly handles lost nodes with allocations while
// scaling down
func TestReconciler_LostNode_ScaleDown(t *testing.T) {
	// Set desired 5
	job := mock.Job()
	job.TaskGroups[0].Count = 5

	// Create 10 existing allocations
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	// Build a map of tainted nodes
	tainted := make(map[string]*structs.Node, 2)
	for i := 0; i < 2; i++ {
		n := mock.Node()
		n.ID = allocs[i].NodeID
		n.Status = structs.NodeStatusDown
		tainted[n.ID] = n
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, nil, allocs, tainted)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             0,
		inplace:           0,
		stop:              5,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Stop:   5,
				Ignore: 5,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 1, 7, 9), stopResultsToNames(r.stop))
}

// Tests the reconciler properly handles draining nodes with allocations
func TestReconciler_DrainNode(t *testing.T) {
	job := mock.Job()

	// Create 10 existing allocations
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	// Build a map of tainted nodes
	tainted := make(map[string]*structs.Node, 2)
	for i := 0; i < 2; i++ {
		n := mock.Node()
		n.ID = allocs[i].NodeID
		n.Drain = true
		tainted[n.ID] = n
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, nil, allocs, tainted)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             2,
		inplace:           0,
		stop:              2,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Migrate: 2,
				Ignore:  8,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 1), stopResultsToNames(r.stop))
	assertNamesHaveIndexes(t, intRange(0, 1), placeResultsToNames(r.place))
	assertPlaceResultsHavePreviousAllocs(t, 2, r.place)
}

// Tests the reconciler properly handles draining nodes with allocations while
// scaling up
func TestReconciler_DrainNode_ScaleUp(t *testing.T) {
	// Set desired 15
	job := mock.Job()
	job.TaskGroups[0].Count = 15

	// Create 10 existing allocations
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	// Build a map of tainted nodes
	tainted := make(map[string]*structs.Node, 2)
	for i := 0; i < 2; i++ {
		n := mock.Node()
		n.ID = allocs[i].NodeID
		n.Drain = true
		tainted[n.ID] = n
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, nil, allocs, tainted)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             7,
		inplace:           0,
		stop:              2,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Place:   5,
				Migrate: 2,
				Ignore:  8,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 1), stopResultsToNames(r.stop))
	assertNamesHaveIndexes(t, intRange(0, 1, 10, 14), placeResultsToNames(r.place))
	assertPlaceResultsHavePreviousAllocs(t, 2, r.place)
}

// Tests the reconciler properly handles draining nodes with allocations while
// scaling down
func TestReconciler_DrainNode_ScaleDown(t *testing.T) {
	// Set desired 8
	job := mock.Job()
	job.TaskGroups[0].Count = 8

	// Create 10 existing allocations
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	// Build a map of tainted nodes
	tainted := make(map[string]*structs.Node, 3)
	for i := 0; i < 3; i++ {
		n := mock.Node()
		n.ID = allocs[i].NodeID
		n.Drain = true
		tainted[n.ID] = n
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, nil, allocs, tainted)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             1,
		inplace:           0,
		stop:              3,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Migrate: 1,
				Stop:    2,
				Ignore:  7,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 2), stopResultsToNames(r.stop))
	assertNamesHaveIndexes(t, intRange(0, 0), placeResultsToNames(r.place))
	assertPlaceResultsHavePreviousAllocs(t, 1, r.place)
}

// Tests the reconciler properly handles a task group being removed
func TestReconciler_RemovedTG(t *testing.T) {
	job := mock.Job()

	// Create 10 allocations for a tg that no longer exists
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	oldName := job.TaskGroups[0].Name
	newName := "different"
	job.TaskGroups[0].Name = newName

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             10,
		inplace:           0,
		stop:              10,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			oldName: {
				Stop: 10,
			},
			newName: {
				Place: 10,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 9), stopResultsToNames(r.stop))
	assertNamesHaveIndexes(t, intRange(0, 9), placeResultsToNames(r.place))
}

// Tests the reconciler properly handles a job in stopped states
func TestReconciler_JobStopped(t *testing.T) {
	job := mock.Job()
	job.Stop = true

	cases := []struct {
		name             string
		job              *structs.Job
		jobID, taskGroup string
	}{
		{
			name:      "stopped job",
			job:       job,
			jobID:     job.ID,
			taskGroup: job.TaskGroups[0].Name,
		},
		{
			name:      "nil job",
			job:       nil,
			jobID:     "foo",
			taskGroup: "bar",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Create 10 allocations
			var allocs []*structs.Allocation
			for i := 0; i < 10; i++ {
				alloc := mock.Alloc()
				alloc.Job = c.job
				alloc.JobID = c.jobID
				alloc.NodeID = structs.GenerateUUID()
				alloc.Name = structs.AllocName(c.jobID, c.taskGroup, uint(i))
				alloc.TaskGroup = c.taskGroup
				allocs = append(allocs, alloc)
			}

			reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, c.jobID, c.job, nil, allocs, nil)
			r := reconciler.Compute()

			// Assert the correct results
			assertResults(t, r, &resultExpectation{
				createDeployment:  nil,
				deploymentUpdates: nil,
				place:             0,
				inplace:           0,
				stop:              10,
				desiredTGUpdates: map[string]*structs.DesiredUpdates{
					c.taskGroup: {
						Stop: 10,
					},
				},
			})

			assertNamesHaveIndexes(t, intRange(0, 9), stopResultsToNames(r.stop))
		})
	}
}

// Tests the reconciler properly handles jobs with multiple task groups
func TestReconciler_MultiTG(t *testing.T) {
	job := mock.Job()
	tg2 := job.TaskGroups[0].Copy()
	tg2.Name = "foo"
	job.TaskGroups = append(job.TaskGroups, tg2)

	// Create 2 existing allocations for the first tg
	var allocs []*structs.Allocation
	for i := 0; i < 2; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             18,
		inplace:           0,
		stop:              0,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Place:  8,
				Ignore: 2,
			},
			tg2.Name: {
				Place: 10,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(2, 9, 0, 9), placeResultsToNames(r.place))
}

// Tests the reconciler cancels an old deployment when the job is being stopped
func TestReconciler_CancelDeployment_JobStop(t *testing.T) {
	job := mock.Job()
	job.Stop = true

	running := structs.NewDeployment(job)
	failed := structs.NewDeployment(job)
	failed.Status = structs.DeploymentStatusFailed

	cases := []struct {
		name             string
		job              *structs.Job
		jobID, taskGroup string
		deployment       *structs.Deployment
		cancel           bool
	}{
		{
			name:       "stopped job, running deployment",
			job:        job,
			jobID:      job.ID,
			taskGroup:  job.TaskGroups[0].Name,
			deployment: running,
			cancel:     true,
		},
		{
			name:       "nil job, running deployment",
			job:        nil,
			jobID:      "foo",
			taskGroup:  "bar",
			deployment: running,
			cancel:     true,
		},
		{
			name:       "stopped job, failed deployment",
			job:        job,
			jobID:      job.ID,
			taskGroup:  job.TaskGroups[0].Name,
			deployment: failed,
			cancel:     false,
		},
		{
			name:       "nil job, failed deployment",
			job:        nil,
			jobID:      "foo",
			taskGroup:  "bar",
			deployment: failed,
			cancel:     false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Create 10 allocations
			var allocs []*structs.Allocation
			for i := 0; i < 10; i++ {
				alloc := mock.Alloc()
				alloc.Job = c.job
				alloc.JobID = c.jobID
				alloc.NodeID = structs.GenerateUUID()
				alloc.Name = structs.AllocName(c.jobID, c.taskGroup, uint(i))
				alloc.TaskGroup = c.taskGroup
				allocs = append(allocs, alloc)
			}

			reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, c.jobID, c.job, c.deployment, allocs, nil)
			r := reconciler.Compute()

			var updates []*structs.DeploymentStatusUpdate
			if c.cancel {
				updates = []*structs.DeploymentStatusUpdate{
					{
						DeploymentID:      c.deployment.ID,
						Status:            structs.DeploymentStatusCancelled,
						StatusDescription: structs.DeploymentStatusDescriptionStoppedJob,
					},
				}
			}

			// Assert the correct results
			assertResults(t, r, &resultExpectation{
				createDeployment:  nil,
				deploymentUpdates: updates,
				place:             0,
				inplace:           0,
				stop:              10,
				desiredTGUpdates: map[string]*structs.DesiredUpdates{
					c.taskGroup: {
						Stop: 10,
					},
				},
			})

			assertNamesHaveIndexes(t, intRange(0, 9), stopResultsToNames(r.stop))
		})
	}
}

// Tests the reconciler cancels an old deployment when the job is updated
func TestReconciler_CancelDeployment_JobUpdate(t *testing.T) {
	// Create a base job
	job := mock.Job()

	// Create two deployments
	running := structs.NewDeployment(job)
	failed := structs.NewDeployment(job)
	failed.Status = structs.DeploymentStatusFailed

	// Make the job newer than the deployment
	job.JobModifyIndex += 10

	cases := []struct {
		name       string
		deployment *structs.Deployment
		cancel     bool
	}{
		{
			name:       "running deployment",
			deployment: running,
			cancel:     true,
		},
		{
			name:       "failed deployment",
			deployment: failed,
			cancel:     false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Create 10 allocations
			var allocs []*structs.Allocation
			for i := 0; i < 10; i++ {
				alloc := mock.Alloc()
				alloc.Job = job
				alloc.JobID = job.ID
				alloc.NodeID = structs.GenerateUUID()
				alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
				alloc.TaskGroup = job.TaskGroups[0].Name
				allocs = append(allocs, alloc)
			}

			reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, c.deployment, allocs, nil)
			r := reconciler.Compute()

			var updates []*structs.DeploymentStatusUpdate
			if c.cancel {
				updates = []*structs.DeploymentStatusUpdate{
					{
						DeploymentID:      c.deployment.ID,
						Status:            structs.DeploymentStatusCancelled,
						StatusDescription: structs.DeploymentStatusDescriptionNewerJob,
					},
				}
			}

			// Assert the correct results
			assertResults(t, r, &resultExpectation{
				createDeployment:  nil,
				deploymentUpdates: updates,
				place:             0,
				inplace:           0,
				stop:              0,
				desiredTGUpdates: map[string]*structs.DesiredUpdates{
					job.TaskGroups[0].Name: {
						Ignore: 10,
					},
				},
			})
		})
	}
}

// Tests the reconciler creates a deployment and does a rolling upgrade with
// destructive changes
func TestReconciler_CreateDeployment_RollingUpgrade_Destructive(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = noCanaryUpdate

	// Create 10 allocations from the old job
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		alloc.TaskGroup = job.TaskGroups[0].Name
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnDestructive, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	d := structs.NewDeployment(job)
	d.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
		DesiredTotal: 10,
	}

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  d,
		deploymentUpdates: nil,
		place:             4,
		inplace:           0,
		stop:              4,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				DestructiveUpdate: 4,
				Ignore:            6,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 3), placeResultsToNames(r.place))
	assertNamesHaveIndexes(t, intRange(0, 3), stopResultsToNames(r.stop))
}

// Tests the reconciler creates a deployment for inplace updates
func TestReconciler_CreateDeployment_RollingUpgrade_Inplace(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = noCanaryUpdate

	// Create 10 allocations from the old job
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		alloc.TaskGroup = job.TaskGroups[0].Name
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnInplace, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	d := structs.NewDeployment(job)
	d.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
		DesiredTotal: 10,
	}

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  d,
		deploymentUpdates: nil,
		place:             0,
		inplace:           10,
		stop:              0,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				InPlaceUpdate: 10,
			},
		},
	})
}

// Tests the reconciler doesn't creates a deployment if there are no changes
func TestReconciler_DontCreateDeployment_NoChanges(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = noCanaryUpdate

	// Create 10 allocations from the job
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		alloc.TaskGroup = job.TaskGroups[0].Name
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             0,
		inplace:           0,
		stop:              0,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				DestructiveUpdate: 0,
				Ignore:            10,
			},
		},
	})
}

// Tests the reconciler doesn't place any more canaries when the deployment is
// paused or failed
func TestReconciler_PausedOrFailedDeployment_NoMoreCanaries(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = canaryUpdate

	cases := []struct {
		name             string
		deploymentStatus string
	}{
		{
			name:             "paused deployment",
			deploymentStatus: structs.DeploymentStatusPaused,
		},
		{
			name:             "failed deployment",
			deploymentStatus: structs.DeploymentStatusFailed,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Create a deployment that is paused and has placed some canaries
			d := structs.NewDeployment(job)
			d.Status = c.deploymentStatus
			d.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
				Promoted:        false,
				DesiredCanaries: 2,
				DesiredTotal:    10,
				PlacedAllocs:    1,
			}

			// Create 10 allocations for the original job
			var allocs []*structs.Allocation
			for i := 0; i < 10; i++ {
				alloc := mock.Alloc()
				alloc.Job = job
				alloc.JobID = job.ID
				alloc.NodeID = structs.GenerateUUID()
				alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
				alloc.TaskGroup = job.TaskGroups[0].Name
				allocs = append(allocs, alloc)
			}

			// Create one canary
			canary := mock.Alloc()
			canary.Job = job
			canary.JobID = job.ID
			canary.NodeID = structs.GenerateUUID()
			canary.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, 0)
			canary.TaskGroup = job.TaskGroups[0].Name
			canary.Canary = true
			canary.DeploymentID = d.ID
			allocs = append(allocs, canary)

			mockUpdateFn := allocUpdateFnMock(map[string]allocUpdateType{canary.ID: allocUpdateFnIgnore}, allocUpdateFnDestructive)
			reconciler := NewAllocReconciler(testLogger(), mockUpdateFn, false, job.ID, job, d, allocs, nil)
			r := reconciler.Compute()

			// Assert the correct results
			assertResults(t, r, &resultExpectation{
				createDeployment:  nil,
				deploymentUpdates: nil,
				place:             0,
				inplace:           0,
				stop:              0,
				desiredTGUpdates: map[string]*structs.DesiredUpdates{
					job.TaskGroups[0].Name: {
						Ignore: 11,
					},
				},
			})
		})
	}
}

// Tests the reconciler doesn't place any more allocs when the deployment is
// paused or failed
func TestReconciler_PausedOrFailedDeployment_NoMorePlacements(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = noCanaryUpdate
	job.TaskGroups[0].Count = 15

	cases := []struct {
		name             string
		deploymentStatus string
	}{
		{
			name:             "paused deployment",
			deploymentStatus: structs.DeploymentStatusPaused,
		},
		{
			name:             "failed deployment",
			deploymentStatus: structs.DeploymentStatusFailed,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Create a deployment that is paused and has placed some canaries
			d := structs.NewDeployment(job)
			d.Status = c.deploymentStatus
			d.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
				Promoted:     false,
				DesiredTotal: 15,
				PlacedAllocs: 10,
			}

			// Create 10 allocations for the new job
			var allocs []*structs.Allocation
			for i := 0; i < 10; i++ {
				alloc := mock.Alloc()
				alloc.Job = job
				alloc.JobID = job.ID
				alloc.NodeID = structs.GenerateUUID()
				alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
				alloc.TaskGroup = job.TaskGroups[0].Name
				allocs = append(allocs, alloc)
			}

			reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, d, allocs, nil)
			r := reconciler.Compute()

			// Assert the correct results
			assertResults(t, r, &resultExpectation{
				createDeployment:  nil,
				deploymentUpdates: nil,
				place:             0,
				inplace:           0,
				stop:              0,
				desiredTGUpdates: map[string]*structs.DesiredUpdates{
					job.TaskGroups[0].Name: {
						Ignore: 10,
					},
				},
			})
		})
	}
}

// Tests the reconciler doesn't do any more destructive updates when the
// deployment is paused or failed
func TestReconciler_PausedOrFailedDeployment_NoMoreDestructiveUpdates(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = noCanaryUpdate

	cases := []struct {
		name             string
		deploymentStatus string
	}{
		{
			name:             "paused deployment",
			deploymentStatus: structs.DeploymentStatusPaused,
		},
		{
			name:             "failed deployment",
			deploymentStatus: structs.DeploymentStatusFailed,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Create a deployment that is paused and has placed some canaries
			d := structs.NewDeployment(job)
			d.Status = c.deploymentStatus
			d.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
				Promoted:     false,
				DesiredTotal: 10,
				PlacedAllocs: 1,
			}

			// Create 9 allocations for the original job
			var allocs []*structs.Allocation
			for i := 1; i < 10; i++ {
				alloc := mock.Alloc()
				alloc.Job = job
				alloc.JobID = job.ID
				alloc.NodeID = structs.GenerateUUID()
				alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
				alloc.TaskGroup = job.TaskGroups[0].Name
				allocs = append(allocs, alloc)
			}

			// Create one for the new job
			newAlloc := mock.Alloc()
			newAlloc.Job = job
			newAlloc.JobID = job.ID
			newAlloc.NodeID = structs.GenerateUUID()
			newAlloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, 0)
			newAlloc.TaskGroup = job.TaskGroups[0].Name
			newAlloc.DeploymentID = d.ID
			allocs = append(allocs, newAlloc)

			mockUpdateFn := allocUpdateFnMock(map[string]allocUpdateType{newAlloc.ID: allocUpdateFnIgnore}, allocUpdateFnDestructive)
			reconciler := NewAllocReconciler(testLogger(), mockUpdateFn, false, job.ID, job, d, allocs, nil)
			r := reconciler.Compute()

			// Assert the correct results
			assertResults(t, r, &resultExpectation{
				createDeployment:  nil,
				deploymentUpdates: nil,
				place:             0,
				inplace:           0,
				stop:              0,
				desiredTGUpdates: map[string]*structs.DesiredUpdates{
					job.TaskGroups[0].Name: {
						Ignore: 10,
					},
				},
			})
		})
	}
}

// Tests the reconciler handles migrations correctly when a deployment is paused
// or failed
func TestReconciler_PausedOrFailedDeployment_Migrations(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = noCanaryUpdate

	cases := []struct {
		name              string
		deploymentStatus  string
		place             int
		stop              int
		ignoreAnnotation  uint64
		migrateAnnotation uint64
		stopAnnotation    uint64
	}{
		{
			name:              "paused deployment",
			deploymentStatus:  structs.DeploymentStatusPaused,
			place:             3,
			stop:              3,
			ignoreAnnotation:  5,
			migrateAnnotation: 3,
		},
		{
			name:              "failed deployment",
			deploymentStatus:  structs.DeploymentStatusFailed,
			place:             0,
			stop:              3,
			ignoreAnnotation:  5,
			migrateAnnotation: 0,
			stopAnnotation:    3,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Create a deployment that is paused and has placed some canaries
			d := structs.NewDeployment(job)
			d.Status = c.deploymentStatus
			d.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
				Promoted:     false,
				DesiredTotal: 10,
				PlacedAllocs: 8,
			}

			// Create 8 allocations in the deployment
			var allocs []*structs.Allocation
			for i := 0; i < 8; i++ {
				alloc := mock.Alloc()
				alloc.Job = job
				alloc.JobID = job.ID
				alloc.NodeID = structs.GenerateUUID()
				alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
				alloc.TaskGroup = job.TaskGroups[0].Name
				alloc.DeploymentID = d.ID
				allocs = append(allocs, alloc)
			}

			// Build a map of tainted nodes
			tainted := make(map[string]*structs.Node, 3)
			for i := 0; i < 3; i++ {
				n := mock.Node()
				n.ID = allocs[i].NodeID
				n.Drain = true
				tainted[n.ID] = n
			}

			reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, d, allocs, tainted)
			r := reconciler.Compute()

			// Assert the correct results
			assertResults(t, r, &resultExpectation{
				createDeployment:  nil,
				deploymentUpdates: nil,
				place:             c.place,
				inplace:           0,
				stop:              c.stop,
				desiredTGUpdates: map[string]*structs.DesiredUpdates{
					job.TaskGroups[0].Name: {
						Migrate: c.migrateAnnotation,
						Ignore:  c.ignoreAnnotation,
						Stop:    c.stopAnnotation,
					},
				},
			})
		})
	}
}

// Tests the reconciler handles migrating a canary correctly on a draining node
func TestReconciler_DrainNode_Canary(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = canaryUpdate

	// Create a deployment that is paused and has placed some canaries
	d := structs.NewDeployment(job)
	d.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
		Promoted:        false,
		DesiredTotal:    10,
		DesiredCanaries: 2,
		PlacedAllocs:    2,
	}

	// Create 10 allocations from the old job
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		alloc.TaskGroup = job.TaskGroups[0].Name
		allocs = append(allocs, alloc)
	}

	// Create two canaries for the new job
	handled := make(map[string]allocUpdateType)
	for i := 0; i < 2; i++ {
		// Create one canary
		canary := mock.Alloc()
		canary.Job = job
		canary.JobID = job.ID
		canary.NodeID = structs.GenerateUUID()
		canary.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		canary.TaskGroup = job.TaskGroups[0].Name
		canary.Canary = true
		canary.DeploymentID = d.ID
		allocs = append(allocs, canary)
		handled[canary.ID] = allocUpdateFnIgnore
	}

	// Build a map of tainted nodes that contains the last canary
	tainted := make(map[string]*structs.Node, 1)
	n := mock.Node()
	n.ID = allocs[11].NodeID
	n.Drain = true
	tainted[n.ID] = n

	mockUpdateFn := allocUpdateFnMock(handled, allocUpdateFnDestructive)
	reconciler := NewAllocReconciler(testLogger(), mockUpdateFn, false, job.ID, job, d, allocs, tainted)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             1,
		inplace:           0,
		stop:              1,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Canary: 1,
				Ignore: 11,
			},
		},
	})
	assertNamesHaveIndexes(t, intRange(1, 1), stopResultsToNames(r.stop))
	assertNamesHaveIndexes(t, intRange(1, 1), placeResultsToNames(r.place))
}

// Tests the reconciler handles migrating a canary correctly on a lost node
func TestReconciler_LostNode_Canary(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = canaryUpdate

	// Create a deployment that is paused and has placed some canaries
	d := structs.NewDeployment(job)
	d.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
		Promoted:        false,
		DesiredTotal:    10,
		DesiredCanaries: 2,
		PlacedAllocs:    2,
	}

	// Create 10 allocations from the old job
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		alloc.TaskGroup = job.TaskGroups[0].Name
		allocs = append(allocs, alloc)
	}

	// Create two canaries for the new job
	handled := make(map[string]allocUpdateType)
	for i := 0; i < 2; i++ {
		// Create one canary
		canary := mock.Alloc()
		canary.Job = job
		canary.JobID = job.ID
		canary.NodeID = structs.GenerateUUID()
		canary.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		canary.TaskGroup = job.TaskGroups[0].Name
		canary.Canary = true
		canary.DeploymentID = d.ID
		allocs = append(allocs, canary)
		handled[canary.ID] = allocUpdateFnIgnore
	}

	// Build a map of tainted nodes that contains the last canary
	tainted := make(map[string]*structs.Node, 1)
	n := mock.Node()
	n.ID = allocs[11].NodeID
	n.Status = structs.NodeStatusDown
	tainted[n.ID] = n

	mockUpdateFn := allocUpdateFnMock(handled, allocUpdateFnDestructive)
	reconciler := NewAllocReconciler(testLogger(), mockUpdateFn, false, job.ID, job, d, allocs, tainted)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             1,
		inplace:           0,
		stop:              1,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Canary: 1,
				Ignore: 11,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(1, 1), stopResultsToNames(r.stop))
	assertNamesHaveIndexes(t, intRange(1, 1), placeResultsToNames(r.place))
}

// Tests the reconciler handles stopping canaries from older deployments
func TestReconciler_StopOldCanaries(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = canaryUpdate

	// Create an old deployment that has placed some canaries
	d := structs.NewDeployment(job)
	d.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
		Promoted:        false,
		DesiredTotal:    10,
		DesiredCanaries: 2,
		PlacedAllocs:    2,
	}

	// Update the job
	job.JobModifyIndex += 10

	// Create 10 allocations from the old job
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		alloc.TaskGroup = job.TaskGroups[0].Name
		allocs = append(allocs, alloc)
	}

	// Create canaries
	for i := 0; i < 2; i++ {
		// Create one canary
		canary := mock.Alloc()
		canary.Job = job
		canary.JobID = job.ID
		canary.NodeID = structs.GenerateUUID()
		canary.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		canary.TaskGroup = job.TaskGroups[0].Name
		canary.Canary = true
		canary.DeploymentID = d.ID
		allocs = append(allocs, canary)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnDestructive, false, job.ID, job, d, allocs, nil)
	r := reconciler.Compute()

	newD := structs.NewDeployment(job)
	newD.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
		DesiredCanaries: 2,
		DesiredTotal:    10,
	}

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment: newD,
		deploymentUpdates: []*structs.DeploymentStatusUpdate{
			{
				DeploymentID:      d.ID,
				Status:            structs.DeploymentStatusCancelled,
				StatusDescription: structs.DeploymentStatusDescriptionNewerJob,
			},
		},
		place:   2,
		inplace: 0,
		stop:    2,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Canary: 2,
				Stop:   2,
				Ignore: 10,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 1), stopResultsToNames(r.stop))
	assertNamesHaveIndexes(t, intRange(0, 1), placeResultsToNames(r.place))
}

// Tests the reconciler creates new canaries when the job changes
func TestReconciler_NewCanaries(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = canaryUpdate

	// Create 10 allocations from the old job
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		alloc.TaskGroup = job.TaskGroups[0].Name
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnDestructive, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	newD := structs.NewDeployment(job)
	newD.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
		DesiredCanaries: 2,
		DesiredTotal:    10,
	}

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  newD,
		deploymentUpdates: nil,
		place:             2,
		inplace:           0,
		stop:              0,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Canary: 2,
				Ignore: 10,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 1), placeResultsToNames(r.place))
}

// Tests the reconciler creates new canaries when the job changes and scales up
func TestReconciler_NewCanaries_ScaleUp(t *testing.T) {
	// Scale the job up to 15
	job := mock.Job()
	job.TaskGroups[0].Update = canaryUpdate
	job.TaskGroups[0].Count = 15

	// Create 10 allocations from the old job
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		alloc.TaskGroup = job.TaskGroups[0].Name
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnDestructive, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	newD := structs.NewDeployment(job)
	newD.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
		DesiredCanaries: 2,
		DesiredTotal:    15,
	}

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  newD,
		deploymentUpdates: nil,
		place:             2,
		inplace:           0,
		stop:              0,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Canary: 2,
				Ignore: 10,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 1), placeResultsToNames(r.place))
}

// Tests the reconciler creates new canaries when the job changes and scales
// down
func TestReconciler_NewCanaries_ScaleDown(t *testing.T) {
	// Scale the job down to 5
	job := mock.Job()
	job.TaskGroups[0].Update = canaryUpdate
	job.TaskGroups[0].Count = 5

	// Create 10 allocations from the old job
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		alloc.TaskGroup = job.TaskGroups[0].Name
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnDestructive, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	newD := structs.NewDeployment(job)
	newD.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
		DesiredCanaries: 2,
		DesiredTotal:    5,
	}

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  newD,
		deploymentUpdates: nil,
		place:             2,
		inplace:           0,
		stop:              5,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Canary: 2,
				Stop:   5,
				Ignore: 5,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 1), placeResultsToNames(r.place))
	assertNamesHaveIndexes(t, intRange(5, 9), stopResultsToNames(r.stop))
}

// Tests the reconciler handles filling the names of partially placed canaries
func TestReconciler_NewCanaries_FillNames(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = &structs.UpdateStrategy{
		Canary:          4,
		MaxParallel:     2,
		HealthCheck:     structs.UpdateStrategyHealthCheck_Checks,
		MinHealthyTime:  10 * time.Second,
		HealthyDeadline: 10 * time.Minute,
	}

	// Create an existing deployment that has placed some canaries
	d := structs.NewDeployment(job)
	d.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
		Promoted:        false,
		DesiredTotal:    10,
		DesiredCanaries: 4,
		PlacedAllocs:    2,
	}

	// Create 10 allocations from the old job
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		alloc.TaskGroup = job.TaskGroups[0].Name
		allocs = append(allocs, alloc)
	}

	// Create canaries but pick names at the ends
	for i := 0; i < 4; i += 3 {
		// Create one canary
		canary := mock.Alloc()
		canary.Job = job
		canary.JobID = job.ID
		canary.NodeID = structs.GenerateUUID()
		canary.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		canary.TaskGroup = job.TaskGroups[0].Name
		canary.Canary = true
		canary.DeploymentID = d.ID
		allocs = append(allocs, canary)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnDestructive, false, job.ID, job, d, allocs, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             2,
		inplace:           0,
		stop:              0,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Canary: 2,
				Ignore: 12,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(1, 2), placeResultsToNames(r.place))
}

// Tests the reconciler handles canary promotion by unblocking max_parallel
func TestReconciler_PromoteCanaries_Unblock(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = canaryUpdate

	// Create an existing deployment that has placed some canaries and mark them
	// promoted
	d := structs.NewDeployment(job)
	d.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
		Promoted:        true,
		DesiredTotal:    10,
		DesiredCanaries: 2,
		PlacedAllocs:    2,
	}

	// Create 10 allocations from the old job
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		alloc.TaskGroup = job.TaskGroups[0].Name
		allocs = append(allocs, alloc)
	}

	// Create the canaries
	handled := make(map[string]allocUpdateType)
	for i := 0; i < 2; i++ {
		// Create one canary
		canary := mock.Alloc()
		canary.Job = job
		canary.JobID = job.ID
		canary.NodeID = structs.GenerateUUID()
		canary.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		canary.TaskGroup = job.TaskGroups[0].Name
		canary.Canary = true
		canary.DeploymentID = d.ID
		canary.DeploymentStatus = &structs.AllocDeploymentStatus{
			Healthy: helper.BoolToPtr(true),
		}
		allocs = append(allocs, canary)
		handled[canary.ID] = allocUpdateFnIgnore
	}

	mockUpdateFn := allocUpdateFnMock(handled, allocUpdateFnDestructive)
	reconciler := NewAllocReconciler(testLogger(), mockUpdateFn, false, job.ID, job, d, allocs, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             2,
		inplace:           0,
		stop:              4,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Stop:              2,
				DestructiveUpdate: 2,
				Ignore:            8,
			},
		},
	})

	assertNoCanariesStopped(t, r.stop)
	assertNamesHaveIndexes(t, intRange(2, 3), placeResultsToNames(r.place))
	assertNamesHaveIndexes(t, intRange(0, 3), stopResultsToNames(r.stop))
}

// Tests the reconciler handles canary promotion when the canary count equals
// the total correctly
func TestReconciler_PromoteCanaries_CanariesEqualCount(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = canaryUpdate
	job.TaskGroups[0].Count = 2

	// Create an existing deployment that has placed some canaries and mark them
	// promoted
	d := structs.NewDeployment(job)
	d.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
		Promoted:        true,
		DesiredTotal:    2,
		DesiredCanaries: 2,
		PlacedAllocs:    2,
	}

	// Create 2 allocations from the old job
	var allocs []*structs.Allocation
	for i := 0; i < 2; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		alloc.TaskGroup = job.TaskGroups[0].Name
		allocs = append(allocs, alloc)
	}

	// Create the canaries
	handled := make(map[string]allocUpdateType)
	for i := 0; i < 2; i++ {
		// Create one canary
		canary := mock.Alloc()
		canary.Job = job
		canary.JobID = job.ID
		canary.NodeID = structs.GenerateUUID()
		canary.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		canary.TaskGroup = job.TaskGroups[0].Name
		canary.Canary = true
		canary.DeploymentID = d.ID
		canary.DeploymentStatus = &structs.AllocDeploymentStatus{
			Healthy: helper.BoolToPtr(true),
		}
		allocs = append(allocs, canary)
		handled[canary.ID] = allocUpdateFnIgnore
	}

	mockUpdateFn := allocUpdateFnMock(handled, allocUpdateFnDestructive)
	reconciler := NewAllocReconciler(testLogger(), mockUpdateFn, false, job.ID, job, d, allocs, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             0,
		inplace:           0,
		stop:              2,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Stop:   2,
				Ignore: 2,
			},
		},
	})

	assertNoCanariesStopped(t, r.stop)
	assertNamesHaveIndexes(t, intRange(0, 1), stopResultsToNames(r.stop))
}

// Tests the reconciler checks the health of placed allocs to determine the
// limit
func TestReconciler_DeploymentLimit_HealthAccounting(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = noCanaryUpdate

	cases := []struct {
		healthy int
	}{
		{
			healthy: 0,
		},
		{
			healthy: 1,
		},
		{
			healthy: 2,
		},
		{
			healthy: 3,
		},
		{
			healthy: 4,
		},
	}

	for _, c := range cases {
		t.Run(fmt.Sprintf("%d healthy", c.healthy), func(t *testing.T) {
			// Create an existing deployment that has placed some canaries and mark them
			// promoted
			d := structs.NewDeployment(job)
			d.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
				Promoted:     true,
				DesiredTotal: 10,
				PlacedAllocs: 4,
			}

			// Create 6 allocations from the old job
			var allocs []*structs.Allocation
			for i := 4; i < 10; i++ {
				alloc := mock.Alloc()
				alloc.Job = job
				alloc.JobID = job.ID
				alloc.NodeID = structs.GenerateUUID()
				alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
				alloc.TaskGroup = job.TaskGroups[0].Name
				allocs = append(allocs, alloc)
			}

			// Create the new allocs
			handled := make(map[string]allocUpdateType)
			for i := 0; i < 4; i++ {
				new := mock.Alloc()
				new.Job = job
				new.JobID = job.ID
				new.NodeID = structs.GenerateUUID()
				new.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
				new.TaskGroup = job.TaskGroups[0].Name
				new.DeploymentID = d.ID
				if i < c.healthy {
					new.DeploymentStatus = &structs.AllocDeploymentStatus{
						Healthy: helper.BoolToPtr(true),
					}
				}
				allocs = append(allocs, new)
				handled[new.ID] = allocUpdateFnIgnore
			}

			mockUpdateFn := allocUpdateFnMock(handled, allocUpdateFnDestructive)
			reconciler := NewAllocReconciler(testLogger(), mockUpdateFn, false, job.ID, job, d, allocs, nil)
			r := reconciler.Compute()

			// Assert the correct results
			assertResults(t, r, &resultExpectation{
				createDeployment:  nil,
				deploymentUpdates: nil,
				place:             c.healthy,
				inplace:           0,
				stop:              c.healthy,
				desiredTGUpdates: map[string]*structs.DesiredUpdates{
					job.TaskGroups[0].Name: {
						DestructiveUpdate: uint64(c.healthy),
						Ignore:            uint64(10 - c.healthy),
					},
				},
			})

			if c.healthy != 0 {
				assertNamesHaveIndexes(t, intRange(4, 3+c.healthy), placeResultsToNames(r.place))
				assertNamesHaveIndexes(t, intRange(4, 3+c.healthy), stopResultsToNames(r.stop))
			}
		})
	}
}

// Tests the reconciler handles an alloc on a tainted node during a rolling
// update
func TestReconciler_TaintedNode_RollingUpgrade(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = noCanaryUpdate

	// Create an existing deployment that has some placed allocs
	d := structs.NewDeployment(job)
	d.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
		Promoted:     true,
		DesiredTotal: 10,
		PlacedAllocs: 4,
	}

	// Create 6 allocations from the old job
	var allocs []*structs.Allocation
	for i := 4; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		alloc.TaskGroup = job.TaskGroups[0].Name
		allocs = append(allocs, alloc)
	}

	// Create the healthy replacements
	handled := make(map[string]allocUpdateType)
	for i := 0; i < 4; i++ {
		new := mock.Alloc()
		new.Job = job
		new.JobID = job.ID
		new.NodeID = structs.GenerateUUID()
		new.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		new.TaskGroup = job.TaskGroups[0].Name
		new.DeploymentID = d.ID
		new.DeploymentStatus = &structs.AllocDeploymentStatus{
			Healthy: helper.BoolToPtr(true),
		}
		allocs = append(allocs, new)
		handled[new.ID] = allocUpdateFnIgnore
	}

	// Build a map of tainted nodes
	tainted := make(map[string]*structs.Node, 2)
	for i := 0; i < 2; i++ {
		n := mock.Node()
		n.ID = allocs[6+i].NodeID
		if i == 0 {
			n.Status = structs.NodeStatusDown
		} else {
			n.Drain = true
		}
		tainted[n.ID] = n
	}

	mockUpdateFn := allocUpdateFnMock(handled, allocUpdateFnDestructive)
	reconciler := NewAllocReconciler(testLogger(), mockUpdateFn, false, job.ID, job, d, allocs, tainted)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             6,
		inplace:           0,
		stop:              6,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Place:             1, // Place the lost
				Stop:              1, // Stop the lost
				Migrate:           1, // Migrate the tainted
				DestructiveUpdate: 4,
				Ignore:            4,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 1, 4, 7), placeResultsToNames(r.place))
	assertNamesHaveIndexes(t, intRange(0, 1, 4, 7), stopResultsToNames(r.stop))
}

// Tests the reconciler handles a failed deployment and does no placements
func TestReconciler_FailedDeployment_NoPlacements(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = noCanaryUpdate

	// Create an existing failed deployment that has some placed allocs
	d := structs.NewDeployment(job)
	d.Status = structs.DeploymentStatusFailed
	d.TaskGroups[job.TaskGroups[0].Name] = &structs.DeploymentState{
		Promoted:     true,
		DesiredTotal: 10,
		PlacedAllocs: 4,
	}

	// Create 6 allocations from the old job
	var allocs []*structs.Allocation
	for i := 4; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		alloc.TaskGroup = job.TaskGroups[0].Name
		allocs = append(allocs, alloc)
	}

	// Create the healthy replacements
	handled := make(map[string]allocUpdateType)
	for i := 0; i < 4; i++ {
		new := mock.Alloc()
		new.Job = job
		new.JobID = job.ID
		new.NodeID = structs.GenerateUUID()
		new.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		new.TaskGroup = job.TaskGroups[0].Name
		new.DeploymentID = d.ID
		new.DeploymentStatus = &structs.AllocDeploymentStatus{
			Healthy: helper.BoolToPtr(true),
		}
		allocs = append(allocs, new)
		handled[new.ID] = allocUpdateFnIgnore
	}

	// Build a map of tainted nodes
	tainted := make(map[string]*structs.Node, 2)
	for i := 0; i < 2; i++ {
		n := mock.Node()
		n.ID = allocs[6+i].NodeID
		if i == 0 {
			n.Status = structs.NodeStatusDown
		} else {
			n.Drain = true
		}
		tainted[n.ID] = n
	}

	mockUpdateFn := allocUpdateFnMock(handled, allocUpdateFnDestructive)
	reconciler := NewAllocReconciler(testLogger(), mockUpdateFn, false, job.ID, job, d, allocs, tainted)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             0,
		inplace:           0,
		stop:              2,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Stop:   2,
				Ignore: 8,
			},
		},
	})

	assertNamesHaveIndexes(t, intRange(0, 1), stopResultsToNames(r.stop))
}

// Tests the reconciler handles a run after a deployment is complete
// successfully.
func TestReconciler_CompleteDeployment(t *testing.T) {
	job := mock.Job()
	job.TaskGroups[0].Update = canaryUpdate

	// Create allocations from the old job
	dID := structs.GenerateUUID()
	var allocs []*structs.Allocation
	for i := 0; i < 10; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = structs.GenerateUUID()
		alloc.Name = structs.AllocName(job.ID, job.TaskGroups[0].Name, uint(i))
		alloc.TaskGroup = job.TaskGroups[0].Name
		alloc.DeploymentID = dID
		if i < 2 {
			alloc.Canary = true
			alloc.DeploymentStatus = &structs.AllocDeploymentStatus{
				Healthy:  helper.BoolToPtr(true),
				Promoted: true,
			}
		}
		allocs = append(allocs, alloc)
	}

	reconciler := NewAllocReconciler(testLogger(), allocUpdateFnIgnore, false, job.ID, job, nil, allocs, nil)
	r := reconciler.Compute()

	// Assert the correct results
	assertResults(t, r, &resultExpectation{
		createDeployment:  nil,
		deploymentUpdates: nil,
		place:             0,
		inplace:           0,
		stop:              0,
		desiredTGUpdates: map[string]*structs.DesiredUpdates{
			job.TaskGroups[0].Name: {
				Ignore: 10,
			},
		},
	})
}