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
	"strings"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/operator/ceph/cluster/osd"
	"github.com/rook/rook/pkg/operator/ceph/disruption/controllerconfig"
	"github.com/rook/rook/pkg/operator/k8sutil"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestParseBlockingInClasses(t *testing.T) {
	assert.Empty(t, parseBlockingInClasses(nil))
	assert.Empty(t, parseBlockingInClasses(&policyv1.PodDisruptionBudget{}))

	pdb := &policyv1.PodDisruptionBudget{
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"topology-location-zone": "zone-a"},
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: osdDeviceClassLabel, Operator: metav1.LabelSelectorOpIn, Values: []string{"hdd", "ssd"}},
				},
			},
		},
	}
	assert.True(t, parseBlockingInClasses(pdb).Equal(sets.New("hdd", "ssd")))
}

// podLabels builds the labels an OSD pod of the given class and zone carries.
func podLabels(class, zone string) map[string]string {
	return map[string]string{
		k8sutil.AppAttr:     osdPDBAppName,
		osdDeviceClassLabel: class,
		fmt.Sprintf(osd.TopologyLocationLabel, "zone"): zone,
	}
}

// pdbMatchCount returns how many of the PDBs select the pod with the given labels.
func pdbMatchCount(t *testing.T, pod map[string]string, pdbs []policyv1.PodDisruptionBudget) int {
	count := 0
	set := labels.Set(pod)
	for i := range pdbs {
		selector, err := metav1.LabelSelectorAsSelector(pdbs[i].Spec.Selector)
		require.NoError(t, err)
		if selector.Matches(set) {
			count++
		}
	}
	return count
}

// TestApplyDeviceClassPDBs verifies the doc's Cases 1 through 5: the resulting PDB set never
// double-matches an OSD pod (the HTTP 500 condition) and exposes exactly the pods in the
// draining failure domain of their class.
func TestApplyDeviceClassPDBs(t *testing.T) {
	// cluster: zones a,b,c; classes hdd,ssd; one OSD of each class per zone.
	zones := []string{"zone-a", "zone-b", "zone-c"}
	classes := []string{"hdd", "ssd"}

	blocking := func(pairs map[string][]string) map[pdbKey]sets.Set[string] {
		out := map[pdbKey]sets.Set[string]{}
		for zone, inClasses := range pairs {
			out[pdbKey{failureDomainType: "zone", failureDomainName: zone}] = sets.New(inClasses...)
		}
		return out
	}

	tests := []struct {
		name            string
		drainingClasses []string
		blockingDesired map[pdbKey]sets.Set[string]
		exposed         map[string]bool // "class/zone" -> expected to match zero PDBs
	}{
		{
			name:            "case 1 idle",
			drainingClasses: nil,
			blockingDesired: blocking(nil),
			exposed:         map[string]bool{},
		},
		{
			name:            "case 2 hdd draining zone-a",
			drainingClasses: []string{"hdd"},
			blockingDesired: blocking(map[string][]string{"zone-b": {"hdd"}, "zone-c": {"hdd"}}),
			exposed:         map[string]bool{"hdd/zone-a": true},
		},
		{
			name:            "case 3 hdd zone-a and ssd zone-c",
			drainingClasses: []string{"hdd", "ssd"},
			blockingDesired: blocking(map[string][]string{"zone-a": {"ssd"}, "zone-b": {"hdd", "ssd"}, "zone-c": {"hdd"}}),
			exposed:         map[string]bool{"hdd/zone-a": true, "ssd/zone-c": true},
		},
		{
			name:            "case 4 both classes draining zone-a",
			drainingClasses: []string{"hdd", "ssd"},
			blockingDesired: blocking(map[string][]string{"zone-b": {"hdd", "ssd"}, "zone-c": {"hdd", "ssd"}}),
			exposed:         map[string]bool{"hdd/zone-a": true, "ssd/zone-a": true},
		},
		{
			name:            "case 5 hdd done ssd still draining zone-c",
			drainingClasses: []string{"ssd"},
			blockingDesired: blocking(map[string][]string{"zone-a": {"ssd"}, "zone-b": {"ssd"}}),
			exposed:         map[string]bool{"ssd/zone-c": true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := getFakeReconciler(t, cephCluster)
			r.context = &controllerconfig.Context{OpManagerContext: context.TODO()}

			err := r.applyDeviceClassPDBs(namespace, tc.drainingClasses, tc.blockingDesired, nil)
			assert.NoError(t, err)

			pdbList := &policyv1.PodDisruptionBudgetList{}
			require.NoError(t, r.client.List(context.TODO(), pdbList))

			for _, class := range classes {
				for _, zone := range zones {
					count := pdbMatchCount(t, podLabels(class, zone), pdbList.Items)
					// The invariant: never matched by more than one PDB (HTTP 500 otherwise).
					assert.LessOrEqualf(t, count, 1, "pod %s/%s matched %d PDBs", class, zone, count)
					if tc.exposed[fmt.Sprintf("%s/%s", class, zone)] {
						assert.Equalf(t, 0, count, "pod %s/%s should be exposed for eviction", class, zone)
					} else {
						assert.Equalf(t, 1, count, "pod %s/%s should be protected by exactly one PDB", class, zone)
					}
				}
			}
		})
	}
}

