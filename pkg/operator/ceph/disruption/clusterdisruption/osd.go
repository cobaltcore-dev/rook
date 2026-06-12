/*
Copyright 2019 The Rook Authors. All rights reserved.

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
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	"github.com/rook/rook/pkg/operator/ceph/cluster/osd"
	opcontroller "github.com/rook/rook/pkg/operator/ceph/controller"
	"github.com/rook/rook/pkg/operator/k8sutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	// osdPDBAppName is that app label value for pdbs targeting osds
	osdPDBAppName = "rook-ceph-osd"
	// osdPDBOsdIdLabel is the label on osd pods for pdbs targeting specific osd ids
	osdPDBOsdIdLabel                 = "osd"
	drainingFailureDomainKey         = "draining-failure-domain"
	drainingFailureDomainDurationKey = "draining-failure-domain-duration"
	setNoOut                         = "set-no-out"
	// DefaultMaintenanceTimeout is the period for which a drained failure domain will remain in noout
	DefaultMaintenanceTimeout = 30 * time.Minute
	nooutFlag                 = "noout"
)

// pdbDrainKeys names the ConfigMap keys that hold one group's drain state. The
// cluster-wide group uses the bare baseline keys; a per-class group uses the
// dc.<class>.* keys.
type pdbDrainKeys struct {
	draining string
	setNoOut string
	duration string
}

// globalDrainKeys are the drain-state ConfigMap keys for the cluster-wide group.
var globalDrainKeys = pdbDrainKeys{
	draining: drainingFailureDomainKey,
	setNoOut: setNoOut,
	duration: drainingFailureDomainDurationKey,
}

// groupDrainState is one group's view of OSD failure domains, the group-scoped
// analog of the tuple the baseline computed for the whole cluster.
type groupDrainState struct {
	allFailureDomains       []string
	nodeDrainFailureDomains []string
	osdDownFailureDomains   []string
	downOSDs                []int
}

// pdbGroup is one set of OSDs reconciled together. A cluster gets either a single
// cluster-wide group (deviceClass "") or one group per device class; both are this same
// struct, driven by the same reconcile. A group's PDB naming, ConfigMap keys, and
// PG-health source are all derived from deviceClass (see the methods below), so the
// reconcile never branches on it.
type pdbGroup struct {
	// deviceClass is "" for the cluster-wide group, else the device class this group covers.
	deviceClass string
	// failureDomainType is the CRUSH failure domain type for this group's PDBs.
	failureDomainType string
	// pools are the RADOS pools backing this group's per-class PG-health check; nil for
	// the cluster-wide group, which checks PG health cluster-wide.
	pools []string
	// state is the enumerated failure-domain state, filled by populateOSDFailureDomains.
	state groupDrainState
	// excludeOSDs are down OSDs to exclude from the group's default PDB, computed
	// by updateDrainState.
	excludeOSDs []int
	// degraded is set by populateOSDFailureDomains when this group's failure-domain type
	// does not resolve to a label on one of its OSDs. A degraded group stays idle
	// (default-only PDB, no blocking, no noout, no drain-state writes) so it neither
	// aborts the whole reconcile nor leaves its OSDs uncovered.
	degraded bool
}

// defaultPDB builds this group's maxUnavailable=1 default PDB.
func (g *pdbGroup) defaultPDB(namespace string) *policyv1.PodDisruptionBudget {
	return defaultOSDPDB(namespace, g.defaultPDBName(), g.deviceClassIn(), nil, g.excludeOSDs)
}

// blockingPDB builds this group's maxUnavailable=0 blocking PDB for a failure domain.
func (g *pdbGroup) blockingPDB(namespace, failureDomainName string) *policyv1.PodDisruptionBudget {
	topologyLabel := fmt.Sprintf(osd.TopologyLocationLabel, g.failureDomainType)
	return blockingOSDPDB(namespace, g.blockingPDBName(failureDomainName), g.deviceClass, topologyLabel, failureDomainName)
}

func (r *ReconcileClusterDisruption) createPDB(pdb client.Object) error {
	err := r.client.Create(r.context.OpManagerContext, pdb)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrapf(err, "failed to create pdb %q", pdb.GetName())
	}
	return nil
}

func (r *ReconcileClusterDisruption) deletePDB(pdb client.Object) error {
	err := r.client.Delete(r.context.OpManagerContext, pdb)
	if err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "failed to delete pdb %q", pdb.GetName())
	}
	return nil
}

// applyPDB creates or updates a PDB in place, setting the CephCluster owner
// reference. It is the "apply" half of the apply-then-prune contract.
func (r *ReconcileClusterDisruption) applyPDB(pdb *policyv1.PodDisruptionBudget) error {
	cephCluster, ok := r.clusterMap.GetCluster(pdb.Namespace)
	if !ok {
		return errors.Errorf("failed to find the namespace %q in the clustermap", pdb.Namespace)
	}
	ownerInfo := k8sutil.NewOwnerInfo(cephCluster, r.scheme)
	if err := ownerInfo.SetControllerReference(pdb); err != nil {
		return errors.Wrapf(err, "failed to set owner reference on pdb %q", pdb.Name)
	}

	existing := &policyv1.PodDisruptionBudget{}
	err := r.client.Get(r.context.OpManagerContext, types.NamespacedName{Name: pdb.Name, Namespace: pdb.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		logger.Infof("creating osd pdb %q", pdb.Name)
		return r.createPDB(pdb)
	}
	if err != nil {
		return errors.Wrapf(err, "failed to get pdb %q", pdb.Name)
	}
	existing.Spec = pdb.Spec
	return r.client.Update(r.context.OpManagerContext, existing)
}

// defaultOSDPDB builds a maxUnavailable=1 OSD PDB. deviceClassIn scopes it to a device
// class; deviceClassNotIn scopes it to OSDs outside the given classes; excludeOSDs drops
// specific OSD ids from the selector.
func defaultOSDPDB(namespace, name string, deviceClassIn, deviceClassNotIn []string, excludeOSDs []int) *policyv1.PodDisruptionBudget {
	matchExpressions := []metav1.LabelSelectorRequirement{
		{
			Key:      k8sutil.AppAttr,
			Operator: metav1.LabelSelectorOpIn,
			Values:   []string{osdPDBAppName},
		},
	}
	if len(deviceClassIn) > 0 {
		matchExpressions = append(matchExpressions, metav1.LabelSelectorRequirement{
			Key:      osd.DeviceClassLabelKey,
			Operator: metav1.LabelSelectorOpIn,
			Values:   deviceClassIn,
		})
	}
	if len(deviceClassNotIn) > 0 {
		matchExpressions = append(matchExpressions, metav1.LabelSelectorRequirement{
			Key:      osd.DeviceClassLabelKey,
			Operator: metav1.LabelSelectorOpNotIn,
			Values:   deviceClassNotIn,
		})
	}
	if len(excludeOSDs) > 0 {
		values := make([]string, len(excludeOSDs))
		for i, id := range excludeOSDs {
			values[i] = strconv.Itoa(id)
		}
		matchExpressions = append(matchExpressions, metav1.LabelSelectorRequirement{
			Key:      osdPDBOsdIdLabel,
			Operator: metav1.LabelSelectorOpNotIn,
			Values:   values,
		})
	}
	maxUnavailable := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       &metav1.LabelSelector{MatchExpressions: matchExpressions},
		},
	}
}

// blockingOSDPDB builds a maxUnavailable=0 blocking PDB for one non-draining
// failure domain. A non-empty deviceClass adds the per-class "In" clause; the
// cluster-wide group passes "" and gets the baseline topology-only selector.
func blockingOSDPDB(namespace, name, deviceClass, topologyLabel, failureDomainName string) *policyv1.PodDisruptionBudget {
	maxUnavailable := intstr.FromInt32(0)
	selector := &metav1.LabelSelector{
		MatchLabels: map[string]string{topologyLabel: failureDomainName},
	}
	if deviceClass != "" {
		selector.MatchExpressions = []metav1.LabelSelectorRequirement{
			{
				Key:      osd.DeviceClassLabelKey,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{deviceClass},
			},
		}
	}
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       selector,
		},
	}
}

func getPDBName(failureDomainType, failureDomainName string) string {
	return k8sutil.TruncateNodeName(fmt.Sprintf("%s-%s-%s", osdPDBAppName, failureDomainType, "%s"), failureDomainName)
}

// listOSDPDBs returns every PDB targeting OSDs — rook-ceph-osd and rook-ceph-osd-* (the
// defaults and blocking PDBs) — and not the rgw/mds PDBs.
func (r *ReconcileClusterDisruption) listOSDPDBs(namespace string) ([]policyv1.PodDisruptionBudget, error) {
	pdbList := &policyv1.PodDisruptionBudgetList{}
	if err := r.client.List(r.context.OpManagerContext, pdbList, client.InNamespace(namespace)); err != nil {
		return nil, errors.Wrap(err, "failed to list pod disruption budgets")
	}
	osdPDBs := make([]policyv1.PodDisruptionBudget, 0, len(pdbList.Items))
	for i := range pdbList.Items {
		name := pdbList.Items[i].Name
		if name == osdPDBAppName || strings.HasPrefix(name, osdPDBAppName+"-") {
			osdPDBs = append(osdPDBs, pdbList.Items[i])
		}
	}
	return osdPDBs, nil
}

// pdbHasDeviceClassSelector reports whether a PDB carries a device-class selector
// clause, the marker that distinguishes class-scoped OSD PDBs from a classless one.
func pdbHasDeviceClassSelector(pdb *policyv1.PodDisruptionBudget) bool {
	if pdb.Spec.Selector == nil {
		return false
	}
	for _, expr := range pdb.Spec.Selector.MatchExpressions {
		if expr.Key == osd.DeviceClassLabelKey {
			return true
		}
	}
	return false
}

func (r *ReconcileClusterDisruption) initializePDBState(request reconcile.Request) (*corev1.ConfigMap, error) {
	pdbStateMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pdbStateMapName,
			Namespace: request.Namespace,
		},
	}
	pdbStateMapRequest := types.NamespacedName{
		Name:      pdbStateMapName,
		Namespace: request.Namespace,
	}
	err := r.client.Get(r.context.OpManagerContext, pdbStateMapRequest, pdbStateMap)
	if apierrors.IsNotFound(err) {
		// create configmap to track the draining failure domain
		pdbStateMap.Data = map[string]string{drainingFailureDomainKey: "", setNoOut: ""}
		err := r.client.Create(r.context.OpManagerContext, pdbStateMap)
		if err != nil {
			return pdbStateMap, errors.Wrapf(err, "failed to create the PDB state map %q", pdbStateMapRequest)
		}
	} else if err != nil {
		return pdbStateMap, errors.Wrapf(err, "failed to get the pdbStateMap %s", pdbStateMapRequest)
	}
	// A ConfigMap whose drain-state keys were all deleted is stored with no data
	// field and reloads with a nil Data map; guarantee it is writable.
	if pdbStateMap.Data == nil {
		pdbStateMap.Data = map[string]string{}
	}
	return pdbStateMap, nil
}

// reconcilePDBsForOSDs updates each group's drain state, then applies the whole desired
// OSD-PDB set before pruning any extra OSD PDB, so a pod is never matched by zero PDBs.
func (r *ReconcileClusterDisruption) reconcilePDBsForOSDs(
	clusterInfo *cephclient.ClusterInfo,
	request reconcile.Request,
	pdbStateMap *corev1.ConfigMap,
	groups []*pdbGroup,
	pgHealthyRegex string,
) (reconcile.Result, error) {
	namespace := clusterInfo.Namespace

	// When some group is scoped to a device class, computeDesiredPDBs also emits the
	// rook-ceph-osd default (NotIn those classes) for OSDs of no managed class.
	hasClassGroups := false
	for _, g := range groups {
		if g.deviceClass != "" {
			hasClassGroups = true
			break
		}
	}

	// per-group drain-state update
	for _, g := range groups {
		if g.degraded {
			// FD unresolvable on some OSD: keep the group idle so its default PDB
			// covers all its OSDs. Clear any prior drain state so computeDesiredPDBs
			// takes the idle branch and updateNoout/requeue treat it as quiescent.
			resetPDBConfig(pdbStateMap, g.keys())
			g.excludeOSDs = nil
			continue
		}
		pgHealthMsg, pgClean, err := r.groupPGsClean(clusterInfo, g, pgHealthyRegex)
		if err != nil {
			// If the error contains that message, this means the cluster is not up and running
			// No monitors are present and thus no ceph configuration has been created
			if strings.Contains(err.Error(), opcontroller.UninitializedCephConfigError) {
				logger.Debugf("ceph %q cluster not ready, cannot check status yet.", request.Namespace)
				return opcontroller.WaitForRequeueIfOperatorNotInitialized, nil
			}
			logger.Debugf("ceph %q cluster failed to check cluster health. %v", request.Namespace, err)
			return opcontroller.WaitForRequeueIfCephClusterNotReady, nil
		}
		if err := r.updateDrainState(pdbStateMap, g, pgClean, pgHealthMsg); err != nil {
			return reconcile.Result{}, err
		}
	}

	desired := computeDesiredPDBs(namespace, groups, pdbStateMap)

	// Apply every desired PDB before pruning, so a pod is never matched by zero PDBs.
	// The rook-ceph-osd default is applied LAST: its NotIn selector grows when a class
	// appears, narrowing coverage, so the per-class defaults that take it over must exist
	// first. Other selectors only ever add coverage, so their order does not matter.
	for name := range desired {
		if hasClassGroups && name == osdPDBAppName {
			continue
		}
		if err := r.applyPDB(desired[name]); err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to apply osd pdb %q", name)
		}
	}
	if hasClassGroups {
		if err := r.applyPDB(desired[osdPDBAppName]); err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to apply osd pdb %q", osdPDBAppName)
		}
	}

	// prune existing OSD PDBs not in the desired set
	existing, err := r.listOSDPDBs(namespace)
	if err != nil {
		return reconcile.Result{}, err
	}
	for i := range existing {
		if _, ok := desired[existing[i].Name]; ok {
			continue
		}
		logger.Infof("pruning osd pdb %q not in the desired set", existing[i].Name)
		if err := r.deletePDB(&existing[i]); err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to prune osd pdb %q", existing[i].Name)
		}
	}

	// reconcile noout across the union of all groups' draining failure domains
	if err := r.updateNoout(clusterInfo, pdbStateMap, groups); err != nil {
		logger.Errorf("failed to update maintenance noout in cluster %q. %v", request, err)
	}

	// drop drain-state keys of classes no longer present (a cluster-wide group clears
	// all dc.* keys, so its ConfigMap state matches the baseline)
	gcStaleDrainKeys(pdbStateMap, groups)

	if err := r.client.Update(clusterInfo.Context, pdbStateMap); err != nil {
		if errors.Is(err, context.Canceled) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, errors.Wrapf(err, "failed to update configMap %q in cluster %q", pdbStateMapName, request)
	}

	return r.requeuePDBController(pdbStateMap, groups), nil
}

// computeDesiredPDBs returns the OSD PDBs every group wants this reconcile, keyed by
// name. Pure over the groups' filled state and the drain-state ConfigMap, so it is
// unit tested directly.
func computeDesiredPDBs(namespace string, groups []*pdbGroup, pdbStateMap *corev1.ConfigMap) map[string]*policyv1.PodDisruptionBudget {
	desired := map[string]*policyv1.PodDisruptionBudget{}
	var classes []string
	for _, g := range groups {
		if g.deviceClass != "" {
			classes = append(classes, g.deviceClass)
		}
		drainingFD := pdbStateMap.Data[g.keys().draining]
		if drainingFD == "" {
			// idle group: one maxUnavailable=1 default lets a single failure domain drain
			desired[g.defaultPDBName()] = g.defaultPDB(namespace)
			continue
		}
		// draining group: pin every failure domain except the draining one with a maxUnavailable=0 PDB
		for _, fd := range g.state.allFailureDomains {
			if fd == drainingFD {
				continue
			}
			desired[g.blockingPDBName(fd)] = g.blockingPDB(namespace, fd)
		}
	}
	// With device-class groups present, the rook-ceph-osd default (NotIn those classes)
	// protects OSDs that no class group selects.
	if len(classes) > 0 {
		slices.Sort(classes)
		desired[osdPDBAppName] = defaultOSDPDB(namespace, osdPDBAppName, nil, classes, nil)
	}
	return desired
}

// updateDrainState updates a single group's drain-state keys from its PG health
// and failure-domain state, and records the OSDs to exclude from its default PDB.
func (r *ReconcileClusterDisruption) updateDrainState(pdbStateMap *corev1.ConfigMap, g *pdbGroup, pgClean bool, pgHealthMsg string) error {
	osdDown := len(g.state.downOSDs) > 0
	// OSDs to exclude from the group default PDB: when there are no active drains and PGs are
	// clean but some OSDs are down, exclude them so drains elsewhere are not blocked while they recover.
	g.excludeOSDs = make([]int, 0)

	name := g.defaultPDBName()
	keys := g.keys()
	switch {
	case !osdDown && pgClean:
		logger.Infof("group %q: OSDs are up and PGs are clean. PG status: %q", name, pgHealthMsg)
		resetPDBConfig(pdbStateMap, keys)
	case osdDown && pgClean:
		logger.Infof("group %q: OSD(s) %v are down but PGs are clean. PG Status: %q", name, g.state.downOSDs, pgHealthMsg)
		// In case of a node drain event, the OSD pods can get drained rapidly and it would take some time for rook to fetch
		// the correct PG status. So wait for 60 seconds when OSD is down and node drain event is detected
		if len(g.state.nodeDrainFailureDomains) > 0 {
			lastNodeDrainTimeStamp, err := getLastNodeDrainTimeStamp(pdbStateMap, keys.duration)
			if err != nil {
				return errors.Wrapf(err, "failed to get last node drain timestamp for group %q", name)
			}
			if time.Since(lastNodeDrainTimeStamp) < 60*time.Second {
				logger.Infof("group %q: node drain is detected. Requeue to ensure that correct PG status is read.", name)
			} else {
				g.excludeOSDs = slices.Clone(g.state.downOSDs)
			}
		} else {
			g.excludeOSDs = slices.Clone(g.state.downOSDs)
			resetPDBConfig(pdbStateMap, keys)
		}
	case osdDown && !pgClean:
		logger.Infof("group %q: OSD(s) %v are down and PGs are not clean. PGs Status: %q", name, g.state.downOSDs, pgHealthMsg)
		setPDBConfig(pdbStateMap, keys, g.state.osdDownFailureDomains, g.state.nodeDrainFailureDomains)
	case !osdDown && !pgClean && pdbStateMap.Data[keys.draining] != "":
		logger.Infof("group %q: OSDs are up but PGs are not clean from previous drain event. PGs Status: %q", name, pgHealthMsg)
	}
	return nil
}

// resetPDBConfig clears a group's drain-state keys (absent reads as "" everywhere,
// confirmed by findReferences on drainingFailureDomainKey).
func resetPDBConfig(pdbStateMap *corev1.ConfigMap, keys pdbDrainKeys) {
	delete(pdbStateMap.Data, keys.draining)
	delete(pdbStateMap.Data, keys.setNoOut)
	delete(pdbStateMap.Data, keys.duration)
}

// setPDBConfig records the draining failure domain and noout intent for one group.
// If there are unschedulable nodes (a node drain) those failure domains take
// precedence over failure domains where OSDs are merely down; noout is set only
// for node drains.
func setPDBConfig(pdbStateMap *corev1.ConfigMap, keys pdbDrainKeys, osdDownFailureDomains, nodeDrainFailureDomains []string) {
	if pdbStateMap.Data[keys.draining] == "" {
		if len(nodeDrainFailureDomains) > 0 {
			pdbStateMap.Data[keys.draining] = nodeDrainFailureDomains[0]
			pdbStateMap.Data[keys.setNoOut] = "true"
		} else if len(osdDownFailureDomains) > 0 {
			pdbStateMap.Data[keys.draining] = osdDownFailureDomains[0]
			pdbStateMap.Data[keys.setNoOut] = ""
		}
		pdbStateMap.Data[keys.duration] = time.Now().Format(time.RFC3339)
	} else {
		// Update if the previously drained failure domain is back but another is down.
		if len(nodeDrainFailureDomains) > 0 && !slices.Contains(nodeDrainFailureDomains, pdbStateMap.Data[keys.draining]) {
			pdbStateMap.Data[keys.draining] = nodeDrainFailureDomains[0]
			pdbStateMap.Data[keys.setNoOut] = "true"
		} else if len(osdDownFailureDomains) > 0 && !slices.Contains(osdDownFailureDomains, pdbStateMap.Data[keys.draining]) {
			pdbStateMap.Data[keys.draining] = osdDownFailureDomains[0]
			pdbStateMap.Data[keys.setNoOut] = ""
		}
		pdbStateMap.Data[keys.duration] = time.Now().Format(time.RFC3339)
	}
}

// updateNoout sets noout on the union of all groups' draining failure domains
// (subject to the maintenance timeout) and unsets it everywhere else. The union
// avoids two groups that share a failure-domain bucket name fighting over the flag.
func (r *ReconcileClusterDisruption) updateNoout(clusterInfo *cephclient.ClusterInfo, pdbStateMap *corev1.ConfigMap, groups []*pdbGroup) error {
	osdDump, err := cephclient.GetOSDDump(r.context.ClusterdContext, clusterInfo)
	if err != nil {
		return errors.Wrapf(err, "failed to get osddump for reconciling maintenance noout in namespace %s", clusterInfo.Namespace)
	}

	nooutOn := map[string]bool{}
	allFailureDomains := sets.New[string]()
	for _, g := range groups {
		keys := g.keys()
		draining := pdbStateMap.Data[keys.draining]
		holdNoout := pdbStateMap.Data[keys.setNoOut] == "true"
		for _, failureDomainName := range g.state.allFailureDomains {
			allFailureDomains.Insert(failureDomainName)
			timestampKey := g.nooutTimestampKey(failureDomainName)
			if failureDomainName == draining && holdNoout {
				if pdbStateMap.Data[timestampKey] == "" {
					pdbStateMap.Data[timestampKey] = time.Now().Format(time.RFC3339)
				}
				nooutSetTime, err := time.Parse(time.RFC3339, pdbStateMap.Data[timestampKey])
				if err != nil {
					return errors.Wrapf(err, "failed to parse noout timestamp %q for failure domain %q", pdbStateMap.Data[timestampKey], failureDomainName)
				}
				if time.Since(nooutSetTime) < r.maintenanceTimeout {
					nooutOn[failureDomainName] = true
				}
			} else {
				delete(pdbStateMap.Data, timestampKey)
			}
		}
	}

	for _, failureDomainName := range sets.List(allFailureDomains) {
		if _, err := osdDump.UpdateFlagOnCrushUnit(r.context.ClusterdContext, clusterInfo, nooutOn[failureDomainName], failureDomainName, nooutFlag); err != nil {
			return errors.Wrapf(err, "failed to update noout flag on crush unit %q", failureDomainName)
		}
	}
	return nil
}

// gcStaleDrainKeys deletes drain-state keys owned by groups that are not active:
// dc.<class>.* keys for classes not among the current groups (a cluster-wide group
// clears every dc.* key), and when there is no cluster-wide group, the bare keys a
// previous cluster-wide reconcile (including pre-upgrade) left behind. Without the
// latter, switching back to a cluster-wide group could resume a long-gone drain.
func gcStaleDrainKeys(pdbStateMap *corev1.ConfigMap, groups []*pdbGroup) {
	hasGlobal := false
	activePrefixes := make([]string, 0, len(groups))
	for _, g := range groups {
		if g.deviceClass == "" {
			hasGlobal = true
			continue
		}
		activePrefixes = append(activePrefixes, fmt.Sprintf("%s%s.", dcKeyPrefix, g.deviceClass))
	}
	for k := range pdbStateMap.Data {
		if strings.HasPrefix(k, dcKeyPrefix) {
			keep := false
			for _, prefix := range activePrefixes {
				if strings.HasPrefix(k, prefix) {
					keep = true
					break
				}
			}
			if !keep {
				delete(pdbStateMap.Data, k)
			}
			continue
		}
		if hasGlobal {
			continue
		}
		// global keys: draining-failure-domain, set-no-out, the drain timestamp,
		// and the per-failure-domain "<fd>-noout-last-set-at" noout timestamps
		if k == drainingFailureDomainKey || k == setNoOut || k == drainingFailureDomainDurationKey || strings.HasSuffix(k, "-noout-last-set-at") {
			delete(pdbStateMap.Data, k)
		}
	}
}

// requeuePDBController requeues while any group is actively draining or has a down
// OSD. Polling on a down OSD preserves the baseline self-heal: it drives the drain
// state machine forward as PG status changes (e.g. the post-drain 60s settle and
// the transition to a blocking layout) and removes a down OSD's default-PDB
// exclusion once it recovers — an OSD-deployment recovery raises no watch event, so
// without this poll the exclusion could linger. It keys off per-group drain state rather
// than the rook-ceph-osd default's DisruptionsAllowed, which reads 0 when it selects no OSDs.
func (r *ReconcileClusterDisruption) requeuePDBController(pdbStateMap *corev1.ConfigMap, groups []*pdbGroup) reconcile.Result {
	for _, g := range groups {
		if pdbStateMap.Data[g.keys().draining] != "" || len(g.state.downOSDs) > 0 {
			logger.Info("reconciling osd pdb controller, a failure domain is draining or an OSD is down")
			return reconcile.Result{Requeue: true, RequeueAfter: 30 * time.Second}
		}
	}
	logger.Info("successfully reconciled OSD PDB controller")
	return reconcile.Result{}
}

// populateOSDFailureDomains buckets OSD deployments into their group and fills each
// group's failure-domain state. A cluster-wide group matches every OSD; per-class
// groups match by the device-class label. An OSD that no group matches is left for
// the rook-ceph-osd default.
func (r *ReconcileClusterDisruption) populateOSDFailureDomains(clusterInfo *cephclient.ClusterInfo, request reconcile.Request, groups []*pdbGroup) error {
	osdDeploymentList := &appsv1.DeploymentList{}
	namespaceListOpts := client.InNamespace(request.Namespace)
	if err := r.client.List(clusterInfo.Context, osdDeploymentList, client.MatchingLabels{k8sutil.AppAttr: osd.AppName}, namespaceListOpts); err != nil {
		return errors.Wrap(err, "failed to list osd deployments")
	}

	osdMetadata, err := cephclient.GetOSDMetadata(r.context.ClusterdContext, clusterInfo)
	if err != nil {
		return errors.Wrapf(err, "failed to get OSD status")
	}

	type fdSets struct {
		all       sets.Set[string]
		nodeDrain sets.Set[string]
		osdDown   sets.Set[string]
		downOSDs  []int
		degraded  bool
	}
	fdByClass := make(map[string]*fdSets, len(groups))
	for _, g := range groups {
		fdByClass[g.deviceClass] = &fdSets{all: sets.New[string](), nodeDrain: sets.New[string](), osdDown: sets.New[string](), downOSDs: []int{}}
	}

	for i := range osdDeploymentList.Items {
		deployment := osdDeploymentList.Items[i]
		labels := deployment.GetLabels()
		g := matchGroup(groups, labels[osd.DeviceClassLabelKey])
		if g == nil {
			// no group matches this OSD's class: the rook-ceph-osd default covers it
			continue
		}
		c := fdByClass[g.deviceClass]
		topologyLocationLabel := fmt.Sprintf(osd.TopologyLocationLabel, g.failureDomainType)
		failureDomainName := labels[topologyLocationLabel]
		if failureDomainName == "" {
			// The group's failure-domain type does not resolve to a label on this OSD.
			// Degrade the group to default-only rather than abort the whole reconcile
			// (which would block every other group); its maxUnavailable=1 default still
			// covers all its OSDs.
			logger.Warningf("OSD deployment %q has no %q label; degrading group %q to a default-only PDB", deployment.Name, topologyLocationLabel, g.defaultPDBName())
			c.degraded = true
			continue
		}
		c.all.Insert(failureDomainName)

		// Assume node drain if osd deployment ReadyReplicas count is 0 and OSD pod is not scheduled on a node
		if deployment.Status.ReadyReplicas < 1 {
			c.osdDown.Insert(failureDomainName)

			osdID, err := osd.GetOSDID(&deployment)
			if err != nil {
				return errors.Wrapf(err, "failed to get ID for the OSD deployment %q", deployment.Name)
			}
			c.downOSDs = append(c.downOSDs, osdID)

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
				return errors.Wrapf(err, "failed to check if osd %q node is drained", deployment.Name)
			}
			if isDrained {
				logger.Infof("osd %q is down on node %q and a possible node drain is detected", deployment.Name, osdNodeName)
				c.nodeDrain.Insert(failureDomainName)
			} else if !strings.HasSuffix(deployment.Name, "-debug") {
				logger.Infof("osd %q is down on node %q but no node drain is detected", deployment.Name, osdNodeName)
			}
		}
	}

	for _, g := range groups {
		c := fdByClass[g.deviceClass]
		if c.degraded {
			// Force the group idle: empty state means computeDesiredPDBs emits only the
			// maxUnavailable=1 default, and updateNoout/requeue see no failure domains.
			g.degraded = true
			g.state = groupDrainState{}
			continue
		}
		g.state = groupDrainState{
			allFailureDomains:       sets.List(c.all),
			nodeDrainFailureDomains: sets.List(c.nodeDrain),
			osdDownFailureDomains:   sets.List(c.osdDown),
			downOSDs:                c.downOSDs,
		}
	}
	return nil
}

// matchGroup returns the group an OSD with the given device class belongs to. A
// cluster-wide group (deviceClass "") matches every OSD; a class group matches only its
// class. Returns nil for an OSD that no group matches (left for the rook-ceph-osd default).
func matchGroup(groups []*pdbGroup, deviceClass string) *pdbGroup {
	var clusterWide *pdbGroup
	for _, g := range groups {
		if g.deviceClass == "" {
			clusterWide = g
			continue
		}
		if g.deviceClass == deviceClass {
			return g
		}
	}
	return clusterWide
}

// hasOSDNodeDrained returns true if OSD pod is not assigned to any node or if the OSD node is not schedulable
func hasOSDNodeDrained(ctx context.Context, c client.Client, osdNodeName string) (bool, error) {
	node, err := getNode(ctx, c, osdNodeName)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, errors.Wrapf(err, "failed to get node %q", osdNodeName)
	}
	return node.Spec.Unschedulable, nil
}

func getNode(ctx context.Context, c client.Client, nodeName string) (*corev1.Node, error) {
	node := &corev1.Node{}
	err := c.Get(ctx, types.NamespacedName{Name: nodeName}, node)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get node %q", nodeName)
	}
	return node, nil
}

func getLastNodeDrainTimeStamp(pdbStateMap *corev1.ConfigMap, key string) (time.Time, error) {
	var err error
	var lastDrainTimeStamp time.Time
	lastDrainTimeStampString, ok := pdbStateMap.Data[key]
	if !ok || len(lastDrainTimeStampString) == 0 {
		currentTimeStamp := time.Now()
		pdbStateMap.Data[key] = currentTimeStamp.Format(time.RFC3339)
		return currentTimeStamp, nil
	} else {
		lastDrainTimeStamp, err = time.Parse(time.RFC3339, pdbStateMap.Data[key])
		if err != nil {
			return time.Time{}, errors.Wrapf(err, "failed to parse timestamp %q", pdbStateMap.Data[key])
		}
	}
	return lastDrainTimeStamp, nil
}
