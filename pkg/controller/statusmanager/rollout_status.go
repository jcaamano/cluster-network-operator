package statusmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-network-operator/pkg/names"
	mcfgv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	client "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// if a rollout has not made any progress by this time,
	// mark ourselves as Degraded
	ProgressTimeout = 10 * time.Minute

	// lastSeenAnnotation - the annotation where we stash our state
	lastSeenAnnotation = "network.operator.openshift.io/last-seen-state"
)

// rolloutState is a snapshot of the last-seen-state and last-changed-times
// for pod-creating entities, as marshalled to json in an annotation
type rolloutState struct {
	// "public" for marshalling to json, since we can't have complex keys
	DaemonsetStates         []daemonsetState
	DeploymentStates        []deploymentState
	MachineConfigPoolStates []machineConfigPoolState
}

// daemonsetState is the internal state we use to check if a rollout has
// stalled.
type daemonsetState struct {
	types.NamespacedName

	LastSeenStatus appsv1.DaemonSetStatus
	LastChangeTime time.Time
}

// deploymentState is the same as daemonsetState.. but for deployments!
type deploymentState struct {
	types.NamespacedName

	LastSeenStatus appsv1.DeploymentStatus
	LastChangeTime time.Time
}

// machineConfigPoolState is the same as daemonsetState.. but for machine config pools!
type machineConfigPoolState struct {
	types.NamespacedName

	LastSeenStatus mcfgv1.MachineConfigPoolStatus
	LastChangeTime time.Time
}

