package deploymentwatcher

import (
	"fmt"
	"log"
	"sync"

	"github.com/hashicorp/nomad/nomad/structs"
)

// DeploymentRaftEndpoints exposes the deployment watcher to a set of functions
// to apply data transforms via Raft.
type DeploymentRaftEndpoints interface {
	// UpsertEvals is used to upsert a set of evaluations
	UpsertEvals([]*structs.Evaluation) (uint64, error)

	// UpsertJob is used to upsert a job
	UpsertJob(job *structs.Job) (uint64, error)

	// UpsertDeploymentStatusUpdate is used to upsert a deployment status update
	// and potentially create an evaluation.
	UpsertDeploymentStatusUpdate(u *structs.DeploymentStatusUpdateRequest) (uint64, error)

	// UpsertDeploymentPromotion is used to promote canaries in a deployment
	UpsertDeploymentPromotion(req *structs.ApplyDeploymentPromoteRequest) (uint64, error)

	// UpsertDeploymentAllocHealth is used to set the health of allocations in a
	// deployment
	UpsertDeploymentAllocHealth(req *structs.ApplyDeploymentAllocHealthRequest) (uint64, error)
}

// DeploymentStateWatchers are the set of functions required to watch objects on
// behalf of a deployment
type DeploymentStateWatchers interface {
	// Evaluations returns the set of evaluations for the given job
	Evaluations(args *structs.JobSpecificRequest, reply *structs.JobEvaluationsResponse) error

	// Allocations returns the set of allocations that are part of the
	// deployment.
	Allocations(args *structs.DeploymentSpecificRequest, reply *structs.AllocListResponse) error

	// GetJobVersions is used to lookup the versions of a job. This is used when
	// rolling back to find the latest stable job
	GetJobVersions(args *structs.JobSpecificRequest, reply *structs.JobVersionsResponse) error
}

// Watcher is used to watch deployments and their allocations created
// by the scheduler and trigger the scheduler when allocation health
// transistions.
type Watcher struct {
	enabled bool
	logger  *log.Logger

	// raft contains the set of Raft endpoints that can be used by the
	// deployments watcher
	raft DeploymentRaftEndpoints

	// stateWatchers is the set of functions required to watch a deployment for
	// state changes
	stateWatchers DeploymentStateWatchers

	// watchers is the set of active watchers, one per deployment
	watchers map[string]*deploymentWatcher

	// evalBatcher is used to batch the creation of evaluations
	evalBatcher *EvalBatcher

	// exitCh is used to exit any goroutines spawned by the watcher
	exitCh chan struct{}

	l sync.RWMutex
}

// NewDeploymentsWatcher returns a deployments watcher that is used to watch
// deployments and trigger the scheduler as needed.
func NewDeploymentsWatcher(logger *log.Logger, w DeploymentStateWatchers, raft DeploymentRaftEndpoints) *Watcher {
	exitCh := make(chan struct{})
	return &Watcher{
		stateWatchers: w,
		raft:          raft,
		watchers:      make(map[string]*deploymentWatcher, 32),
		evalBatcher:   NewEvalBatcher(raft, exitCh),
		exitCh:        exitCh,
		logger:        logger,
	}
}

// SetEnabled is used to control if the watcher is enabled. The watcher
// should only be enabled on the active leader.
func (w *Watcher) SetEnabled(enabled bool) {
	w.l.Lock()
	w.enabled = enabled
	w.l.Unlock()
	if !enabled {
		w.Flush()
	}
}

// Flush is used to clear the state of the watcher
func (w *Watcher) Flush() {
	w.l.Lock()
	defer w.l.Unlock()

	// Stop all the watchers and clear it
	for _, watcher := range w.watchers {
		watcher.StopWatch()
	}

	close(w.exitCh)

	w.watchers = make(map[string]*deploymentWatcher, 32)
	w.exitCh = make(chan struct{})
	w.evalBatcher = NewEvalBatcher(w.raft, w.exitCh)
}

// Watch adds a deployment to the watch list
func (w *Watcher) Watch(d *structs.Deployment, j *structs.Job) {
	w.l.Lock()
	defer w.l.Unlock()

	// Not enabled so no-op
	if !w.enabled {
		return
	}

	// Already watched so no-op
	if _, ok := w.watchers[d.ID]; ok {
		return
	}

	w.watchers[d.ID] = newDeploymentWatcher(w.logger, w.stateWatchers, d, j, w)
}