// TestApplyDeviceClassPDBsTransition exercises the Case 3 -> Case 5 transition: the zone-c
// blocking PDB is deleted and the zone-a/zone-b blocking PDBs are narrowed to ssd only.
func TestApplyDeviceClassPDBsTransition(t *testing.T) {
	r := getFakeReconciler(t, cephCluster)
	r.context = &controllerconfig.Context{OpManagerContext: context.TODO()}

	// Establish Case 3.
	case3 := map[pdbKey]sets.Set[string]{
		{failureDomainType: "zone", failureDomainName: "zone-a"}: sets.New("ssd"),
		{failureDomainType: "zone", failureDomainName: "zone-b"}: sets.New("hdd", "ssd"),
		{failureDomainType: "zone", failureDomainName: "zone-c"}: sets.New("hdd"),
	}
	require.NoError(t, r.applyDeviceClassPDBs(namespace, []string{"hdd", "ssd"}, case3, nil))

	// Move to Case 5: hdd finished draining, ssd still draining zone-c.
	case5 := map[pdbKey]sets.Set[string]{
		{failureDomainType: "zone", failureDomainName: "zone-a"}: sets.New("ssd"),
		{failureDomainType: "zone", failureDomainName: "zone-b"}: sets.New("ssd"),
	}
	require.NoError(t, r.applyDeviceClassPDBs(namespace, []string{"ssd"}, case5, nil))

	// zone-c blocking PDB must be gone.
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: getPDBName("zone", "zone-c"), Namespace: namespace}, &policyv1.PodDisruptionBudget{})
	assert.True(t, apierrors.IsNotFound(err), "zone-c blocking pdb should be deleted")

	// zone-a and zone-b narrowed to ssd only.
	for _, zone := range []string{"zone-a", "zone-b"} {
		pdb := &policyv1.PodDisruptionBudget{}
		require.NoError(t, r.client.Get(context.TODO(), types.NamespacedName{Name: getPDBName("zone", zone), Namespace: namespace}, pdb))
		assert.True(t, parseBlockingInClasses(pdb).Equal(sets.New("ssd")), "zone %s should block ssd only", zone)
	}

	// Default PDB excludes only ssd now.
	defaultPDB := &policyv1.PodDisruptionBudget{}
	require.NoError(t, r.client.Get(context.TODO(), types.NamespacedName{Name: osdPDBAppName, Namespace: namespace}, defaultPDB))
	assert.Equal(t, 1, pdbMatchCount(t, podLabels("hdd", "zone-a"), []policyv1.PodDisruptionBudget{*defaultPDB}), "hdd pod should be back under the default PDB")
	assert.Equal(t, 0, pdbMatchCount(t, podLabels("ssd", "zone-a"), []policyv1.PodDisruptionBudget{*defaultPDB}), "ssd pod should be excluded from the default PDB")
}

func fakeClassOSDDeployment(id int, class, zone string, readyReplicas int) appsv1.Deployment {
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("osd-%d", id),
			Namespace: namespace,
			Labels: map[string]string{
				k8sutil.AppAttr:          osdPDBAppName,
				osdDeviceClassLabel:      class,
				"topology-location-zone": zone,
				"ceph-osd-id":            fmt.Sprintf("%d", id),
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: int32(readyReplicas)}, // nolint:gosec
	}
}

