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
	"sync"
	"time"

	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"

	"github.com/coreos/pkg/capnslog"
	cephClient "github.com/rook/rook/pkg/daemon/ceph/client"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	opcontroller "github.com/rook/rook/pkg/operator/ceph/controller"
	"github.com/rook/rook/pkg/operator/ceph/disruption/controllerconfig"
	"github.com/rook/rook/pkg/operator/k8sutil"
)

const (
	controllerName = "clusterdisruption-controller"
	// pdbStateMapName for the clusterdisruption pdb state map
	pdbStateMapName        = "rook-ceph-pdbstatemap"
	legacyDrainCanaryLabel = "rook-ceph-drain-canary"
)

var (
	logger = capnslog.NewPackageLogger("github.com/rook/rook", "disruption")

	// Implement reconcile.Reconciler so the controller can reconcile objects
	_ reconcile.Reconciler = &ReconcileClusterDisruption{}

	// delete legacy drain canary pods and blocking OSD podDisruptionBudgets
	deleteLegacyResources = true
)

// ReconcileClusterDisruption reconciles ReplicaSets
type ReconcileClusterDisruption struct {
	// client can be used to retrieve objects from the APIServer.
	scheme             *runtime.Scheme
	client             client.Client
	context            *controllerconfig.Context
	clusterMap         *ClusterMap
	maintenanceTimeout time.Duration
}

// Reconcile reconciles a node and ensures that it has a drain-detection deployment
// attached to it.
// The Controller will requeue the Request to be processed again if an error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileClusterDisruption) Reconcile(context context.Context, request reconcile.Request) (reconcile.Result, error) {
	defer opcontroller.RecoverAndLogException()
	// wrapping reconcile because the rook logging mechanism is not compatible with the controller-runtime logging interface
	result, err := r.reconcile(request)
	if err != nil {
		logger.Error(err)
	}
	return result, err
}

func (r *ReconcileClusterDisruption) reconcile(request reconcile.Request) (reconcile.Result, error) {
	if request.Namespace == "" {
		return reconcile.Result{}, errors.Errorf("request did not have namespace: %q", request.NamespacedName)
	}

	logger.Debugf("reconciling %q", request.NamespacedName)

	// get the ceph cluster
	cephClusters := &cephv1.CephClusterList{}
	if err := r.client.List(r.context.OpManagerContext, cephClusters, client.InNamespace(request.Namespace)); err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "could not get cephclusters in namespace %q", request.Namespace)
	}
	if len(cephClusters.Items) == 0 {
		logger.Errorf("cephcluster %q seems to be deleted, not requeuing until triggered again", request)
		return reconcile.Result{Requeue: false}, nil
	}

	cephCluster := cephClusters.Items[0]

	// update the clustermap with the cluster's name so that
	// events on resources associated with the cluster can trigger reconciliation by namespace
	r.clusterMap.UpdateClusterMap(request.Namespace, &cephCluster)

	// get the cluster info
	clusterInfo := r.clusterMap.GetClusterInfo(request.Namespace)
	if clusterInfo == nil {
		logger.Infof("clusterName is not known for namespace %q", request.Namespace)
		return reconcile.Result{Requeue: true, RequeueAfter: 5 * time.Second}, errors.New("clusterName for this namespace not yet known")
	}
	clusterInfo.Context = r.context.OpManagerContext

	// ensure that the cluster name is populated
	if request.Name == "" {
		request.Name = clusterInfo.NamespacedName().Name
	}

	if !cephCluster.Spec.DisruptionManagement.ManagePodBudgets {
		// feature disabled for this cluster. not requeueing
		return reconcile.Result{Requeue: false}, nil
	}

	if deleteLegacyResources {
		// delete any legacy node drain canary pods
		err := r.deleteDrainCanaryPods(clusterInfo.Namespace)
		if err != nil {
			return reconcile.Result{}, err
		}
		logger.Info("deleted all legacy node drain canary pods")

		deleteLegacyResources = false
	}

	r.maintenanceTimeout = cephCluster.Spec.DisruptionManagement.OSDMaintenanceTimeout * time.Minute
	if r.maintenanceTimeout == 0 {
		r.maintenanceTimeout = DefaultMaintenanceTimeout
		logger.Debugf("Using default maintenance timeout: %v", r.maintenanceTimeout)
	}

	//  reconcile the pools and get the failure domain
	cephObjectStoreList, cephFilesystemList, poolFailureDomain, poolCount, err := r.processPools(request)
	if err != nil {
		return reconcile.Result{}, err
	}

	// reconcile the pdbs for objectstores
	err = r.reconcileCephObjectStore(cephObjectStoreList)
	if err != nil {
		return reconcile.Result{}, err
	}

	// reconcile the pdbs for filesystems
	err = r.reconcileCephFilesystem(cephFilesystemList)
	if err != nil {
		return reconcile.Result{}, err
	}

	// no pools, no need to reconcile OSD PDB
	if poolCount < 1 {
		return reconcile.Result{}, nil
	}

	// get the map that stores currently draining failure domain
	pdbStateMap, err := r.initializePDBState(request)
	if err != nil {
		return reconcile.Result{}, err
	}

	pgHealthyRegex := cephCluster.Spec.DisruptionManagement.PGHealthyRegex

	// Idle fast path: if no OSD is down and no drain bookkeeping exists, the PDB layout is the
	// same on either path (just the default PDB), so apply it without any Ceph reads. This keeps
	// the common steady state -- and every reconcile on a cluster that never classifies its pools
	// -- off the CRUSH/PG queries, and stops a transient Ceph read failure from blocking PDB
	// management for an otherwise-healthy cluster.
	osdDown, err := r.anyOSDDown(request)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !osdDown && !hasActiveDrainState(pdbStateMap) {
		return r.reconcileIdleOSDPDBs(request)
	}

	// Determine device-class isolation from the live CRUSH map, not from CR specs: a pool whose
	// CRUSH rule spans device classes (including pools with no CR, like the CephNFS .nfs pool)
	// means a class drain can endanger data on another class's OSDs, so the operator must use the
	// global, class-agnostic fallback. Resolving this from CRUSH also makes the isolation decision
	// and the per-class PG cleanliness check share one source of truth.
	classPoolMap, err := cephClient.GetDeviceClassPools(r.context.ClusterdContext, clusterInfo)
	if err != nil {
		return cephNotReadyResult(request, err)
	}
	classIsolated := !classPoolMap.HasSpanningPool && len(classPoolMap.Pools) > 0

	// Use the device-class-aware path only when every pool isolates a device class and no legacy
	// (pre-upgrade) global drain is in progress. A non-empty legacy drainingFailureDomain key means
	// a drain started under the old code; finish it on the class-agnostic path, after which
	// subsequent drains use the class-aware path. See "Mid-drain upgrade" in the design.
	if classIsolated && pdbStateMap.Data[drainingFailureDomainKey] == "" {
		classFailureDomain := map[string]string{}
		for class, fdTypes := range classPoolMap.FailureDomains {
			classFailureDomain[class] = minimumFailureDomainFromTypes(fdTypes)
		}
		classStates, err := r.getOSDFailureDomainsByClass(clusterInfo, request, classFailureDomain)
		if err != nil {
			return reconcile.Result{}, err
		}
		return r.reconcilePDBsForOSDsByClass(clusterInfo, request, pdbStateMap, classStates, classPoolMap.Pools, pgHealthyRegex)
	}

	// Fallback (class-agnostic) path. If isolation was lost while a per-class drain was active
	// (for example an unclassified pool was added), remove any class-scoped PDBs and per-class
	// ConfigMap keys first so they cannot coexist with the fallback PDBs and double-match a pod.
	if err := r.cleanupDeviceClassArtifacts(request.Namespace, pdbStateMap); err != nil {
		return reconcile.Result{}, err
	}

	// get a list of all the failure domains, failure domains with failed OSDs and failure domains with drained nodes
	allFailureDomains, nodeDrainFailureDomains, osdDownFailureDomains, downOSDs, err := r.getOSDFailureDomains(clusterInfo, request, poolFailureDomain)
	if err != nil {
		return reconcile.Result{}, err
	}

	return r.reconcilePDBsForOSDs(clusterInfo, request, pdbStateMap, poolFailureDomain, allFailureDomains, osdDownFailureDomains, nodeDrainFailureDomains, downOSDs, pgHealthyRegex)
}

