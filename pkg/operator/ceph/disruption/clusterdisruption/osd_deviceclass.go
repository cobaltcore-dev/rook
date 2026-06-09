/*
Copyright 2026 The Rook Authors. All rights reserved.

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

package clusterdisruption

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pkg/errors"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	"github.com/rook/rook/pkg/operator/ceph/cluster/osd"
	opcontroller "github.com/rook/rook/pkg/operator/ceph/controller"
	"github.com/rook/rook/pkg/operator/k8sutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// classDrainState is the per-device-class generalization of the failure-domain data the
// fallback path computes globally. Each device class is enumerated using its own minimum
// failure-domain type, so classes whose pools use different failure domains (for example
// hdd pools on zone, ssd pools on host) are handled independently.
type classDrainState struct {
	failureDomainType       string
	allFailureDomains       []string
	nodeDrainFailureDomains []string
	osdDownFailureDomains   []string
	downOSDs                []int
}

// pdbKey identifies a blocking PDB by its failure-domain type and name. Two device classes
// that share a failure-domain type and name share a single blocking PDB (its device-class
// In list then lists both), so the PDB object is keyed independently of device class.
type pdbKey struct {
	failureDomainType string
	failureDomainName string
}

// deviceClassKeyPrefix namespaces all per-device-class ConfigMap keys. The "/" separator cannot
// appear in a Ceph device-class name, so per-class keys never collide with the legacy
// (class-agnostic) keys, and the fallback path can identify and clear them by prefix.
const deviceClassKeyPrefix = "device-class/"

// classConfigKey returns the per-device-class ConfigMap key for a base key (for example
// "draining-failure-domain" + "hdd" -> "device-class/hdd/draining-failure-domain").
func classConfigKey(baseKey, deviceClass string) string {
	return fmt.Sprintf("%s%s/%s", deviceClassKeyPrefix, deviceClass, baseKey)
}

// getOSDFailureDomainsByClass is the per-class analog of getOSDFailureDomains. For each
// device class it enumerates that class's failure domains using the class's own minimum
// failure-domain type. OSDs whose device-class label is empty or names a class with no
// pools are skipped here; they remain covered by the default PDB and never participate in a
// class-aware drain.
func (r *ReconcileClusterDisruption) getOSDFailureDomainsByClass(clusterInfo *cephclient.ClusterInfo, request reconcile.Request, classFailureDomain map[string]string) (map[string]*classDrainState, error) {
	osdDeploymentList := &appsv1.DeploymentList{}
	namespaceListOpts := client.InNamespace(request.Namespace)
	err := r.client.List(clusterInfo.Context, osdDeploymentList, client.MatchingLabels{k8sutil.AppAttr: osd.AppName}, namespaceListOpts)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list osd deployments")
	}

	osdMetadata, err := cephclient.GetOSDMetadata(r.context.ClusterdContext, clusterInfo)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get OSD metadata")
	}

	states := map[string]*classDrainState{}
	allFDs := map[string]sets.Set[string]{}
	nodeDrainFDs := map[string]sets.Set[string]{}
	osdDownFDs := map[string]sets.Set[string]{}
	for class, fdType := range classFailureDomain {
		states[class] = &classDrainState{failureDomainType: fdType}
		allFDs[class] = sets.New[string]()
		nodeDrainFDs[class] = sets.New[string]()
		osdDownFDs[class] = sets.New[string]()
	}

	for _, deployment := range osdDeploymentList.Items {
		labels := deployment.GetLabels()
		class := labels[osdDeviceClassLabel]
		fdType, tracked := classFailureDomain[class]
		if !tracked {
			// OSD has no device-class label, or its class has no pools. It stays
			// protected by the default PDB and is excluded from class-aware drains.
			continue
		}
		topologyLocationLabel := fmt.Sprintf(osd.TopologyLocationLabel, fdType)
		failureDomainName := labels[topologyLocationLabel]
		if failureDomainName == "" {
			return nil, errors.Errorf("failed to get the topology location label %q in OSD deployment %q",
				topologyLocationLabel, deployment.Name)
		}
		allFDs[class].Insert(failureDomainName)

		if deployment.Status.ReadyReplicas >= 1 {
			continue
		}
		osdDownFDs[class].Insert(failureDomainName)

		osdID, err := osd.GetOSDID(&deployment)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get ID for the OSD deployment %q", deployment.Name)
		}
		states[class].downOSDs = append(states[class].downOSDs, osdID)

		var osdNodeName string
		for _, metadata := range *osdMetadata {
			if metadata.Id == osdID {
				osdNodeName = metadata.HostName
			}
		}
		if osdNodeName == "" {
			logger.Warningf("failed to get the node name for the OSD %d", osdID)
			continue
		}
		isDrained, err := hasOSDNodeDrained(clusterInfo.Context, r.client, osdNodeName)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to check if osd %q node is drained", deployment.Name)
		}
		if isDrained {
			logger.Infof("osd %q (class %q) is down on node %q and a possible node drain is detected", deployment.Name, class, osdNodeName)
			nodeDrainFDs[class].Insert(failureDomainName)
		} else if !strings.HasSuffix(deployment.Name, "-debug") {
			logger.Infof("osd %q (class %q) is down on node %q but no node drain is detected", deployment.Name, class, osdNodeName)
		}
	}

	for class, state := range states {
		state.allFailureDomains = sets.List(allFDs[class])
		state.nodeDrainFailureDomains = sets.List(nodeDrainFDs[class])
		state.osdDownFailureDomains = sets.List(osdDownFDs[class])
	}
	return states, nil
}

// anyOSDDown reports whether any OSD deployment has no ready replica, using only the Kubernetes
// API (no Ceph round-trip) so an idle, healthy cluster can be detected cheaply.
func (r *ReconcileClusterDisruption) anyOSDDown(request reconcile.Request) (bool, error) {
	osdDeploymentList := &appsv1.DeploymentList{}
	err := r.client.List(r.context.OpManagerContext, osdDeploymentList,
		client.MatchingLabels{k8sutil.AppAttr: osd.AppName}, client.InNamespace(request.Namespace))
	if err != nil {
		return false, errors.Wrap(err, "failed to list osd deployments")
	}
	for i := range osdDeploymentList.Items {
		if osdDeploymentList.Items[i].Status.ReadyReplicas < 1 {
			return true, nil
		}
	}
	return false, nil
}

// hasActiveDrainState reports whether any drain bookkeeping exists in the ConfigMap: the legacy
// (class-agnostic) draining key or any per-device-class key.
func hasActiveDrainState(pdbStateMap *corev1.ConfigMap) bool {
	if pdbStateMap.Data[drainingFailureDomainKey] != "" {
		return true
	}
	for key := range pdbStateMap.Data {
		if strings.HasPrefix(key, deviceClassKeyPrefix) {
			return true
		}
	}
	return false
}

// reconcileIdleOSDPDBs handles the steady state where no OSD is down and no drain is in progress.
// The PDB layout is identical on either path then -- just the default PDB with no class clause and
// no blocking PDBs -- so it is applied without reading Ceph. This keeps idle reconciles (and every
// reconcile on a cluster that never uses the device-class feature) off the CRUSH/PG queries, and
// means a transient Ceph read failure cannot block PDB management for an otherwise-healthy cluster.
func (r *ReconcileClusterDisruption) reconcileIdleOSDPDBs(request reconcile.Request) (reconcile.Result, error) {
	if err := r.createDefaultPDBforOSD(request.Namespace, nil, nil); err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to create default pdb")
	}
	existing, err := r.listBlockingPDBsByName(request.Namespace)
	if err != nil {
		return reconcile.Result{}, err
	}
	for name := range existing {
		if err := r.deleteBlockingPDBByName(request.Namespace, name); err != nil {
			return reconcile.Result{}, err
		}
	}
	return r.requeuePDBController(request)
}

// reconcilePDBsForOSDsByClass is the device-class-aware counterpart of reconcilePDBsForOSDs.
// It updates the per-class ConfigMap state from per-class PG health, then applies the desired
// default and blocking PDBs using the gap-free three-phase write ordering.
func (r *ReconcileClusterDisruption) reconcilePDBsForOSDsByClass(
	clusterInfo *cephclient.ClusterInfo,
	request reconcile.Request,
	pdbStateMap *corev1.ConfigMap,
	classStates map[string]*classDrainState,
	classPools map[string][]string,
	pgHealthyRegex string,
) (reconcile.Result, error) {
	// Update per-class ConfigMap state based on each class's PG health and down OSDs. PG health is
	// queried only for classes that have a down OSD or an active drain; an idle class with all OSDs
	// up needs no per-class cleanliness query, which keeps idle reconciles cheap.
	excludeOSDs := []int{}
	for _, class := range sortedClasses(classStates) {
		state := classStates[class]
		osdDown := len(state.downOSDs) > 0
		draining := pdbStateMap.Data[classConfigKey(drainingFailureDomainKey, class)] != ""
		if !osdDown && !draining {
			resetPDBConfigForClass(pdbStateMap, class)
			continue
		}
		pgHealthMsg, pgClean, err := cephclient.ArePoolsClean(r.context.ClusterdContext, clusterInfo, classPools[class], pgHealthyRegex)
		if err != nil {
			return cephNotReadyResult(request, err)
		}
		excludeOSDs = append(excludeOSDs, r.updateClassPDBConfig(pdbStateMap, class, state, pgClean, pgHealthMsg)...)
	}

	// Build the desired PDB layout from the per-class draining state.
	drainingClasses := []string{}
	blockingDesired := map[pdbKey]sets.Set[string]{}
	for _, class := range sortedClasses(classStates) {
		state := classStates[class]
		drainingFD := pdbStateMap.Data[classConfigKey(drainingFailureDomainKey, class)]
		if drainingFD == "" {
			continue
		}
		drainingClasses = append(drainingClasses, class)
		// Block this class in every failure domain where it has OSDs except the one draining.
		for _, fd := range state.allFailureDomains {
			if fd == drainingFD {
				continue
			}
			key := pdbKey{failureDomainType: state.failureDomainType, failureDomainName: fd}
			if blockingDesired[key] == nil {
				blockingDesired[key] = sets.New[string]()
			}
			blockingDesired[key].Insert(class)
		}
	}

	if err := r.applyDeviceClassPDBs(request.Namespace, drainingClasses, blockingDesired, excludeOSDs); err != nil {
		return reconcile.Result{}, errors.Wrap(err, "failed to apply device-class-aware PDBs")
	}

	if err := r.updateNooutForClasses(clusterInfo, pdbStateMap, classStates); err != nil {
		logger.Errorf("failed to update maintenance noout in cluster %q. %v", request, err)
	}

	if err := r.client.Update(clusterInfo.Context, pdbStateMap); err != nil {
		if errors.Is(err, context.Canceled) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, errors.Wrapf(err, "failed to update configMap %q in cluster %q", pdbStateMapName, request)
	}

	return r.requeuePDBController(request)
}

// updateClassPDBConfig updates the ConfigMap drain state for a single device class, mirroring
// the global switch in reconcilePDBsForOSDs. It returns the down OSDs that should be excluded
// from the default PDB (down but healthy, no active drain for this class).
func (r *ReconcileClusterDisruption) updateClassPDBConfig(pdbStateMap *corev1.ConfigMap, class string, state *classDrainState, pgClean bool, pgHealthMsg string) []int {
	osdDown := len(state.downOSDs) > 0
	drainingFDKey := classConfigKey(drainingFailureDomainKey, class)
	excludeOSDs := []int{}

	switch {
	case !osdDown && pgClean:
		logger.Infof("device class %q: OSDs are up and PGs are clean. PG status: %q", class, pgHealthMsg)
		resetPDBConfigForClass(pdbStateMap, class)
	case osdDown && pgClean:
		logger.Infof("device class %q: OSD(s) %v are down but PGs are clean. PG Status: %q", class, state.downOSDs, pgHealthMsg)
		if len(state.nodeDrainFailureDomains) > 0 {
			lastNodeDrainTimeStamp, err := getLastNodeDrainTimeStamp(pdbStateMap, classConfigKey(drainingFailureDomainDurationKey, class))
			if err != nil {
				logger.Errorf("device class %q: failed to get last node drain timestamp. %v", class, err)
			} else if time.Since(lastNodeDrainTimeStamp) < 60*time.Second {
				logger.Infof("device class %q: node drain is detected. Waiting to ensure correct PG status is read.", class)
			} else {
				excludeOSDs = slices.Clone(state.downOSDs)
			}
		} else {
			excludeOSDs = slices.Clone(state.downOSDs)
			resetPDBConfigForClass(pdbStateMap, class)
		}
	case osdDown && !pgClean:
		setPDBConfigForClass(pdbStateMap, class, state.osdDownFailureDomains, state.nodeDrainFailureDomains)
		logger.Infof("device class %q: OSD(s) %v are down and PGs are not clean. PGs Status: %q", class, state.downOSDs, pgHealthMsg)
	case !osdDown && !pgClean && len(pdbStateMap.Data[drainingFDKey]) > 1:
		logger.Infof("device class %q: OSDs are up but PGs are not clean from previous drain event. PGs Status: %q", class, pgHealthMsg)
	}
	return excludeOSDs
}

// applyDeviceClassPDBs writes the desired default and blocking PDBs using the gap-free
// three-phase ordering: widen blocking coverage, update the default PDB, then narrow blocking
// coverage. Every transient state is a double-match (eviction fails closed with HTTP 500),
// never a no-match gap.
func (r *ReconcileClusterDisruption) applyDeviceClassPDBs(namespace string, drainingClasses []string, blockingDesired map[pdbKey]sets.Set[string], excludeOSDs []int) error {
	existing, err := r.listBlockingPDBsByName(namespace)
	if err != nil {
		return err
	}

	desiredByName := map[string]pdbKey{}
	for key := range blockingDesired {
		desiredByName[getPDBName(key.failureDomainType, key.failureDomainName)] = key
	}

	// Phase 1: widen blocking PDBs to the union of their existing and desired In lists. Track the
	// PDBs already at their desired set so phase 3 can skip a redundant identical write.
	alreadyNarrow := map[string]bool{}
	for key, desiredIn := range blockingDesired {
		name := getPDBName(key.failureDomainType, key.failureDomainName)
		unionIn := desiredIn.Union(parseBlockingInClasses(existing[name]))
		if err := r.upsertBlockingPDBForClass(namespace, key, sets.List(unionIn)); err != nil {
			return err
		}
		alreadyNarrow[name] = unionIn.Equal(desiredIn)
	}

	// Phase 2: update the default PDB with the new device-class NotIn list.
	if err := r.createDefaultPDBforOSD(namespace, excludeOSDs, drainingClasses); err != nil {
		return errors.Wrap(err, "failed to create default pdb")
	}

	// Phase 3: narrow blocking PDBs to their desired In lists, deleting any that should not exist.
	names := sets.New[string]()
	for name := range existing {
		names.Insert(name)
	}
	for name := range desiredByName {
		names.Insert(name)
	}
	for _, name := range sets.List(names) {
		key, desired := desiredByName[name]
		if !desired {
			if err := r.deleteBlockingPDBByName(namespace, name); err != nil {
				return err
			}
			continue
		}
		if alreadyNarrow[name] {
			// phase 1 already wrote the desired set; no narrowing needed
			continue
		}
		if err := r.upsertBlockingPDBForClass(namespace, key, sets.List(blockingDesired[key])); err != nil {
			return err
		}
	}
	return nil
}

// listBlockingPDBsByName returns the existing blocking OSD PDBs keyed by name. The default OSD
// PDB and the static RGW/MDS PDBs are excluded.
func (r *ReconcileClusterDisruption) listBlockingPDBsByName(namespace string) (map[string]*policyv1.PodDisruptionBudget, error) {
	pdbList := &policyv1.PodDisruptionBudgetList{}
	if err := r.client.List(r.context.OpManagerContext, pdbList, client.InNamespace(namespace)); err != nil {
		return nil, errors.Wrap(err, "failed to list pdbs")
	}
	result := map[string]*policyv1.PodDisruptionBudget{}
	for i := range pdbList.Items {
		pdb := &pdbList.Items[i]
		if pdb.Name == osdPDBAppName {
			continue
		}
		if !strings.HasPrefix(pdb.Name, osdPDBAppName+"-") {
			continue
		}
		result[pdb.Name] = pdb
	}
	return result, nil
}

// upsertBlockingPDBForClass creates or updates a blocking PDB (maxUnavailable=0) whose selector
// matches the failure domain and the listed device classes.
func (r *ReconcileClusterDisruption) upsertBlockingPDBForClass(namespace string, key pdbKey, inClasses []string) error {
	cephCluster, ok := r.clusterMap.GetCluster(namespace)
	if !ok {
		return errors.Errorf("failed to find the namespace %q in the clustermap", namespace)
	}
	pdbName := getPDBName(key.failureDomainType, key.failureDomainName)
	maxUnavailable := intstr.FromInt32(0)
	selector := &metav1.LabelSelector{
		MatchLabels: map[string]string{fmt.Sprintf(osd.TopologyLocationLabel, key.failureDomainType): key.failureDomainName},
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      osdDeviceClassLabel,
				Operator: metav1.LabelSelectorOpIn,
				Values:   inClasses,
			},
		},
	}
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: pdbName, Namespace: namespace},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       selector,
		},
	}
	ownerInfo := k8sutil.NewOwnerInfo(cephCluster, r.scheme)
	if err := ownerInfo.SetControllerReference(pdb); err != nil {
		return errors.Wrapf(err, "failed to set owner reference to pdb %v", pdb)
	}

	existingPDB := &policyv1.PodDisruptionBudget{}
	err := r.client.Get(r.context.OpManagerContext, types.NamespacedName{Name: pdbName, Namespace: namespace}, existingPDB)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Infof("creating blocking pdb %q with maxUnavailable=0 for device classes %v in %q %q", pdbName, inClasses, key.failureDomainType, key.failureDomainName)
			return r.createPDB(pdb)
		}
		return errors.Wrapf(err, "failed to get pdb %q", pdbName)
	}
	existingPDB.Spec = pdb.Spec
	return r.client.Update(r.context.OpManagerContext, existingPDB)
}

// deleteBlockingPDBByName deletes a blocking PDB by name.
func (r *ReconcileClusterDisruption) deleteBlockingPDBByName(namespace, name string) error {
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	err := r.client.Get(r.context.OpManagerContext, types.NamespacedName{Name: name, Namespace: namespace}, &policyv1.PodDisruptionBudget{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return errors.Wrapf(err, "failed to get pdb %q", name)
	}
	logger.Infof("deleting blocking pdb %q", name)
	return r.deletePDB(pdb)
}

// cleanupDeviceClassArtifacts removes everything the class-aware path created, so the fallback
// path can take over cleanly. It deletes blocking PDBs that carry a device-class In clause (a
// classless fallback blocking PDB has none) and clears the per-class ConfigMap keys. Without
// this, a class-scoped blocking PDB left behind when isolation is lost mid-drain would either
// keep a stale device-class clause (leaving other classes unprotected) or coexist with the
// recreated classless default PDB and double-match a pod (HTTP 500 on eviction).
func (r *ReconcileClusterDisruption) cleanupDeviceClassArtifacts(namespace string, pdbStateMap *corev1.ConfigMap) error {
	existing, err := r.listBlockingPDBsByName(namespace)
	if err != nil {
		return err
	}
	for name, pdb := range existing {
		if parseBlockingInClasses(pdb).Len() == 0 {
			continue
		}
		if err := r.deleteBlockingPDBByName(namespace, name); err != nil {
			return err
		}
	}
	for key := range pdbStateMap.Data {
		if strings.HasPrefix(key, deviceClassKeyPrefix) {
			delete(pdbStateMap.Data, key)
		}
	}
	return nil
}

// parseBlockingInClasses extracts the device-class In list from an existing blocking PDB.
func parseBlockingInClasses(pdb *policyv1.PodDisruptionBudget) sets.Set[string] {
	result := sets.New[string]()
	if pdb == nil || pdb.Spec.Selector == nil {
		return result
	}
	for _, expr := range pdb.Spec.Selector.MatchExpressions {
		if expr.Key == osdDeviceClassLabel && expr.Operator == metav1.LabelSelectorOpIn {
			result.Insert(expr.Values...)
		}
	}
	return result
}

// nooutTimestampKey returns the per-(failure-domain, class) ConfigMap key recording when noout
// was first set for that class's drain on that failure domain. Keying by class (not just by
// failure domain) means a second class that starts draining the same failure domain gets its own
// maintenance window instead of inheriting the first class's possibly-expired timestamp. The key
// is namespaced under device-class/ so the fallback handoff cleans it up with the other per-class
// keys.
func nooutTimestampKey(failureDomain, class string) string {
	return fmt.Sprintf("%s%s/%s-noout-last-set-at", deviceClassKeyPrefix, class, failureDomain)
}

// updateNooutForClasses sets the noout flag on the union of all per-class draining failure
// domains (those whose class requested noout and is still within its maintenance window), and
// unsets it everywhere else. This replaces the global updateNoout's "unset on every failure
// domain that is not the single draining one", which would fight itself when several classes
// drain different failure domains. noout on a failure domain stays set while any draining class
// on it is within its own window (noout is a per-bucket flag until per-class noout granularity,
// which is a follow-up).
func (r *ReconcileClusterDisruption) updateNooutForClasses(clusterInfo *cephclient.ClusterInfo, pdbStateMap *corev1.ConfigMap, classStates map[string]*classDrainState) error {
	osdDump, err := cephclient.GetOSDDump(r.context.ClusterdContext, clusterInfo)
	if err != nil {
		return errors.Wrapf(err, "failed to get osddump for reconciling maintenance noout in namespace %s", clusterInfo.Namespace)
	}

	nooutFDs := map[string]bool{}
	allFDs := sets.New[string]()
	activeTimestampKeys := sets.New[string]()
	for _, class := range sortedClasses(classStates) {
		allFDs.Insert(classStates[class].allFailureDomains...)
		drainingFD := pdbStateMap.Data[classConfigKey(drainingFailureDomainKey, class)]
		if drainingFD == "" || pdbStateMap.Data[classConfigKey(setNoOut, class)] != "true" {
			continue
		}
		timeStampKey := nooutTimestampKey(drainingFD, class)
		activeTimestampKeys.Insert(timeStampKey)
		if pdbStateMap.Data[timeStampKey] == "" {
			pdbStateMap.Data[timeStampKey] = time.Now().Format(time.RFC3339)
		}
		nooutSetTime, err := time.Parse(time.RFC3339, pdbStateMap.Data[timeStampKey])
		if err != nil {
			return errors.Wrapf(err, "failed to parse noout timestamp %q for class %q", pdbStateMap.Data[timeStampKey], class)
		}
		// noout stays set for this class's failure domain only while within the window
		if time.Since(nooutSetTime) < r.maintenanceTimeout {
			nooutFDs[drainingFD] = true
		}
	}

	for _, failureDomainName := range sets.List(allFDs) {
		if _, err := osdDump.UpdateFlagOnCrushUnit(r.context.ClusterdContext, clusterInfo, nooutFDs[failureDomainName], failureDomainName, nooutFlag); err != nil {
			return errors.Wrapf(err, "failed to update noout on crush unit %q", failureDomainName)
		}
	}

	// drop timestamps for (failure-domain, class) pairs that are no longer draining
	for key := range pdbStateMap.Data {
		if strings.HasSuffix(key, "-noout-last-set-at") && strings.HasPrefix(key, deviceClassKeyPrefix) && !activeTimestampKeys.Has(key) {
			delete(pdbStateMap.Data, key)
		}
	}
	return nil
}

// setPDBConfigForClass is the per-class analog of setPDBConfig.
func setPDBConfigForClass(pdbStateMap *corev1.ConfigMap, class string, osdDownFailureDomains, nodeDrainFailureDomains []string) {
	drainingFDKey := classConfigKey(drainingFailureDomainKey, class)
	noOutKey := classConfigKey(setNoOut, class)
	durationKey := classConfigKey(drainingFailureDomainDurationKey, class)

	if len(pdbStateMap.Data[drainingFDKey]) == 0 {
		if len(nodeDrainFailureDomains) > 0 {
			pdbStateMap.Data[drainingFDKey] = nodeDrainFailureDomains[0]
			pdbStateMap.Data[noOutKey] = "true"
		} else if len(osdDownFailureDomains) > 0 {
			pdbStateMap.Data[drainingFDKey] = osdDownFailureDomains[0]
			pdbStateMap.Data[noOutKey] = ""
		}
		pdbStateMap.Data[durationKey] = time.Now().Format(time.RFC3339)
	} else {
		if len(nodeDrainFailureDomains) > 0 && !slices.Contains(nodeDrainFailureDomains, pdbStateMap.Data[drainingFDKey]) {
			pdbStateMap.Data[drainingFDKey] = nodeDrainFailureDomains[0]
			pdbStateMap.Data[noOutKey] = "true"
		} else if len(osdDownFailureDomains) > 0 && !slices.Contains(osdDownFailureDomains, pdbStateMap.Data[drainingFDKey]) {
			pdbStateMap.Data[drainingFDKey] = osdDownFailureDomains[0]
			pdbStateMap.Data[noOutKey] = ""
		}
		pdbStateMap.Data[durationKey] = time.Now().Format(time.RFC3339)
	}
}

// resetPDBConfigForClass is the per-class analog of resetPDBConfig. The keys are deleted so an
// absent key cleanly means "this class has no active drain".
func resetPDBConfigForClass(pdbStateMap *corev1.ConfigMap, class string) {
	delete(pdbStateMap.Data, classConfigKey(drainingFailureDomainKey, class))
	delete(pdbStateMap.Data, classConfigKey(setNoOut, class))
	delete(pdbStateMap.Data, classConfigKey(drainingFailureDomainDurationKey, class))
}

// sortedClasses returns the device classes in deterministic order for stable iteration.
func sortedClasses(classStates map[string]*classDrainState) []string {
	classes := make([]string, 0, len(classStates))
	for class := range classStates {
		classes = append(classes, class)
	}
	slices.Sort(classes)
	return classes
}

// cephNotReadyResult maps a Ceph query error to the same requeue behavior reconcilePDBsForOSDs
// uses when the cluster is not yet ready.
func cephNotReadyResult(request reconcile.Request, err error) (reconcile.Result, error) {
	if strings.Contains(err.Error(), opcontroller.UninitializedCephConfigError) {
		logger.Debugf("ceph %q cluster not ready, cannot check status yet.", request.Namespace)
		return opcontroller.WaitForRequeueIfOperatorNotInitialized, nil
	}
	logger.Debugf("ceph %q cluster failed to check cluster health. %v", request.Namespace, err)
	return opcontroller.WaitForRequeueIfCephClusterNotReady, nil
}