func TestGetOSDFailureDomainsByClass(t *testing.T) {
	osds := []appsv1.Deployment{
		fakeClassOSDDeployment(0, "hdd", "zone-a", 0), // down, node drained
		fakeClassOSDDeployment(1, "hdd", "zone-b", 1),
		fakeClassOSDDeployment(2, "ssd", "zone-a", 1),
		fakeClassOSDDeployment(3, "ssd", "zone-c", 1),
	}
	objs := []runtime.Object{cephCluster, &corev1.ConfigMap{}, getNodeObject("node-0", true), getNodeObject("node-1", false), getNodeObject("node-2", false), getNodeObject("node-3", false)}
	for i := range osds {
		objs = append(objs, osds[i].DeepCopy())
	}

	executor := &exectest.MockExecutor{}
	executor.MockExecuteCommandWithOutput = func(command string, args ...string) (string, error) {
		if args[0] == "osd" && args[1] == "metadata" {
			return `[{"id":0,"hostname":"node-0"},{"id":1,"hostname":"node-1"},{"id":2,"hostname":"node-2"},{"id":3,"hostname":"node-3"}]`, nil
		}
		return "", errors.Errorf("unexpected command %v", args)
	}

	r := getFakeReconciler(t, objs...)
	r.context = &controllerconfig.Context{ClusterdContext: &clusterd.Context{Executor: executor}, OpManagerContext: context.TODO()}
	clusterInfo := getFakeClusterInfo()
	clusterInfo.Context = context.TODO()
	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace}}

	states, err := r.getOSDFailureDomainsByClass(clusterInfo, request, map[string]string{"hdd": "zone", "ssd": "zone"})
	assert.NoError(t, err)
	require.Contains(t, states, "hdd")
	require.Contains(t, states, "ssd")
	assert.ElementsMatch(t, []string{"zone-a", "zone-b"}, states["hdd"].allFailureDomains)
	assert.ElementsMatch(t, []string{"zone-a"}, states["hdd"].nodeDrainFailureDomains)
	assert.ElementsMatch(t, []string{"zone-a"}, states["hdd"].osdDownFailureDomains)
	assert.ElementsMatch(t, []int{0}, states["hdd"].downOSDs)
	assert.ElementsMatch(t, []string{"zone-a", "zone-c"}, states["ssd"].allFailureDomains)
	assert.Empty(t, states["ssd"].downOSDs)
}

// deviceClassReconcileExecutor mocks only the Ceph commands reconcilePDBsForOSDsByClass issues:
// `osd metadata` (failure-domain enumeration), `pg ls-by-pool` (per-class cleanliness), `osd dump`
// and set/unset-group (noout). The CRUSH map and pool list are not read here -- the class-to-pool
// map is passed into reconcilePDBsForOSDsByClass directly (GetDeviceClassPools is exercised by the
// client-package tests). hdd-pool reports clean PGs; ssd-pool reports a degraded PG.
func deviceClassReconcileExecutor() *exectest.MockExecutor {
	executor := &exectest.MockExecutor{}
	executor.MockExecuteCommandWithOutput = func(command string, args ...string) (string, error) {
		switch {
		case args[0] == "osd" && args[1] == "metadata":
			return `[{"id":0,"hostname":"node-a"},{"id":1,"hostname":"node-b"},{"id":2,"hostname":"node-c"},{"id":3,"hostname":"node-a"},{"id":4,"hostname":"node-b"},{"id":5,"hostname":"node-c"}]`, nil
		case args[0] == "pg" && args[1] == "ls-by-pool":
			if args[2] == "ssd-pool" {
				return `[{"pgid":"2.0","state":"active+undersized+degraded"}]`, nil
			}
			return `[{"pgid":"1.0","state":"active+clean"}]`, nil
		case args[0] == "osd" && args[1] == "dump":
			return `{"OSDs":[]}`, nil
		case args[0] == "osd" && (args[1] == "set-group" || args[1] == "unset-group"):
			return "", nil
		}
		return "", errors.Errorf("unexpected command %v", args)
	}
	return executor
}

