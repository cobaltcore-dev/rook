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

package client

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"github.com/rook/rook/pkg/clusterd"
)

// DeviceClassInfo describes a device class whose pools are confined to a single
// CRUSH device-class shadow tree.
type DeviceClassInfo struct {
	// FailureDomainType is the CRUSH failure domain type (host, zone, ...) that
	// the class's rules select with their chooseleaf/choose step.
	FailureDomainType string
	// Pools are the RADOS pool names confined to this device class.
	Pools []string
}

// DeviceClassPDBLayout is the per-device-class view of the cluster derived from
// the live CRUSH map joined to the in-use pools. It is the source of truth for
// the class-aware OSD PodDisruptionBudget path: the CRUSH map, not the CRs,
// decides whether each pool keeps its data inside a single device class.
type DeviceClassPDBLayout struct {
	// Classes maps each device class to its pools and failure domain — the device classes
	// the in-use pools partition into. It is populated only when every in-use pool maps to
	// a single device class; if any pool's data spans device classes (or maps to no
	// resolvable failure domain), the pools do not partition by class and Classes is empty.
	Classes map[string]*DeviceClassInfo
	// FailureDomainTypes holds the failure-domain type each in-use pool's CRUSH rule
	// selects. The operator reduces these to the cluster-wide failure domain (the finest
	// in the CRUSH hierarchy) for the cluster-wide OSD PDB group.
	FailureDomainTypes []string
}

// poolLsDetailEntry is the slice of an `osd pool ls detail` entry the layout
// needs: the pool name and the numeric id of the crush rule it references.
type poolLsDetailEntry struct {
	PoolName  string `json:"pool_name"`
	CrushRule int    `json:"crush_rule"`
}

// GetDeviceClassPDBLayout reads the live CRUSH map and pool list and reports the device
// classes the in-use pools partition into (empty unless every pool maps to a single
// device class), plus each in-use pool's failure-domain type for the cluster-wide failure
// domain. This single read backs the eligibility gate, the per-class failure-domain types,
// and the per-class pool mapping so they cannot disagree. The whole read is two ceph calls
// regardless of pool count.
func GetDeviceClassPDBLayout(context *clusterd.Context, clusterInfo *ClusterInfo) (*DeviceClassPDBLayout, error) {
	crushMap, err := GetCrushMap(context, clusterInfo)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get crush map")
	}

	rulesByID := make(map[int]ruleSpec, len(crushMap.Rules))
	for _, rule := range crushMap.Rules {
		rulesByID[rule.ID] = rule
	}

	args := []string{"osd", "pool", "ls", "detail"}
	buf, err := NewCephCommand(context, clusterInfo, args).Run()
	if err != nil {
		return nil, errors.Wrap(err, "failed to list pool details")
	}
	var pools []poolLsDetailEntry
	if err := json.Unmarshal(buf, &pools); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal osd pool ls detail response")
	}

	layout := &DeviceClassPDBLayout{Classes: map[string]*DeviceClassInfo{}}
	poolsPartitionByClass := true
	for _, pool := range pools {
		rule, ok := rulesByID[pool.CrushRule]
		if !ok {
			// The rule the pool references is missing from the crush map. Fail closed:
			// the pool escapes the per-class partition, so protect cluster-wide.
			logger.Warningf("crush rule %d for pool %q not found in crush map; the pools do not partition by device class", pool.CrushRule, pool.PoolName)
			poolsPartitionByClass = false
			continue
		}
		// Collect the outermost selected level of every in-use pool (whether or not it
		// maps to a single class) so the operator can derive the cluster-wide failure domain.
		if fd := ruleFailureDomain(rule); fd != "" {
			layout.FailureDomainTypes = append(layout.FailureDomainTypes, fd)
		}
		class, fdType, confined := ruleDeviceClass(rule)
		if !confined {
			poolsPartitionByClass = false
			continue
		}
		info, ok := layout.Classes[class]
		if !ok {
			info = &DeviceClassInfo{FailureDomainType: fdType}
			layout.Classes[class] = info
		}
		info.Pools = append(info.Pools, pool.PoolName)
	}
	// Per-class protection is valid only if every in-use pool maps to a single device
	// class. If any pool spans classes, the pools do not partition by class: clear the
	// set so the cluster-wide group is used.
	if !poolsPartitionByClass {
		layout.Classes = nil
	}
	return layout, nil
}

