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
	"fmt"
	"slices"

	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	"github.com/rook/rook/pkg/operator/ceph/cluster/osd/topology"
	"github.com/rook/rook/pkg/operator/k8sutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// dcKeyPrefix marks the per-class ConfigMap keys for bulk cleanup. ConfigMap keys
// must match [-._a-zA-Z0-9]+, so "/" is invalid; the class-aware keys use "." as
// the separator: dc.<class>.<base>.
const dcKeyPrefix = "dc."

func dcDrainingKey(class string) string {
	return fmt.Sprintf("%s%s.%s", dcKeyPrefix, class, drainingFailureDomainKey)
}

func dcSetNoOutKey(class string) string {
	return fmt.Sprintf("%s%s.%s", dcKeyPrefix, class, setNoOut)
}

func dcDurationKey(class string) string {
	return fmt.Sprintf("%s%s.%s", dcKeyPrefix, class, drainingFailureDomainDurationKey)
}

func dcNooutTimestampKey(class, failureDomainName string) string {
	return fmt.Sprintf("%s%s.%s.noout-last-set-at", dcKeyPrefix, class, failureDomainName)
}

// classDrainKeys is the per-class constructor for pdbDrainKeys.
func classDrainKeys(class string) pdbDrainKeys {
	return pdbDrainKeys{
		draining: dcDrainingKey(class),
		setNoOut: dcSetNoOutKey(class),
		duration: dcDurationKey(class),
	}
}

func perClassDefaultPDBName(class string) string {
	return fmt.Sprintf("%s-%s", osdPDBAppName, class)
}

func perClassBlockingPDBName(class, failureDomainType, failureDomainName string) string {
	return k8sutil.TruncateNodeName(fmt.Sprintf("%s-%s-%s-%s", osdPDBAppName, class, failureDomainType, "%s"), failureDomainName)
}

// globalFailureDomain reduces the in-use pools' failure-domain types to the finest one in
// the CRUSH hierarchy — the cluster-wide failure domain for the cluster-wide OSD PDB group.
// Defaults to cephv1.DefaultFailureDomain when no in-use pool resolves a known level.
func globalFailureDomain(layout *cephclient.DeviceClassPDBLayout) string {
	minIndex := -1
	for _, fdType := range layout.FailureDomainTypes {
		for i, level := range topology.CRUSHMapLevelsOrdered {
			if level == fdType {
				if minIndex == -1 || i < minIndex {
					minIndex = i
				}
				break
			}
		}
	}
	if minIndex == -1 {
		return cephv1.DefaultFailureDomain
	}
	return topology.CRUSHMapLevelsOrdered[minIndex]
}

// sortedClasses returns the device class names from the layout in sorted order.
func sortedClasses(layout *cephclient.DeviceClassPDBLayout) []string {
	classes := make([]string, 0, len(layout.Classes))
	for class := range layout.Classes {
		classes = append(classes, class)
	}
	slices.Sort(classes)
	return classes
}

// buildOSDPDBGroups reads the live CRUSH map and returns the OSD PDB groups to reconcile,
// each with its OSD failure-domain state populated: one group per device class when the
// in-use pools partition into more than one device class, otherwise a single cluster-wide
// group. Every error here is a ceph/api read failure the caller treats as "not ready yet".
func (r *ReconcileClusterDisruption) buildOSDPDBGroups(clusterInfo *cephclient.ClusterInfo, request reconcile.Request) ([]*pdbGroup, error) {
	layout, err := cephclient.GetDeviceClassPDBLayout(r.context.ClusterdContext, clusterInfo)
	if err != nil {
		return nil, err
	}

	var groups []*pdbGroup
	classes := sortedClasses(layout)
	if len(classes) <= 1 {
		groups = []*pdbGroup{newPDBGroup("", globalFailureDomain(layout), nil)}
	} else {
		logger.Debugf("using device-class-aware OSD PDB groups for device classes %v", classes)
		groups = make([]*pdbGroup, 0, len(classes))
		for _, class := range classes {
			info := layout.Classes[class]
			groups = append(groups, newPDBGroup(class, info.FailureDomainType, info.Pools))
		}
	}

	if err := r.populateOSDFailureDomains(clusterInfo, request, groups); err != nil {
		return nil, err
	}
	return groups, nil
}

// newPDBGroup builds an OSD-PDB group. An empty deviceClass yields the cluster-wide group;
// a non-empty one yields a device-class group over the given pools.
func newPDBGroup(deviceClass, failureDomainType string, pools []string) *pdbGroup {
	return &pdbGroup{
		deviceClass:       deviceClass,
		failureDomainType: failureDomainType,
		pools:             pools,
	}
}

// The cluster-wide group ("") keeps the bare rook-ceph-osd names, keys, and cluster-wide
// PG health, so existing clusters need no migration; a device-class group uses the
// dc.<class>.* names, keys, and per-class PG health. These derivations are why the
// reconcile never has to branch on deviceClass itself.

func (g *pdbGroup) defaultPDBName() string {
	if g.deviceClass == "" {
		return osdPDBAppName
	}
	return perClassDefaultPDBName(g.deviceClass)
}

// deviceClassIn is the device-class "In" selector clause for the group's default PDB:
// nil for the cluster-wide group, []string{deviceClass} for a device-class group.
func (g *pdbGroup) deviceClassIn() []string {
	if g.deviceClass == "" {
		return nil
	}
	return []string{g.deviceClass}
}

func (g *pdbGroup) keys() pdbDrainKeys {
	if g.deviceClass == "" {
		return globalDrainKeys
	}
	return classDrainKeys(g.deviceClass)
}

func (g *pdbGroup) blockingPDBName(failureDomainName string) string {
	if g.deviceClass == "" {
		return getPDBName(g.failureDomainType, failureDomainName)
	}
	return perClassBlockingPDBName(g.deviceClass, g.failureDomainType, failureDomainName)
}

func (g *pdbGroup) nooutTimestampKey(failureDomainName string) string {
	if g.deviceClass == "" {
		return fmt.Sprintf("%s-noout-last-set-at", failureDomainName)
	}
	return dcNooutTimestampKey(g.deviceClass, failureDomainName)
}

// groupPGsClean reports whether the group's PGs are healthy: cluster-wide for the
// cluster-wide group, scoped to the class's pools for a device-class group.
func (r *ReconcileClusterDisruption) groupPGsClean(clusterInfo *cephclient.ClusterInfo, g *pdbGroup, pgHealthyRegex string) (string, bool, error) {
	if g.deviceClass == "" {
		return cephclient.IsClusterClean(r.context.ClusterdContext, clusterInfo, pgHealthyRegex)
	}
	return cephclient.IsDeviceClassClean(r.context.ClusterdContext, clusterInfo, g.pools, pgHealthyRegex)
}