// TestReconcilePDBsForOSDsByClass exercises the headline behavior end to end: both classes had
// been draining, all OSDs are back up, hdd PGs are clean while ssd PGs are still recovering.
// hdd must unblock independently (Case 5) while ssd stays blocked.
func TestReconcilePDBsForOSDsByClass(t *testing.T) {
	osds := []appsv1.Deployment{
		fakeClassOSDDeployment(0, "hdd", "zone-a", 1), fakeClassOSDDeployment(1, "hdd", "zone-b", 1), fakeClassOSDDeployment(2, "hdd", "zone-c", 1),
		fakeClassOSDDeployment(3, "ssd", "zone-a", 1), fakeClassOSDDeployment(4, "ssd", "zone-b", 1), fakeClassOSDDeployment(5, "ssd", "zone-c", 1),
	}
	// Pre-existing state: both hdd (zone-a) and ssd (zone-c) draining.
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: pdbStateMapName, Namespace: namespace},
		Data: map[string]string{
			classConfigKey(drainingFailureDomainKey, "hdd"): "zone-a",
			classConfigKey(setNoOut, "hdd"):                 "true",
			classConfigKey(drainingFailureDomainKey, "ssd"): "zone-c",
			classConfigKey(setNoOut, "ssd"):                 "true",
		},
	}
	objs := []runtime.Object{
		cephCluster, configMap,
		getNodeObject("node-a", false), getNodeObject("node-b", false), getNodeObject("node-c", false),
	}
	for i := range osds {
		objs = append(objs, osds[i].DeepCopy())
	}

	r := getFakeReconciler(t, objs...)
	r.context = &controllerconfig.Context{ClusterdContext: &clusterd.Context{Executor: deviceClassReconcileExecutor()}, OpManagerContext: context.TODO()}
	clusterInfo := getFakeClusterInfo()
	clusterInfo.Context = context.TODO()
	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace}}

	classStates, err := r.getOSDFailureDomainsByClass(clusterInfo, request, map[string]string{"hdd": "zone", "ssd": "zone"})
	require.NoError(t, err)
	classPools := map[string][]string{"hdd": {"hdd-pool"}, "ssd": {"ssd-pool"}}
	_, err = r.reconcilePDBsForOSDsByClass(clusterInfo, request, configMap, classStates, classPools, "")
	require.NoError(t, err)

	// hdd unblocked: its ConfigMap state is cleared; ssd remains draining zone-c.
	cm := &corev1.ConfigMap{}
	require.NoError(t, r.client.Get(context.TODO(), types.NamespacedName{Name: pdbStateMapName, Namespace: namespace}, cm))
	_, hddPresent := cm.Data[classConfigKey(drainingFailureDomainKey, "hdd")]
	assert.False(t, hddPresent, "hdd drain state should be cleared once its PGs are clean")
	assert.Equal(t, "zone-c", cm.Data[classConfigKey(drainingFailureDomainKey, "ssd")], "ssd should stay draining while its PGs recover")

	// Resulting PDBs: default excludes only ssd; blocking PDBs for ssd at zone-a and zone-b; none for hdd.
	pdbList := &policyv1.PodDisruptionBudgetList{}
	require.NoError(t, r.client.List(context.TODO(), pdbList))
	for _, zone := range []string{"zone-a", "zone-b", "zone-c"} {
		for _, class := range []string{"hdd", "ssd"} {
			count := pdbMatchCount(t, podLabels(class, zone), pdbList.Items)
			assert.LessOrEqualf(t, count, 1, "pod %s/%s matched %d PDBs", class, zone, count)
		}
	}
	// hdd pods are all protected by the default PDB again.
	for _, zone := range []string{"zone-a", "zone-b", "zone-c"} {
		assert.Equalf(t, 1, pdbMatchCount(t, podLabels("hdd", zone), pdbList.Items), "hdd/%s should be protected", zone)
	}
	// ssd in zone-c (its draining FD) is exposed; ssd elsewhere is blocked.
	assert.Equal(t, 0, pdbMatchCount(t, podLabels("ssd", "zone-c"), pdbList.Items), "ssd/zone-c should be exposed")
	assert.Equal(t, 1, pdbMatchCount(t, podLabels("ssd", "zone-a"), pdbList.Items), "ssd/zone-a should be blocked")
}