// SetFromRollout sets the operator Degraded/Progressing/Available status, based on
// the current status of the manager's DaemonSets, Deployments and MachineConfigs.
func (status *StatusManager) SetFromRollout() []types.NamespacedName {
	status.Lock()
	defer status.Unlock()

	targetLevel := os.Getenv("RELEASE_VERSION")
	reachedAvailableLevel := (len(status.daemonSets)+len(status.deployments))+len(status.machineConfigs) > 0

	resources := []types.NamespacedName{}
	progressing := []string{}
	hung := []string{}

	daemonsetStates, deploymentStates, machineConfigPoolStates := status.getLastRolloutState()

	for _, dsName := range status.daemonSets {
		resources = append(resources, dsName)
		ds := &appsv1.DaemonSet{}
		if err := status.client.Get(context.TODO(), dsName, ds); err != nil {
			log.Printf("Error getting DaemonSet %q: %v", dsName.String(), err)
			progressing = append(progressing, fmt.Sprintf("Waiting for DaemonSet %q to be created", dsName.String()))
			reachedAvailableLevel = false
			// Assume the OperConfig Controller is in the process of reconciling
			// things; it will set a Degraded status if it fails.
			continue
		}

		dsProgressing := false

		if isNonCritical(ds) && ds.Status.NumberReady == 0 && !status.installComplete {
			progressing = append(progressing, fmt.Sprintf("DaemonSet %q is waiting for other operators to become ready", dsName.String()))
			dsProgressing = true
		} else if ds.Status.UpdatedNumberScheduled < ds.Status.DesiredNumberScheduled {
			progressing = append(progressing, fmt.Sprintf("DaemonSet %q update is rolling out (%d out of %d updated)", dsName.String(), ds.Status.UpdatedNumberScheduled, ds.Status.DesiredNumberScheduled))
			dsProgressing = true
		} else if ds.Status.NumberUnavailable > 0 {
			progressing = append(progressing, fmt.Sprintf("DaemonSet %q is not available (awaiting %d nodes)", dsName.String(), ds.Status.NumberUnavailable))
			dsProgressing = true
			// Check for any pods in CrashLoopBackOff state and mark the operator as degraded if so.
			if !isNonCritical(ds) {
				hung = append(hung, status.CheckCrashLoopBackOffPods(dsName, ds.Spec.Selector.MatchLabels, "DaemonSet")...)
			}
		} else if ds.Status.NumberAvailable == 0 { // NOTE: update this if we ever expect empty (unscheduled) daemonsets ~cdc
			progressing = append(progressing, fmt.Sprintf("DaemonSet %q is not yet scheduled on any nodes", dsName.String()))
			dsProgressing = true
		} else if ds.Generation > ds.Status.ObservedGeneration {
			progressing = append(progressing, fmt.Sprintf("DaemonSet %q update is being processed (generation %d, observed generation %d)", dsName.String(), ds.Generation, ds.Status.ObservedGeneration))
			dsProgressing = true
		}

		if ds.Annotations["release.openshift.io/version"] != targetLevel {
			reachedAvailableLevel = false
		}

		var dsHung *string

		if dsProgressing && !isNonCritical(ds) {
			reachedAvailableLevel = false

			dsState, exists := daemonsetStates[dsName]
			if !exists || !reflect.DeepEqual(dsState.LastSeenStatus, ds.Status) {
				dsState.LastChangeTime = time.Now()
				ds.Status.DeepCopyInto(&dsState.LastSeenStatus)
				daemonsetStates[dsName] = dsState
			}

			// Catch hung rollouts
			if exists && (time.Since(dsState.LastChangeTime)) > ProgressTimeout {
				hung = append(hung, fmt.Sprintf("DaemonSet %q rollout is not making progress - last change %s", dsName.String(), dsState.LastChangeTime.Format(time.RFC3339)))
				empty := ""
				dsHung = &empty
			}
		} else {
			delete(daemonsetStates, dsName)
		}
		if err := status.setDSAnnotation(ds, names.RolloutHungAnnotation, dsHung); err != nil {
			log.Printf("Error setting DaemonSet %q annotation: %v", dsName, err)
		}
	}

	for _, depName := range status.deployments {
		resources = append(resources, depName)
		dep := &appsv1.Deployment{}
		if err := status.client.Get(context.TODO(), depName, dep); err != nil {
			log.Printf("Error getting Deployment %q: %v", depName.String(), err)
			progressing = append(progressing, fmt.Sprintf("Waiting for Deployment %q to be created", depName.String()))
			reachedAvailableLevel = false
			// Assume the OperConfig Controller is in the process of reconciling
			// things; it will set a Degraded status if it fails.
			continue
		}

		depProgressing := false

		if isNonCritical(dep) && dep.Status.UnavailableReplicas > 0 && !status.installComplete {
			progressing = append(progressing, fmt.Sprintf("Deployment %q is waiting for other operators to become ready", depName.String()))
			depProgressing = true
		} else if dep.Status.UnavailableReplicas > 0 {
			progressing = append(progressing, fmt.Sprintf("Deployment %q is not available (awaiting %d nodes)", depName.String(), dep.Status.UnavailableReplicas))
			depProgressing = true
			// Check for any pods in CrashLoopBackOff state and mark the operator as degraded if so.
			if !isNonCritical(dep) {
				hung = append(hung, status.CheckCrashLoopBackOffPods(depName, dep.Spec.Selector.MatchLabels, "Deployment")...)
			}
		} else if dep.Status.AvailableReplicas == 0 {
			progressing = append(progressing, fmt.Sprintf("Deployment %q is not yet scheduled on any nodes", depName.String()))
			depProgressing = true
		} else if dep.Status.ObservedGeneration < dep.Generation {
			progressing = append(progressing, fmt.Sprintf("Deployment %q update is being processed (generation %d, observed generation %d)", depName.String(), dep.Generation, dep.Status.ObservedGeneration))
			depProgressing = true
		}

		if dep.Annotations["release.openshift.io/version"] != targetLevel {
			reachedAvailableLevel = false
		}

		var depHung *string

		if depProgressing && !isNonCritical(dep) {
			reachedAvailableLevel = false

			depState, exists := deploymentStates[depName]
			if !exists || !reflect.DeepEqual(depState.LastSeenStatus, dep.Status) {
				depState.LastChangeTime = time.Now()
				dep.Status.DeepCopyInto(&depState.LastSeenStatus)
				deploymentStates[depName] = depState
			}

			// Catch hung rollouts
			if exists && (time.Since(depState.LastChangeTime)) > ProgressTimeout {
				hung = append(hung, fmt.Sprintf("Deployment %q rollout is not making progress - last change %s", depName.String(), depState.LastChangeTime.Format(time.RFC3339)))
				empty := ""
				depHung = &empty
			}
		} else {
			delete(deploymentStates, depName)
		}
		if err := status.setDepAnnotation(dep, names.RolloutHungAnnotation, depHung); err != nil {
			log.Printf("Error setting Deployment %q annotation: %v", depName, err)
		}
	}

	// CNO might render MachineConfigs so its status is derived from the status
	// of the MachineConfigPool associated to those MachineConfigs.
	observedMachineConfigPools := map[types.NamespacedName]bool{}
	for _, mcName := range status.machineConfigs {
		resources = append(resources, mcName)
		mc := &mcfgv1.MachineConfig{}
		if err := status.client.Get(context.TODO(), mcName, mc); err != nil {
			log.Printf("Error getting MachineConfig %q: %v", mcName.String(), err)
			reachedAvailableLevel = false
			progressing = append(progressing, fmt.Sprintf("Waiting for MachineConfig %q to be created", mcName.String()))
			continue
		}

		mcpName, isAssignedToPool := mc.Labels[names.MachineConfigPoolLabel]
		if !isAssignedToPool {
			reachedAvailableLevel = false
			progressing = append(progressing, fmt.Sprintf("Waiting for MachineConfig %q to have a MachineConfigPool assigned", mcName.String()))
			continue
		}

		mcpNamespacedName := types.NamespacedName{Name: mcpName}
		resources = append(resources, mcpNamespacedName)
		mcp := &mcfgv1.MachineConfigPool{}
		if err := status.client.Get(context.TODO(), mcpNamespacedName, mcp); err != nil {
			log.Printf("Error getting MachineConfigPool %q: %v", mcpName, err)
			reachedAvailableLevel = false
			progressing = append(progressing, fmt.Sprintf("Waiting for MachineConfigPool %q of MachineConfig %q to be created", mcpName, mcName.String()))
			continue
		}

		var isSourcedByPool bool
		for _, obj := range mcp.Spec.Configuration.Source {
			if obj.Kind == "MachineConfig" && obj.Namespace == mc.Namespace && obj.Name == mc.Name {
				isSourcedByPool = true
				break
			}
		}

		var mcProgressing bool
		if !isSourcedByPool {
			mcProgressing = true
			progressing = append(progressing, fmt.Sprintf("MachineConfig %q is not yet sourced by MachineConfigPool %q", mcName.String(), mcpName))
		} else if mcp.Status.UpdatedMachineCount < mcp.Status.MachineCount || mcp.Status.ReadyMachineCount < mcp.Status.MachineCount {
			mcProgressing = true
			progressing = append(progressing, fmt.Sprintf("MachineConfig %q is being rolled out by MachineConfigPool %q", mcName.String(), mcpName))
		}

		if mc.Annotations["release.openshift.io/version"] != targetLevel {
			reachedAvailableLevel = false
		}

		if mcProgressing && !isNonCritical(mc) {
			reachedAvailableLevel = false
			observedMachineConfigPools[mcpNamespacedName] = true
			mcpState, exists := machineConfigPoolStates[mcpNamespacedName]
			if !exists || !reflect.DeepEqual(mcpState.LastSeenStatus, mcp.Status) {
				mcpState.LastChangeTime = time.Now()
				mcp.Status.DeepCopyInto(&mcpState.LastSeenStatus)
				machineConfigPoolStates[mcpNamespacedName] = mcpState
			}

			// Catch hung rollouts
			if exists && (time.Since(mcpState.LastChangeTime)) > ProgressTimeout {
				hung = append(hung, fmt.Sprintf("MachineConfig %q rollout via MachineConfigPool %q is not making progress - last change %s", mcName.String(), mcpName, mcpState.LastChangeTime.Format(time.RFC3339)))
			}
		}
	}
	for mcpNamespacedName := range machineConfigPoolStates {
		if !observedMachineConfigPools[mcpNamespacedName] {
			delete(machineConfigPoolStates, mcpNamespacedName)
		}
	}

	status.setNotDegraded(PodDeployment)
	if err := status.setLastRolloutState(daemonsetStates, deploymentStates, machineConfigPoolStates); err != nil {
		log.Printf("Failed to set rollout state (continuing): %+v\n", err)
	}

	conditions := make([]operv1.OperatorCondition, 0, 2)
	if len(progressing) > 0 {
		conditions = append(conditions,
			operv1.OperatorCondition{
				Type:    operv1.OperatorStatusTypeProgressing,
				Status:  operv1.ConditionTrue,
				Reason:  "Deploying",
				Message: strings.Join(progressing, "\n"),
			},
		)
	} else {
		conditions = append(conditions,
			operv1.OperatorCondition{
				Type:   operv1.OperatorStatusTypeProgressing,
				Status: operv1.ConditionFalse,
			},
		)
	}
	if reachedAvailableLevel {
		conditions = append(conditions,
			operv1.OperatorCondition{
				Type:   operv1.OperatorStatusTypeAvailable,
				Status: operv1.ConditionTrue,
			},
		)
	}

	if reachedAvailableLevel && len(progressing) == 0 {
		status.installComplete = true
	}

	status.set(reachedAvailableLevel, conditions...)
	if len(hung) > 0 {
		status.setDegraded(RolloutHung, "RolloutHung", strings.Join(hung, "\n"))
	} else {
		status.setNotDegraded(RolloutHung)
	}

	return resources
}