// ClusterMap maintains the association between namespace and clusername
type ClusterMap struct {
	clusterMap map[string]*cephv1.CephCluster
	mux        sync.Mutex
}

// UpdateClusterMap to populate the clusterName for the namespace
func (c *ClusterMap) UpdateClusterMap(namespace string, cluster *cephv1.CephCluster) {
	defer c.mux.Unlock()
	c.mux.Lock()
	if len(c.clusterMap) == 0 {
		c.clusterMap = make(map[string]*cephv1.CephCluster)
	}
	c.clusterMap[namespace] = cluster
}

// GetClusterInfo looks up the context for the current ceph cluster.
// found is the boolean indicating whether a cluster was populated for that namespace or not.
func (c *ClusterMap) GetClusterInfo(namespace string) *cephClient.ClusterInfo {
	defer c.mux.Unlock()
	c.mux.Lock()

	if len(c.clusterMap) == 0 {
		c.clusterMap = make(map[string]*cephv1.CephCluster)
	}

	cluster, ok := c.clusterMap[namespace]
	if !ok {
		return nil
	}

	clusterInfo := cephClient.NewClusterInfo(namespace, cluster.ObjectMeta.GetName())
	clusterInfo.CephCred.Username = cephClient.AdminUsername
	return clusterInfo
}

// GetCluster returns vars cluster, found. cluster is the cephcluster associated
// with that namespace and found is the boolean indicating whether a cluster was
// populated for that namespace or not.
func (c *ClusterMap) GetCluster(namespace string) (*cephv1.CephCluster, bool) {
	defer c.mux.Unlock()
	c.mux.Lock()

	if len(c.clusterMap) == 0 {
		c.clusterMap = make(map[string]*cephv1.CephCluster)
	}

	cluster, ok := c.clusterMap[namespace]
	if !ok {
		return nil, false
	}

	return cluster, true
}

// GetClusterNamespaces returns the internal clustermap for iteration purposes
func (c *ClusterMap) GetClusterNamespaces() []string {
	defer c.mux.Unlock()
	c.mux.Lock()
	var namespaces []string
	for _, cluster := range c.clusterMap {
		namespaces = append(namespaces, cluster.Namespace)
	}
	return namespaces
}

func (r *ReconcileClusterDisruption) deleteDrainCanaryPods(namespace string) error {
	err := r.client.DeleteAllOf(r.context.OpManagerContext, &appsv1.Deployment{}, client.InNamespace(namespace),
		client.MatchingLabels{k8sutil.AppAttr: legacyDrainCanaryLabel})
	if err != nil && !kerrors.IsNotFound(err) {
		return errors.Wrapf(err, "failed to delete all the legacy drain-canary pods with label %q", legacyDrainCanaryLabel)
	}
	return nil
}