func TestClassConfigKeyHelpers(t *testing.T) {
	cm := &corev1.ConfigMap{Data: map[string]string{}}
	setPDBConfigForClass(cm, "hdd", []string{"zone-a"}, []string{"zone-a"})
	assert.Equal(t, "zone-a", cm.Data[classConfigKey(drainingFailureDomainKey, "hdd")])
	assert.Equal(t, "true", cm.Data[classConfigKey(setNoOut, "hdd")])

	resetPDBConfigForClass(cm, "hdd")
	_, ok := cm.Data[classConfigKey(drainingFailureDomainKey, "hdd")]
	assert.False(t, ok)
}

// TestClassConfigKeyNoCollision guards against a per-class key colliding with a legacy
// (class-agnostic) key. A class literally named "duration" must not produce the legacy
// drainingFailureDomainDurationKey.
func TestClassConfigKeyNoCollision(t *testing.T) {
	assert.NotEqual(t, drainingFailureDomainDurationKey, classConfigKey(drainingFailureDomainKey, "duration"))
	assert.NotEqual(t, drainingFailureDomainKey, classConfigKey(drainingFailureDomainKey, ""))
	assert.True(t, strings.HasPrefix(classConfigKey(drainingFailureDomainKey, "hdd"), deviceClassKeyPrefix))
}

func TestHasActiveDrainState(t *testing.T) {
	assert.False(t, hasActiveDrainState(&corev1.ConfigMap{Data: map[string]string{drainingFailureDomainKey: "", setNoOut: ""}}))
	assert.True(t, hasActiveDrainState(&corev1.ConfigMap{Data: map[string]string{drainingFailureDomainKey: "zone-a"}}), "legacy drain key means active")
	assert.True(t, hasActiveDrainState(&corev1.ConfigMap{Data: map[string]string{classConfigKey(drainingFailureDomainKey, "hdd"): "zone-a"}}), "per-class key means active")
}

// TestReconcileIdleOSDPDBs verifies the idle fast path (N1): the default PDB is (re)created with
// no class clause and any leftover blocking PDB is removed, all without any Ceph call. The
// executor errors on every command, so the test fails if the idle path touches Ceph.
func TestReconcileIdleOSDPDBs(t *testing.T) {
	stray := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: getPDBName("zone", "zone-b"), Namespace: namespace},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"topology-location-zone": "zone-b"}}},
	}
	r := getFakeReconciler(t, cephCluster, stray)
	noCephExecutor := &exectest.MockExecutor{MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
		return "", errors.Errorf("idle path must not call ceph, got %v", args)
	}}
	r.context = &controllerconfig.Context{ClusterdContext: &clusterd.Context{Executor: noCephExecutor}, OpManagerContext: context.TODO()}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace}}

	_, err := r.reconcileIdleOSDPDBs(request)
	assert.NoError(t, err)

	defaultPDB := &policyv1.PodDisruptionBudget{}
	require.NoError(t, r.client.Get(context.TODO(), types.NamespacedName{Name: osdPDBAppName, Namespace: namespace}, defaultPDB))
	assert.Empty(t, parseBlockingInClasses(defaultPDB), "default PDB should carry no device-class clause when idle")
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: getPDBName("zone", "zone-b"), Namespace: namespace}, &policyv1.PodDisruptionBudget{})
	assert.True(t, apierrors.IsNotFound(err), "leftover blocking pdb should be deleted")
}