// getLastRolloutState reads the last-seen daemonset, deployment and machine
// config pool state from the clusteroperator annotation and parses it. On
// error, it returns an empty state, since this should not block updating
// operator status.
func (status *StatusManager) getLastRolloutState() (map[types.NamespacedName]daemonsetState, map[types.NamespacedName]deploymentState, map[types.NamespacedName]machineConfigPoolState) {
	// with maps allocated
	daemonsetStates := map[types.NamespacedName]daemonsetState{}
	deploymentStates := map[types.NamespacedName]deploymentState{}
	machineConfigPoolStates := map[types.NamespacedName]machineConfigPoolState{}

	// Load the last-seen snapshot from our annotation
	co := &configv1.ClusterOperator{ObjectMeta: metav1.ObjectMeta{Name: status.name}}
	err := status.client.Get(context.TODO(), types.NamespacedName{Name: status.name}, co)
	if err != nil {
		log.Printf("Failed to get ClusterOperator: %v", err)
		return daemonsetStates, deploymentStates, machineConfigPoolStates
	}

	lsbytes := co.Annotations[lastSeenAnnotation]
	if lsbytes == "" {
		return daemonsetStates, deploymentStates, machineConfigPoolStates
	}

	out := rolloutState{}
	err = json.Unmarshal([]byte(lsbytes), &out)
	if err != nil {
		// No need to return error; just move on
		log.Printf("failed to unmashal last-seen-status: %v", err)
		return daemonsetStates, deploymentStates, machineConfigPoolStates
	}

	for _, ds := range out.DaemonsetStates {
		daemonsetStates[ds.NamespacedName] = ds
	}

	for _, ds := range out.DeploymentStates {
		deploymentStates[ds.NamespacedName] = ds
	}

	for _, mcps := range out.MachineConfigPoolStates {
		machineConfigPoolStates[mcps.NamespacedName] = mcps
	}

	return daemonsetStates, deploymentStates, machineConfigPoolStates
}

