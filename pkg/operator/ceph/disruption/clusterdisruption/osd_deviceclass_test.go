/*
Copyright 2025 The Rook Authors. All rights reserved.

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
	"sort"
	"testing"
	"time"

	"github.com/pkg/errors"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	"github.com/rook/rook/pkg/client/clientset/versioned/scheme"
	"github.com/rook/rook/pkg/clusterd"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	opcontroller "github.com/rook/rook/pkg/operator/ceph/controller"
	"github.com/rook/rook/pkg/operator/ceph/disruption/controllerconfig"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// fakeClassedOSD builds an OSD deployment carrying the device-class and host
// topology labels the class-aware path reads. host is used both as the
// topology-location-host failure domain and as the node/metadata hostname.
func fakeClassedOSD(id, readyReplicas int, deviceClass, host string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("rook-ceph-osd-%d", id),
			Namespace: namespace,
			Labels: map[string]string{
				"app":                    "rook-ceph-osd",
				"device-class":           deviceClass,
				"topology-location-host": host,
				"ceph-osd-id":            fmt.Sprintf("%d", id),
			},
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: int32(readyReplicas), // nolint:gosec // G115 small test value
		},
	}
}

func osdPodLabels(deviceClass, host string, id int) map[string]string {
	l := map[string]string{
		"app":                    "rook-ceph-osd",
		"topology-location-host": host,
		"ceph-osd-id":            fmt.Sprintf("%d", id),
	}
	if deviceClass != "" {
		l["device-class"] = deviceClass
	}
	return l
}

// deviceClassExecutor mocks the ceph commands the class-aware reconcile issues.
// uncleanPools lists pools whose `pg ls-by-pool` returns a non-clean PG.
func deviceClassExecutor(osdMetadata string, uncleanPools map[string]bool) (*exectest.MockExecutor, *[]string) {
	setGroupCalls := &[]string{}
	executor := &exectest.MockExecutor{}
	executor.MockExecuteCommandWithOutput = func(command string, args ...string) (string, error) {
		switch {
		case args[0] == "status":
			// global-group IsClusterClean (fallback path); class groups use pg ls-by-pool
			return healthyCephStatus, nil
		case args[0] == "osd" && args[1] == "metadata":
			return osdMetadata, nil
		case args[0] == "osd" && args[1] == "dump":
			return `{"OSDs":[]}`, nil
		case args[0] == "pg" && args[1] == "ls-by-pool":
			pool := args[2]
			if uncleanPools[pool] {
				return `{"pg_stats":[{"state":"active+clean"},{"state":"active+recovering"}]}`, nil
			}
			return `{"pg_stats":[{"state":"active+clean"},{"state":"active+clean"}]}`, nil
		case args[0] == "osd" && args[1] == "set-group":
			*setGroupCalls = append(*setGroupCalls, args[3]) // crush unit (failure domain)
			return "", nil
		case args[0] == "osd" && args[1] == "unset-group":
			return "", nil
		}
		return "", errors.Errorf("unexpected ceph command '%v'", args)
	}
	return executor, setGroupCalls
}

func twoClassLayout() *cephclient.DeviceClassPDBLayout {
	return &cephclient.DeviceClassPDBLayout{
		Classes: map[string]*cephclient.DeviceClassInfo{
			"ssd": {FailureDomainType: "host", Pools: []string{"ssd-pool"}},
			"hdd": {FailureDomainType: "host", Pools: []string{"hdd-pool"}},
		},
	}
}

func pdbNames(t *testing.T, r *ReconcileClusterDisruption) []string {
	list := &policyv1.PodDisruptionBudgetList{}
	assert.NoError(t, r.client.List(context.TODO(), list))
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	sort.Strings(names)
	return names
}

func getPDB(r *ReconcileClusterDisruption, name string) *policyv1.PodDisruptionBudget {
	pdb := &policyv1.PodDisruptionBudget{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: namespace}, pdb)
	if err != nil {
		return nil
	}
	return pdb
}

// countMatchingPDBs returns how many of the namespace's PDBs select a pod with
// the given labels — the eviction-time count that must be exactly 1 in steady state.
func countMatchingPDBs(t *testing.T, r *ReconcileClusterDisruption, podLabels map[string]string) int {
	list := &policyv1.PodDisruptionBudgetList{}
	assert.NoError(t, r.client.List(context.TODO(), list))
	count := 0
	for i := range list.Items {
		sel, err := metav1.LabelSelectorAsSelector(list.Items[i].Spec.Selector)
		assert.NoError(t, err)
		if sel.Matches(labels.Set(podLabels)) {
			count++
		}
	}
	return count
}

func newClassAwareReconciler(t *testing.T, executor *exectest.MockExecutor, objs ...runtime.Object) *ReconcileClusterDisruption {
	r := getFakeReconciler(t, objs...)
	r.context = &controllerconfig.Context{ClusterdContext: &clusterd.Context{Executor: executor}, OpManagerContext: context.TODO()}
	r.maintenanceTimeout = 30 * time.Minute
	return r
}

func runClassAware(t *testing.T, r *ReconcileClusterDisruption, cm *corev1.ConfigMap, layout *cephclient.DeviceClassPDBLayout) reconcile.Result {
	clusterInfo := getFakeClusterInfo()
	clusterInfo.Context = context.TODO()
	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace}}

	var groups []*pdbGroup
	for _, class := range sortedClasses(layout) {
		info := layout.Classes[class]
		groups = append(groups, newPDBGroup(class, info.FailureDomainType, info.Pools))
	}
	assert.NoError(t, r.populateOSDFailureDomains(clusterInfo, request, groups))

	result, err := r.reconcilePDBsForOSDs(clusterInfo, request, cm, groups, "")
	assert.NoError(t, err)
	return result
}

// runFallback drives the single global-group path (the fallback) through the
// unified reconcile, the way reconcile() does when a cluster is not class-eligible.
func runFallback(t *testing.T, r *ReconcileClusterDisruption, cm *corev1.ConfigMap, failureDomainType string) reconcile.Result {
	clusterInfo := getFakeClusterInfo()
	clusterInfo.Context = context.TODO()
	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace}}

	groups := []*pdbGroup{newPDBGroup("", failureDomainType, nil)}
	assert.NoError(t, r.populateOSDFailureDomains(clusterInfo, request, groups))

	result, err := r.reconcilePDBsForOSDs(clusterInfo, request, cm, groups, "")
	assert.NoError(t, err)
	return result
}

func TestClassAwareIdle(t *testing.T) {
	// Two classes, all OSDs up, all PGs clean: catch-all + one default per class.
	cm := fakePDBConfigMap("")
	osds := []runtime.Object{
		fakeClassedOSD(0, 1, "ssd", "node-a"),
		fakeClassedOSD(1, 1, "hdd", "node-b"),
	}
	executor, _ := deviceClassExecutor(`[{"id":0,"hostname":"node-a"},{"id":1,"hostname":"node-b"}]`, nil)
	r := newClassAwareReconciler(t, executor, append(osds, cephCluster, cm,
		getNodeObject("node-a", false), getNodeObject("node-b", false))...)

	result := runClassAware(t, r, cm, twoClassLayout())
	assert.Zero(t, result.RequeueAfter)

	assert.Equal(t, []string{"rook-ceph-osd", "rook-ceph-osd-hdd", "rook-ceph-osd-ssd"}, pdbNames(t, r))

	// catch-all carries a device-class NotIn clause covering both classes
	catchAll := getPDB(r, "rook-ceph-osd")
	assert.True(t, pdbHasDeviceClassSelector(catchAll))
	assert.Equal(t, int32(1), catchAll.Spec.MaxUnavailable.IntVal)

	// every OSD pod (and a classless one) is matched by exactly one PDB
	assert.Equal(t, 1, countMatchingPDBs(t, r, osdPodLabels("ssd", "node-a", 0)))
	assert.Equal(t, 1, countMatchingPDBs(t, r, osdPodLabels("hdd", "node-b", 1)))
	assert.Equal(t, 1, countMatchingPDBs(t, r, osdPodLabels("", "node-c", 9)))

	// no class is draining
	for k, v := range cm.Data {
		assert.False(t, k == dcDrainingKey("ssd") && v != "", "ssd should not be draining")
		assert.False(t, k == dcDrainingKey("hdd") && v != "", "hdd should not be draining")
	}
}

func TestClassAwareOneClassDraining(t *testing.T) {
	// ssd node-a1 is drained (osd down, node unschedulable) with unclean ssd PGs.
	// hdd stays healthy. ssd gets a blocking PDB for its other failure domain and
	// no per-class default; hdd keeps its default.
	cm := fakePDBConfigMap("")
	osds := []runtime.Object{
		fakeClassedOSD(0, 0, "ssd", "node-a1"), // down + drained
		fakeClassedOSD(1, 1, "ssd", "node-a2"),
		fakeClassedOSD(2, 1, "hdd", "node-b"),
	}
	executor, setGroup := deviceClassExecutor(
		`[{"id":0,"hostname":"node-a1"},{"id":1,"hostname":"node-a2"},{"id":2,"hostname":"node-b"}]`,
		map[string]bool{"ssd-pool": true})
	r := newClassAwareReconciler(t, executor, append(osds, cephCluster, cm,
		getNodeObject("node-a1", true), getNodeObject("node-a2", false), getNodeObject("node-b", false))...)

	result := runClassAware(t, r, cm, twoClassLayout())
	assert.NotZero(t, result.RequeueAfter)

	assert.Equal(t, []string{
		"rook-ceph-osd",
		"rook-ceph-osd-hdd",
		"rook-ceph-osd-ssd-host-node-a2",
	}, pdbNames(t, r))

	// ssd recorded as draining node-a1 with noout set
	assert.Equal(t, "node-a1", cm.Data[dcDrainingKey("ssd")])
	assert.Equal(t, "true", cm.Data[dcSetNoOutKey("ssd")])
	assert.Equal(t, "", cm.Data[dcDrainingKey("hdd")])
	assert.Contains(t, *setGroup, "node-a1") // noout set on the drained failure domain

	// the draining ssd OSD on node-a1 matches no PDB (free to evict)
	assert.Equal(t, 0, countMatchingPDBs(t, r, osdPodLabels("ssd", "node-a1", 0)))
	// the non-draining ssd OSD is blocked by exactly one PDB
	assert.Equal(t, 1, countMatchingPDBs(t, r, osdPodLabels("ssd", "node-a2", 1)))
	// hdd is unaffected
	assert.Equal(t, 1, countMatchingPDBs(t, r, osdPodLabels("hdd", "node-b", 2)))
}

func TestClassAwareTwoClassesDrainingDifferentDomains(t *testing.T) {
	// ssd drains node-a1, hdd drains node-b1 concurrently. Each class gets its own
	// blocking PDB for its surviving domain; neither blocks the other.
	cm := fakePDBConfigMap("")
	osds := []runtime.Object{
		fakeClassedOSD(0, 0, "ssd", "node-a1"),
		fakeClassedOSD(1, 1, "ssd", "node-a2"),
		fakeClassedOSD(2, 0, "hdd", "node-b1"),
		fakeClassedOSD(3, 1, "hdd", "node-b2"),
	}
	executor, _ := deviceClassExecutor(
		`[{"id":0,"hostname":"node-a1"},{"id":1,"hostname":"node-a2"},{"id":2,"hostname":"node-b1"},{"id":3,"hostname":"node-b2"}]`,
		map[string]bool{"ssd-pool": true, "hdd-pool": true})
	r := newClassAwareReconciler(t, executor, append(osds, cephCluster, cm,
		getNodeObject("node-a1", true), getNodeObject("node-a2", false),
		getNodeObject("node-b1", true), getNodeObject("node-b2", false))...)

	runClassAware(t, r, cm, twoClassLayout())

	assert.Equal(t, []string{
		"rook-ceph-osd",
		"rook-ceph-osd-hdd-host-node-b2",
		"rook-ceph-osd-ssd-host-node-a2",
	}, pdbNames(t, r))
	assert.Equal(t, "node-a1", cm.Data[dcDrainingKey("ssd")])
	assert.Equal(t, "node-b1", cm.Data[dcDrainingKey("hdd")])
}

func TestClassAwareTwoClassesDrainingSameDomain(t *testing.T) {
	// Both classes share node-x and drain it. noout is set once (union) and each
	// class keeps a blocking PDB for the shared surviving domain node-y.
	cm := fakePDBConfigMap("")
	osds := []runtime.Object{
		fakeClassedOSD(0, 0, "ssd", "node-x"),
		fakeClassedOSD(1, 1, "ssd", "node-y"),
		fakeClassedOSD(2, 0, "hdd", "node-x"),
		fakeClassedOSD(3, 1, "hdd", "node-y"),
	}
	executor, setGroup := deviceClassExecutor(
		`[{"id":0,"hostname":"node-x"},{"id":1,"hostname":"node-y"},{"id":2,"hostname":"node-x"},{"id":3,"hostname":"node-y"}]`,
		map[string]bool{"ssd-pool": true, "hdd-pool": true})
	r := newClassAwareReconciler(t, executor, append(osds, cephCluster, cm,
		getNodeObject("node-x", true), getNodeObject("node-y", false))...)

	runClassAware(t, r, cm, twoClassLayout())

	assert.Equal(t, []string{
		"rook-ceph-osd",
		"rook-ceph-osd-hdd-host-node-y",
		"rook-ceph-osd-ssd-host-node-y",
	}, pdbNames(t, r))
	assert.Equal(t, "node-x", cm.Data[dcDrainingKey("ssd")])
	assert.Equal(t, "node-x", cm.Data[dcDrainingKey("hdd")])

	// noout set exactly once on the shared node-x bucket (no per-class fighting)
	nodeXCount := 0
	for _, fd := range *setGroup {
		if fd == "node-x" {
			nodeXCount++
		}
	}
	assert.Equal(t, 1, nodeXCount)
}

func TestClassAwareIndependentCompletion(t *testing.T) {
	// ssd finished draining (up + clean) while hdd is still draining node-b1.
	// ssd returns to its per-class default without waiting for hdd.
	cm := fakePDBConfigMap("")
	cm.Data[dcDrainingKey("ssd")] = "node-a1"
	cm.Data[dcSetNoOutKey("ssd")] = "true"
	cm.Data[dcDrainingKey("hdd")] = "node-b1"
	cm.Data[dcSetNoOutKey("hdd")] = "true"
	osds := []runtime.Object{
		fakeClassedOSD(0, 1, "ssd", "node-a1"), // back up
		fakeClassedOSD(1, 1, "ssd", "node-a2"),
		fakeClassedOSD(2, 0, "hdd", "node-b1"), // still draining
		fakeClassedOSD(3, 1, "hdd", "node-b2"),
	}
	executor, _ := deviceClassExecutor(
		`[{"id":0,"hostname":"node-a1"},{"id":1,"hostname":"node-a2"},{"id":2,"hostname":"node-b1"},{"id":3,"hostname":"node-b2"}]`,
		map[string]bool{"hdd-pool": true}) // only hdd still unclean
	r := newClassAwareReconciler(t, executor, append(osds, cephCluster, cm,
		getNodeObject("node-a1", false), getNodeObject("node-a2", false),
		getNodeObject("node-b1", true), getNodeObject("node-b2", false))...)

	runClassAware(t, r, cm, twoClassLayout())

	assert.Equal(t, []string{
		"rook-ceph-osd",
		"rook-ceph-osd-hdd-host-node-b2",
		"rook-ceph-osd-ssd",
	}, pdbNames(t, r))
	assert.Equal(t, "", cm.Data[dcDrainingKey("ssd")], "ssd should have completed")
	assert.Equal(t, "node-b1", cm.Data[dcDrainingKey("hdd")], "hdd should still be draining")
}

func TestClassAwareApplyBeforePruneOnDrainStart(t *testing.T) {
	// Start from the settled idle state (per-class defaults present), then begin a
	// drain. The ssd per-class default must be replaced by blocking PDBs, and the
	// end state never leaves the surviving ssd OSD uncovered.
	cm := fakePDBConfigMap("")
	// pre-seed the idle per-class defaults and catch-all
	preexisting := []runtime.Object{
		defaultOSDPDB(namespace, "rook-ceph-osd", nil, []string{"hdd", "ssd"}, nil),
		defaultOSDPDB(namespace, "rook-ceph-osd-ssd", []string{"ssd"}, nil, nil),
		defaultOSDPDB(namespace, "rook-ceph-osd-hdd", []string{"hdd"}, nil, nil),
	}
	osds := []runtime.Object{
		fakeClassedOSD(0, 0, "ssd", "node-a1"),
		fakeClassedOSD(1, 1, "ssd", "node-a2"),
		fakeClassedOSD(2, 1, "hdd", "node-b"),
	}
	executor, _ := deviceClassExecutor(
		`[{"id":0,"hostname":"node-a1"},{"id":1,"hostname":"node-a2"},{"id":2,"hostname":"node-b"}]`,
		map[string]bool{"ssd-pool": true})
	allObjs := append(osds, cephCluster, cm,
		getNodeObject("node-a1", true), getNodeObject("node-a2", false), getNodeObject("node-b", false))
	allObjs = append(allObjs, preexisting...)
	r := newClassAwareReconciler(t, executor, allObjs...)

	runClassAware(t, r, cm, twoClassLayout())

	assert.Equal(t, []string{
		"rook-ceph-osd",
		"rook-ceph-osd-hdd",
		"rook-ceph-osd-ssd-host-node-a2",
	}, pdbNames(t, r))
	// the stale ssd per-class default was pruned
	assert.Nil(t, getPDB(r, "rook-ceph-osd-ssd"))
	// the surviving ssd OSD remains covered by exactly one PDB throughout
	assert.Equal(t, 1, countMatchingPDBs(t, r, osdPodLabels("ssd", "node-a2", 1)))
}

func TestClassAwareUpgradeIdleAdoptsDefault(t *testing.T) {
	// An old plain rook-ceph-osd default and an old classless blocking PDB exist.
	// On an idle class-aware reconcile the default is overwritten to the catch-all
	// and the classless blocking PDB is pruned.
	cm := fakePDBConfigMap("")
	oldDefault := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "rook-ceph-osd", Namespace: namespace},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &intstr.IntOrString{IntVal: 1},
			Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"rook-ceph-osd"}},
			}},
		},
	}
	oldBlocking := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "rook-ceph-osd-host-node-a", Namespace: namespace},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &intstr.IntOrString{IntVal: 0},
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"topology-location-host": "node-a"}},
		},
	}
	osds := []runtime.Object{
		fakeClassedOSD(0, 1, "ssd", "node-a"),
		fakeClassedOSD(1, 1, "hdd", "node-b"),
	}
	executor, _ := deviceClassExecutor(`[{"id":0,"hostname":"node-a"},{"id":1,"hostname":"node-b"}]`, nil)
	r := newClassAwareReconciler(t, executor, append(osds, cephCluster, cm, oldDefault, oldBlocking,
		getNodeObject("node-a", false), getNodeObject("node-b", false))...)

	runClassAware(t, r, cm, twoClassLayout())

	assert.Equal(t, []string{"rook-ceph-osd", "rook-ceph-osd-hdd", "rook-ceph-osd-ssd"}, pdbNames(t, r))
	// rook-ceph-osd now carries the catch-all device-class NotIn clause
	assert.True(t, pdbHasDeviceClassSelector(getPDB(r, "rook-ceph-osd")))
}

func TestClassAwareUpgradeMidDrain(t *testing.T) {
	// Mid-drain upgrade: an old classless blocking PDB is present while ssd node-a1
	// is being drained. The class-aware path recomputes the drain from live state,
	// prunes the old classless PDB, and keeps the surviving OSDs covered.
	// The pre-upgrade global drain keys recorded for the same drain must be cleared.
	cm := fakePDBConfigMap("node-a1")
	cm.Data[setNoOut] = "true"
	cm.Data[drainingFailureDomainDurationKey] = "2026-01-01T00:00:00Z"
	cm.Data["node-a1-noout-last-set-at"] = "2026-01-01T00:00:00Z"
	oldBlocking := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "rook-ceph-osd-host-node-a2", Namespace: namespace},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &intstr.IntOrString{IntVal: 0},
			Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"topology-location-host": "node-a2"}},
		},
	}
	osds := []runtime.Object{
		fakeClassedOSD(0, 0, "ssd", "node-a1"),
		fakeClassedOSD(1, 1, "ssd", "node-a2"),
		fakeClassedOSD(2, 1, "hdd", "node-b"),
	}
	executor, _ := deviceClassExecutor(
		`[{"id":0,"hostname":"node-a1"},{"id":1,"hostname":"node-a2"},{"id":2,"hostname":"node-b"}]`,
		map[string]bool{"ssd-pool": true})
	r := newClassAwareReconciler(t, executor, append(osds, cephCluster, cm, oldBlocking,
		getNodeObject("node-a1", true), getNodeObject("node-a2", false), getNodeObject("node-b", false))...)

	runClassAware(t, r, cm, twoClassLayout())

	assert.Equal(t, []string{
		"rook-ceph-osd",
		"rook-ceph-osd-hdd",
		"rook-ceph-osd-ssd-host-node-a2",
	}, pdbNames(t, r))
	// the old classless blocking PDB is gone
	assert.Nil(t, getPDB(r, "rook-ceph-osd-host-node-a2"))
	assert.Equal(t, "node-a1", cm.Data[dcDrainingKey("ssd")])
	// stale global drain keys are cleared so a later fallback cannot resume them
	for _, k := range []string{drainingFailureDomainKey, setNoOut, drainingFailureDomainDurationKey, "node-a1-noout-last-set-at"} {
		assert.NotContains(t, cm.Data, k)
	}
}

func countMatchInSet(t *testing.T, pdbs []policyv1.PodDisruptionBudget, podLabels map[string]string) int {
	count := 0
	for i := range pdbs {
		sel, err := metav1.LabelSelectorAsSelector(pdbs[i].Spec.Selector)
		assert.NoError(t, err)
		if sel.Matches(labels.Set(podLabels)) {
			count++
		}
	}
	return count
}

func TestClassAwareNoZeroCoverageOnEligibilityTransition(t *testing.T) {
	// Transition fallback -> class-aware: the live rook-ceph-osd is the plain default
	// covering all OSDs and is narrowed in place to the NotIn catch-all. An
	// interceptor asserts that after EVERY PDB write, a classed OSD pod still matches
	// at least one PDB — i.e. the catch-all is never narrowed before the per-class
	// defaults that replace its coverage exist (the apply-catch-all-last fix).
	cm := fakePDBConfigMap("")
	plainDefault := defaultOSDPDB(namespace, osdPDBAppName, nil, nil, nil) // covers all OSDs (fallback state)
	osds := []runtime.Object{
		fakeClassedOSD(0, 1, "hdd", "node-a"),
		fakeClassedOSD(1, 1, "ssd", "node-b"),
	}
	executor, _ := deviceClassExecutor(`[{"id":0,"hostname":"node-a"},{"id":1,"hostname":"node-b"}]`, nil)

	hddPod := osdPodLabels("hdd", "node-a", 0)
	ssdPod := osdPodLabels("ssd", "node-b", 1)

	// On each PDB write, check coverage of the post-write set (current store overlaid
	// with the object being written).
	checkCoverage := func(ctx context.Context, c client.WithWatch, obj client.Object) {
		pdb, ok := obj.(*policyv1.PodDisruptionBudget)
		if !ok {
			return
		}
		list := &policyv1.PodDisruptionBudgetList{}
		assert.NoError(t, c.List(ctx, list, client.InNamespace(namespace)))
		set := map[string]policyv1.PodDisruptionBudget{}
		for i := range list.Items {
			set[list.Items[i].Name] = list.Items[i]
		}
		set[pdb.Name] = *pdb
		pdbs := make([]policyv1.PodDisruptionBudget, 0, len(set))
		for _, p := range set {
			pdbs = append(pdbs, p)
		}
		assert.GreaterOrEqualf(t, countMatchInSet(t, pdbs, hddPod), 1, "hdd OSD left uncovered after writing %q", pdb.Name)
		assert.GreaterOrEqualf(t, countMatchInSet(t, pdbs, ssdPod), 1, "ssd OSD left uncovered after writing %q", pdb.Name)
	}
	funcs := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			checkCoverage(ctx, c, obj)
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			checkCoverage(ctx, c, obj)
			return c.Update(ctx, obj, opts...)
		},
	}

	s := scheme.Scheme
	assert.NoError(t, policyv1.AddToScheme(s))
	assert.NoError(t, appsv1.AddToScheme(s))
	assert.NoError(t, corev1.AddToScheme(s))
	objs := append(osds, cephCluster, cm, plainDefault,
		getNodeObject("node-a", false), getNodeObject("node-b", false))
	cl := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(objs...).WithInterceptorFuncs(funcs).Build()
	r := &ReconcileClusterDisruption{
		client:             cl,
		scheme:             s,
		clusterMap:         &ClusterMap{clusterMap: map[string]*cephv1.CephCluster{namespace: cephCluster}},
		context:            &controllerconfig.Context{ClusterdContext: &clusterd.Context{Executor: executor}, OpManagerContext: context.TODO()},
		maintenanceTimeout: 30 * time.Minute,
	}

	runClassAware(t, r, cm, twoClassLayout())

	// end state is the catch-all + per-class defaults, each OSD matched by exactly one
	assert.Equal(t, []string{"rook-ceph-osd", "rook-ceph-osd-hdd", "rook-ceph-osd-ssd"}, pdbNames(t, r))
	assert.Equal(t, 1, countMatchingPDBs(t, r, hddPod))
	assert.Equal(t, 1, countMatchingPDBs(t, r, ssdPod))
}

func TestFallbackConvergenceFromClassAware(t *testing.T) {
	// Eligible-to-ineligible transition: the single global-group reconcile prunes
	// the class-scoped PDBs and dc.* keys while (re)establishing the plain
	// rook-ceph-osd default that covers all OSDs (apply before prune).
	cm := fakePDBConfigMap("")
	cm.Data[dcDrainingKey("ssd")] = "node-a1"
	cm.Data[dcSetNoOutKey("ssd")] = "true"
	cm.Data["dc.ssd.node-a1.noout-last-set-at"] = "2026-01-01T00:00:00Z"

	classScopedPDBs := []runtime.Object{
		defaultOSDPDB(namespace, "rook-ceph-osd", nil, []string{"hdd", "ssd"}, nil),
		defaultOSDPDB(namespace, "rook-ceph-osd-hdd", []string{"hdd"}, nil, nil),
		blockingOSDPDB(namespace, "rook-ceph-osd-ssd-host-node-a2", "ssd", "topology-location-host", "node-a2"),
	}
	osds := []runtime.Object{
		fakeClassedOSD(0, 1, "hdd", "node-a"),
		fakeClassedOSD(1, 1, "ssd", "node-b"),
	}
	executor, _ := deviceClassExecutor(`[{"id":0,"hostname":"node-a"},{"id":1,"hostname":"node-b"}]`, nil)
	objs := append(classScopedPDBs, cephCluster, cm,
		getNodeObject("node-a", false), getNodeObject("node-b", false))
	objs = append(objs, osds...)
	r := newClassAwareReconciler(t, executor, objs...)

	runFallback(t, r, cm, "host")

	// only the plain default remains; class-scoped PDBs were pruned
	assert.Equal(t, []string{"rook-ceph-osd"}, pdbNames(t, r))
	// rook-ceph-osd is now the plain default (no device-class selector)
	assert.False(t, pdbHasDeviceClassSelector(getPDB(r, "rook-ceph-osd")))

	// all dc.* keys cleared
	for k := range cm.Data {
		assert.NotContains(t, k, "dc.")
	}
}

func TestClassAwareDegradesWhenFDLabelMissing(t *testing.T) {
	// The ssd class's CRUSH-derived failure domain is "rack", but the OSD deployments are
	// only labeled topology-location-host. The ssd group must degrade to a default-only PDB
	// (covering its OSDs) instead of aborting the whole reconcile; the hdd group (FD host,
	// resolvable) proceeds normally.
	cm := fakePDBConfigMap("")
	layout := &cephclient.DeviceClassPDBLayout{
		Classes: map[string]*cephclient.DeviceClassInfo{
			"ssd": {FailureDomainType: "rack", Pools: []string{"ssd-pool"}},
			"hdd": {FailureDomainType: "host", Pools: []string{"hdd-pool"}},
		},
	}
	osds := []runtime.Object{
		fakeClassedOSD(0, 1, "ssd", "node-a"), // only topology-location-host, no rack
		fakeClassedOSD(1, 1, "hdd", "node-b"),
	}
	executor, _ := deviceClassExecutor(`[{"id":0,"hostname":"node-a"},{"id":1,"hostname":"node-b"}]`, nil)
	r := newClassAwareReconciler(t, executor, append(osds, cephCluster, cm,
		getNodeObject("node-a", false), getNodeObject("node-b", false))...)

	// runClassAware asserts populateOSDFailureDomains and reconcilePDBsForOSDs return no error.
	runClassAware(t, r, cm, layout)

	// the ssd group degraded to its plain per-class default (no blocking PDBs)
	assert.Equal(t, []string{"rook-ceph-osd", "rook-ceph-osd-hdd", "rook-ceph-osd-ssd"}, pdbNames(t, r))
	// the ssd OSD with no rack label is still covered by exactly one PDB
	assert.Equal(t, 1, countMatchingPDBs(t, r, osdPodLabels("ssd", "node-a", 0)))
	// no ssd drain state was written
	assert.Equal(t, "", cm.Data[dcDrainingKey("ssd")])
}

func TestReconcileRequeuesOnCrushReadErrorMidDrain(t *testing.T) {
	// A transient CRUSH read error during an in-flight class-aware drain must requeue and
	// leave the dc.* drain state and class-aware PDBs untouched, never flip to the global
	// group (which would run GC/noout with an empty global group and wipe the dc.* state).
	managedCluster := &cephv1.CephCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "ceph-cluster", Namespace: namespace},
		Spec: cephv1.ClusterSpec{
			DisruptionManagement: cephv1.DisruptionManagementSpec{ManagePodBudgets: true},
		},
	}
	// a pool CR so the poolCount<1 gate does not early-return before the CRUSH read
	pool := &cephv1.CephBlockPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: namespace}}

	// in-flight ssd drain state and the class-aware PDBs that go with it
	cm := fakePDBConfigMap("")
	cm.Data[dcDrainingKey("ssd")] = "node-a1"
	cm.Data[dcSetNoOutKey("ssd")] = "true"
	cm.Data[dcNooutTimestampKey("ssd", "node-a1")] = "2026-01-01T00:00:00Z"
	preexisting := []runtime.Object{
		defaultOSDPDB(namespace, "rook-ceph-osd", nil, []string{"hdd", "ssd"}, nil),
		defaultOSDPDB(namespace, "rook-ceph-osd-hdd", []string{"hdd"}, nil, nil),
		blockingOSDPDB(namespace, "rook-ceph-osd-ssd-host-node-a2", "ssd", "topology-location-host", "node-a2"),
	}
	osds := []runtime.Object{
		fakeClassedOSD(0, 0, "ssd", "node-a1"),
		fakeClassedOSD(1, 1, "ssd", "node-a2"),
		fakeClassedOSD(2, 1, "hdd", "node-b"),
	}

	// executor that fails the crush dump (the first ceph call GetDeviceClassPDBLayout makes)
	executor := &exectest.MockExecutor{}
	executor.MockExecuteCommandWithOutput = func(command string, args ...string) (string, error) {
		if args[0] == "osd" && args[1] == "crush" && args[2] == "dump" {
			return "", errors.New("transient mon read error")
		}
		return "", errors.Errorf("unexpected ceph command '%v'", args)
	}

	objs := append(osds, managedCluster, pool, cm,
		getNodeObject("node-a1", true), getNodeObject("node-a2", false), getNodeObject("node-b", false))
	objs = append(objs, preexisting...)
	r := getFakeReconciler(t, objs...)
	r.clusterMap = &ClusterMap{clusterMap: map[string]*cephv1.CephCluster{namespace: managedCluster}}
	r.context = &controllerconfig.Context{ClusterdContext: &clusterd.Context{Executor: executor}, OpManagerContext: context.TODO()}

	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace}}
	result, err := r.reconcile(request)
	assert.NoError(t, err)
	assert.Equal(t, opcontroller.WaitForRequeueIfCephClusterNotReady, result)

	// dc.* drain state preserved (no GC churn)
	gotCM := &corev1.ConfigMap{}
	assert.NoError(t, r.client.Get(context.TODO(), types.NamespacedName{Name: pdbStateMapName, Namespace: namespace}, gotCM))
	assert.Equal(t, "node-a1", gotCM.Data[dcDrainingKey("ssd")])
	assert.Equal(t, "true", gotCM.Data[dcSetNoOutKey("ssd")])
	assert.Equal(t, "2026-01-01T00:00:00Z", gotCM.Data[dcNooutTimestampKey("ssd", "node-a1")])

	// class-aware PDBs left in place (no prune)
	assert.NotNil(t, getPDB(r, "rook-ceph-osd-ssd-host-node-a2"))
	assert.NotNil(t, getPDB(r, "rook-ceph-osd-hdd"))
	assert.NotNil(t, getPDB(r, "rook-ceph-osd"))
}

func TestComputeDesiredPDBs(t *testing.T) {
	// computeDesiredPDBs is pure over the groups' state and the drain-state ConfigMap, so
	// the groups are built directly with their failure-domain state pre-filled. No ceph mocking.
	ssd := func() *pdbGroup {
		g := newPDBGroup("ssd", "host", []string{"ssd-pool"})
		g.state = groupDrainState{allFailureDomains: []string{"node-a", "node-b"}}
		return g
	}
	hdd := func() *pdbGroup {
		g := newPDBGroup("hdd", "host", []string{"hdd-pool"})
		g.state = groupDrainState{allFailureDomains: []string{"node-c", "node-d"}}
		return g
	}
	global := func() *pdbGroup {
		g := newPDBGroup("", "host", nil)
		g.state = groupDrainState{allFailureDomains: []string{"node-a", "node-b"}}
		return g
	}

	tests := []struct {
		name       string
		groups     []*pdbGroup
		drainState map[string]string
		wantNames  []string
	}{
		{
			name:      "global idle yields the plain default",
			groups:    []*pdbGroup{global()},
			wantNames: []string{"rook-ceph-osd"},
		},
		{
			name:       "global draining yields a blocking pdb for each surviving failure domain",
			groups:     []*pdbGroup{global()},
			drainState: map[string]string{drainingFailureDomainKey: "node-a"},
			wantNames:  []string{"rook-ceph-osd-host-node-b"},
		},
		{
			// the catch-all is derived from the class groups, not passed in
			name:      "class-aware idle yields per-class defaults plus the catch-all",
			groups:    []*pdbGroup{ssd(), hdd()},
			wantNames: []string{"rook-ceph-osd", "rook-ceph-osd-hdd", "rook-ceph-osd-ssd"},
		},
		{
			name:       "one class draining keeps the other class default and the catch-all",
			groups:     []*pdbGroup{ssd(), hdd()},
			drainState: map[string]string{dcDrainingKey("ssd"): "node-a"},
			wantNames:  []string{"rook-ceph-osd", "rook-ceph-osd-hdd", "rook-ceph-osd-ssd-host-node-b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := &corev1.ConfigMap{Data: tt.drainState}
			if cm.Data == nil {
				cm.Data = map[string]string{}
			}
			desired := computeDesiredPDBs(namespace, tt.groups, cm)
			got := make([]string, 0, len(desired))
			for name := range desired {
				got = append(got, name)
			}
			sort.Strings(got)
			assert.Equal(t, tt.wantNames, got)
		})
	}
}