// TestUpdateNooutForClassesPerClassWindow verifies N4: a class that starts draining a failure
// domain already drained (and expired) by another class still gets its own maintenance window.
func TestUpdateNooutForClassesPerClassWindow(t *testing.T) {
	var setCommands []string
	executor := &exectest.MockExecutor{MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
		if args[0] == "osd" && args[1] == "dump" {
			return `{"OSDs":[]}`, nil
		}
		if args[0] == "osd" && (args[1] == "set-group" || args[1] == "unset-group") {
			setCommands = append(setCommands, strings.Join(args[1:], " "))
			return "", nil
		}
		return "", errors.Errorf("unexpected command %v", args)
	}}
	r := getFakeReconciler(t, cephCluster)
	r.context = &controllerconfig.Context{ClusterdContext: &clusterd.Context{Executor: executor}, OpManagerContext: context.TODO()}
	r.maintenanceTimeout = 30 * time.Minute
	clusterInfo := getFakeClusterInfo()
	clusterInfo.Context = context.TODO()

	// hdd has been draining zone-a long enough to expire; ssd just started draining zone-a.
	cm := &corev1.ConfigMap{Data: map[string]string{
		classConfigKey(drainingFailureDomainKey, "hdd"): "zone-a",
		classConfigKey(setNoOut, "hdd"):                 "true",
		nooutTimestampKey("zone-a", "hdd"):              time.Now().Add(-40 * time.Minute).Format(time.RFC3339),
		classConfigKey(drainingFailureDomainKey, "ssd"): "zone-a",
		classConfigKey(setNoOut, "ssd"):                 "true",
	}}
	classStates := map[string]*classDrainState{
		"hdd": {failureDomainType: "zone", allFailureDomains: []string{"zone-a"}},
		"ssd": {failureDomainType: "zone", allFailureDomains: []string{"zone-a"}},
	}

	require.NoError(t, r.updateNooutForClasses(clusterInfo, cm, classStates))

	// ssd is within its own window, so noout must be SET on zone-a despite hdd's expired timestamp.
	hasCmd := func(prefix string) bool {
		for _, c := range setCommands {
			if strings.HasPrefix(c, prefix) {
				return true
			}
		}
		return false
	}
	assert.True(t, hasCmd("set-group noout zone-a"), "noout should be set on zone-a: %v", setCommands)
	assert.False(t, hasCmd("unset-group noout zone-a"), "noout should not be unset on zone-a: %v", setCommands)
	// ssd got its own timestamp key, independent of hdd's.
	assert.NotEmpty(t, cm.Data[nooutTimestampKey("zone-a", "ssd")])
}

// TestCleanupDeviceClassArtifacts verifies the fallback handoff (R1): class-scoped blocking
// PDBs and per-class ConfigMap keys are removed, while a classless blocking PDB and the legacy
// keys are left intact.
func TestCleanupDeviceClassArtifacts(t *testing.T) {
	classBlocking := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: getPDBName("zone", "zone-a"), Namespace: namespace},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels:      map[string]string{"topology-location-zone": "zone-a"},
				MatchExpressions: []metav1.LabelSelectorRequirement{{Key: osdDeviceClassLabel, Operator: metav1.LabelSelectorOpIn, Values: []string{"hdd"}}},
			},
		},
	}
	classlessBlocking := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: getPDBName("zone", "zone-b"), Namespace: namespace},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"topology-location-zone": "zone-b"}},
		},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: pdbStateMapName, Namespace: namespace},
		Data: map[string]string{
			classConfigKey(drainingFailureDomainKey, "hdd"): "zone-a",
			classConfigKey(setNoOut, "hdd"):                 "true",
			drainingFailureDomainKey:                        "", // legacy key must survive
		},
	}

	r := getFakeReconciler(t, cephCluster, classBlocking, classlessBlocking, cm)
	r.context = &controllerconfig.Context{OpManagerContext: context.TODO()}

	require.NoError(t, r.cleanupDeviceClassArtifacts(namespace, cm))

	// class-scoped blocking PDB deleted, classless one kept
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: getPDBName("zone", "zone-a"), Namespace: namespace}, &policyv1.PodDisruptionBudget{})
	assert.True(t, apierrors.IsNotFound(err), "class-scoped blocking pdb should be deleted")
	assert.NoError(t, r.client.Get(context.TODO(), types.NamespacedName{Name: getPDBName("zone", "zone-b"), Namespace: namespace}, &policyv1.PodDisruptionBudget{}), "classless blocking pdb should be kept")

	// per-class keys cleared, legacy key preserved
	_, hddKey := cm.Data[classConfigKey(drainingFailureDomainKey, "hdd")]
	assert.False(t, hddKey, "per-class keys should be cleared")
	_, legacy := cm.Data[drainingFailureDomainKey]
	assert.True(t, legacy, "legacy key should be preserved")
}