func (status *StatusManager) setLastRolloutState(
	dss map[types.NamespacedName]daemonsetState,
	deps map[types.NamespacedName]deploymentState,
	mcps map[types.NamespacedName]machineConfigPoolState) error {

	ps := rolloutState{
		DaemonsetStates:         make([]daemonsetState, 0, len(dss)),
		DeploymentStates:        make([]deploymentState, 0, len(deps)),
		MachineConfigPoolStates: make([]machineConfigPoolState, 0, len(mcps)),
	}

	for nsn, ds := range dss {
		ds.NamespacedName = nsn
		ps.DaemonsetStates = append(ps.DaemonsetStates, ds)
	}

	for nsn, ds := range deps {
		ds.NamespacedName = nsn
		ps.DeploymentStates = append(ps.DeploymentStates, ds)
	}

	for nsn, ms := range mcps {
		ms.NamespacedName = nsn
		ps.MachineConfigPoolStates = append(ps.MachineConfigPoolStates, ms)
	}

	lsbytes, err := json.Marshal(ps)
	if err != nil {
		return err
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		oldStatus := &configv1.ClusterOperator{ObjectMeta: metav1.ObjectMeta{Name: status.name}}
		err := status.client.Get(context.TODO(), types.NamespacedName{Name: status.name}, oldStatus)
		isNotFound := errors.IsNotFound(err)
		if err != nil && !isNotFound {
			return err
		}

		newStatus := oldStatus.DeepCopy()
		if newStatus.Annotations == nil {
			newStatus.Annotations = map[string]string{}
		}
		newStatus.Annotations[lastSeenAnnotation] = string(lsbytes)
		return status.client.Patch(context.TODO(), newStatus, client.MergeFrom(oldStatus))
	})
}

