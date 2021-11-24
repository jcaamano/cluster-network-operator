package operconfig

import (
	"context"
	"log"

	"github.com/openshift/cluster-network-operator/pkg/controller/statusmanager"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// newRolloutReconciler returns a new reconcile.Reconciler
func newRolloutReconciler(status *statusmanager.StatusManager) *ReconcileRollout {
	return &ReconcileRollout{status: status}
}

var _ reconcile.Reconciler = &ReconcileRollout{}

// ReconcileRollout watches for updates to specified resources and then updates its StatusManager
type ReconcileRollout struct {
	status *statusmanager.StatusManager

	resources []types.NamespacedName
}

func (r *ReconcileRollout) SetResources(resources []types.NamespacedName) {
	r.resources = resources
}

// Reconcile updates the ClusterOperator.Status to match the current state of the
// watched Deployments/DaemonSets/MachineConfigs/MachineConfigPools
func (r *ReconcileRollout) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	found := false
	for _, name := range r.resources {
		if name.Namespace == request.Namespace && name.Name == request.Name {
			found = true
			break
		}
	}
	if !found {
		return reconcile.Result{}, nil
	}

	log.Printf("Reconciling update to %s/%s\n", request.Namespace, request.Name)
	r.status.SetFromRollout()

	return reconcile.Result{RequeueAfter: ResyncPeriod}, nil
}