// ruleDeviceClass returns the single device class a CRUSH rule confines its data
// to, the failure domain type the rule selects, and whether the rule is confined
// to exactly one device class with a resolvable failure domain. A rule that takes
// a plain root (item_name without a "~class" suffix), spans multiple device
// classes (e.g. a hybrid rule with several take steps), or has no chooseleaf type
// is not confined. The failure domain comes from ruleFailureDomain (outermost-wins).
func ruleDeviceClass(rule ruleSpec) (string, string, bool) {
	classes := map[string]struct{}{}
	plainRoot := false
	for _, step := range rule.Steps {
		if step.Operation == "take" {
			// item_name is "<root>~<class>" for a device-class shadow tree, or a
			// plain "<root>" when the rule spans all classes.
			if idx := strings.LastIndex(step.ItemName, "~"); idx >= 0 {
				classes[step.ItemName[idx+1:]] = struct{}{}
			} else {
				plainRoot = true
			}
		}
	}
	fdType := ruleFailureDomain(rule)
	if plainRoot || len(classes) != 1 || fdType == "" {
		return "", "", false
	}
	var class string
	for c := range classes {
		class = c
	}
	return class, fdType, true
}

// ruleFailureDomain returns the failure-domain type a CRUSH rule selects: the type of its
// outermost choose/chooseleaf step (a two-step rule's inner chooseleaf does not override
// the outer level). Returns "" when the rule selects no level.
func ruleFailureDomain(rule ruleSpec) string {
	fdType := ""
	for _, step := range rule.Steps {
		switch {
		case strings.HasPrefix(step.Operation, "chooseleaf") && step.Type != "" && fdType == "":
			fdType = step.Type
		case strings.HasPrefix(step.Operation, "choose") && step.Type != "" && fdType == "":
			fdType = step.Type
		}
	}
	return fdType
}

type pgLsByPoolResponse struct {
	PgStats []struct {
		State string `json:"state"`
	} `json:"pg_stats"`
}

// IsDeviceClassClean reports whether every PG of the given pools matches
// pgHealthyRegex (the same regex IsClusterClean uses). Unlike the cluster-wide
// IsClusterClean, it scopes the check to one device class's pools so a class that
// has finished rebalancing returns to normal protection without waiting for a
// slower class. An empty pgHealthyRegex uses the default healthy-PG regex.
func IsDeviceClassClean(context *clusterd.Context, clusterInfo *ClusterInfo, pools []string, pgHealthyRegex string) (string, bool, error) {
	regexCompiled := defaultPgHealthyRegexCompiled
	if pgHealthyRegex != "" {
		var err error
		regexCompiled, err = regexp.Compile(pgHealthyRegex)
		if err != nil {
			return "unable to compile pgHealthyRegex", false, err
		}
	}

	totalPGs := 0
	uncleanStates := map[string]int{}
	for _, pool := range pools {
		args := []string{"pg", "ls-by-pool", pool}
		buf, err := NewCephCommand(context, clusterInfo, args).Run()
		if err != nil {
			return "unable to get PG health", false, errors.Wrapf(err, "failed to get pg status for pool %q", pool)
		}
		var resp pgLsByPoolResponse
		if err := json.Unmarshal(buf, &resp); err != nil {
			return "unable to parse PG health", false, errors.Wrapf(err, "failed to unmarshal pg ls-by-pool response for pool %q", pool)
		}
		for _, pg := range resp.PgStats {
			totalPGs++
			if !regexCompiled.MatchString(pg.State) {
				uncleanStates[pg.State]++
			}
		}
	}

	if totalPGs == 0 {
		return "device class has no PGs", true, nil
	}
	if len(uncleanStates) == 0 {
		return "all PGs for device class are clean", true, nil
	}
	return "device class PGs are not clean: " + statesSummary(uncleanStates), false, nil
}

func statesSummary(states map[string]int) string {
	parts := make([]string, 0, len(states))
	for state, count := range states {
		parts = append(parts, state+"="+strconv.Itoa(count))
	}
	return strings.Join(parts, ",")
}