// CheckCrashLoopBackOffPods checks for pods (matching the label selector) with
// any containers in the CrashLoopBackoff state. It returns a human-readable string
// for any pod in such a state.
// dName should be the name of a DaemonSet or Deployment.
func (status *StatusManager) CheckCrashLoopBackOffPods(dName types.NamespacedName, selector map[string]string, kind string) []string {
	hung := []string{}
	pods := &v1.PodList{}
	err := status.client.List(context.TODO(), pods, client.InNamespace(dName.Namespace), client.MatchingLabels(selector))
	if err != nil {
		log.Printf("Error getting pods from %s %q: %v", kind, dName.String(), err)
	}
	for _, pod := range pods.Items {
		for _, container := range pod.Status.ContainerStatuses {
			if container.State.Waiting != nil {
				if container.State.Waiting.Reason == "CrashLoopBackOff" {
					hung = append(hung, fmt.Sprintf("%s %q rollout is not making progress - pod %s is in CrashLoopBackOff State", kind, dName.String(), pod.Name))
					// we can break once we find at least one container crashing in this pod
					break
				}
			}
		}
	}
	return hung
}

func isNonCritical(obj metav1.Object) bool {
	_, exists := obj.GetAnnotations()[names.NonCriticalAnnotation]
	return exists
}

// setDSAnnotation sets an annotation on a daemonset; or unsets it if value is nil
func (status *StatusManager) setDSAnnotation(obj *appsv1.DaemonSet, key string, value *string) error {
	new := obj.DeepCopy()
	anno := new.GetAnnotations()

	existing, set := anno[key]
	if value != nil && set && existing == *value {
		return nil
	}
	if !set && value == nil {
		return nil
	}

	if value != nil {
		if anno == nil {
			anno = map[string]string{}
		}
		anno[key] = *value
	} else {
		delete(anno, key)
	}
	new.SetAnnotations(anno)
	return status.client.Patch(context.TODO(), new, client.MergeFrom(obj))
}

// setDepAnnotation sets an annotation on a Deployment. If value is nil,
// it unsets the annotation
func (status *StatusManager) setDepAnnotation(obj *appsv1.Deployment, key string, value *string) error {
	new := obj.DeepCopy()
	anno := new.GetAnnotations()

	existing, set := anno[key]
	if value != nil && set && existing == *value {
		return nil
	}
	if !set && value == nil {
		return nil
	}

	if value != nil {
		if anno == nil {
			anno = map[string]string{}
		}
		anno[key] = *value
	} else {
		delete(anno, key)
	}
	new.SetAnnotations(anno)
	return status.client.Patch(context.TODO(), new, client.MergeFrom(obj))
}