// Unwatch stops watching a deployment. This can be because the deployment is
// complete or being deleted.
func (w *Watcher) Unwatch(d *structs.Deployment) {
	w.l.Lock()
	defer w.l.Unlock()

	// Not enabled so no-op
	if !w.enabled {
		return
	}

	if watcher, ok := w.watchers[d.ID]; ok {
		watcher.StopWatch()
		delete(w.watchers, d.ID)
	}
}

// SetAllocHealth is used to set the health of allocations for a deployment. If
// there are any unhealthy allocations, the deployment is updated to be failed.
// Otherwise the allocations are updated and an evaluation is created.
func (w *Watcher) SetAllocHealth(req *structs.DeploymentAllocHealthRequest) (
	*structs.DeploymentUpdateResponse, error) {
	w.l.Lock()
	defer w.l.Unlock()

	// Not enabled so no-op
	if !w.enabled {
		return nil, nil
	}

	watcher, ok := w.watchers[req.DeploymentID]
	if !ok {
		return nil, fmt.Errorf("deployment %q not being watched for updates", req.DeploymentID)
	}

	return watcher.SetAllocHealth(req)
}

// PromoteDeployment is used to promote a deployment. If promote is false,
// deployment is marked as failed. Otherwise the deployment is updated and an
// evaluation is created.
func (w *Watcher) PromoteDeployment(req *structs.DeploymentPromoteRequest) (
	*structs.DeploymentUpdateResponse, error) {
	w.l.Lock()
	defer w.l.Unlock()

	// Not enabled so no-op
	if !w.enabled {
		return nil, nil
	}

	watcher, ok := w.watchers[req.DeploymentID]
	if !ok {
		return nil, fmt.Errorf("deployment %q not being watched for updates", req.DeploymentID)
	}

	return watcher.PromoteDeployment(req)
}

// PauseDeployment is used to toggle the pause state on a deployment. If the
// deployment is being unpaused, an evaluation is created.
func (w *Watcher) PauseDeployment(req *structs.DeploymentPauseRequest) (
	*structs.DeploymentUpdateResponse, error) {
	w.l.Lock()
	defer w.l.Unlock()

	// Not enabled so no-op
	if !w.enabled {
		return nil, nil
	}

	watcher, ok := w.watchers[req.DeploymentID]
	if !ok {
		return nil, fmt.Errorf("deployment %q not being watched for updates", req.DeploymentID)
	}

	return watcher.PauseDeployment(req)
}

// createEvaluation commits the given evaluation to Raft but batches the commit
// with other calls.
func (w *Watcher) createEvaluation(eval *structs.Evaluation) (uint64, error) {
	w.l.Lock()
	f := w.evalBatcher.CreateEval(eval)
	w.l.Unlock()

	return f.Results()
}

// upsertJob commits the given job to Raft
func (w *Watcher) upsertJob(job *structs.Job) (uint64, error) {
	return w.raft.UpsertJob(job)
}

// upsertDeploymentStatusUpdate commits the given deployment update and optional
// evaluation to Raft
func (w *Watcher) upsertDeploymentStatusUpdate(
	u *structs.DeploymentStatusUpdate,
	e *structs.Evaluation,
	j *structs.Job) (uint64, error) {
	return w.raft.UpsertDeploymentStatusUpdate(&structs.DeploymentStatusUpdateRequest{
		DeploymentUpdate: u,
		Eval:             e,
		Job:              j,
	})
}

// upsertDeploymentPromotion commits the given deployment promotion to Raft
func (w *Watcher) upsertDeploymentPromotion(req *structs.ApplyDeploymentPromoteRequest) (uint64, error) {
	return w.raft.UpsertDeploymentPromotion(req)
}

// upsertDeploymentAllocHealth commits the given allocation health changes to
// Raft
func (w *Watcher) upsertDeploymentAllocHealth(req *structs.ApplyDeploymentAllocHealthRequest) (uint64, error) {
	return w.raft.UpsertDeploymentAllocHealth(req)
}