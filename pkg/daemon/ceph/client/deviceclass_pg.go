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

package client

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/pkg/errors"
	"github.com/rook/rook/pkg/clusterd"
)

// classAndFailureDomainForRule derives, from a CRUSH rule, the single device class it targets
// and its failure-domain type. The device class comes from the `take` step's item_name (for
// example "default~ssd" -> "ssd"); it is "" when the rule does not isolate a single class:
// a plain "default" take (spans all classes) or a hybrid rule with several class-specific takes.
// The failure domain comes from the first step that sets a `type` (the chooseleaf/choose step).
//
// pool.go's extractPoolDetails does a similar parse for the pool-crush-rule-update feature but
// assumes a simple two-step rule; this version is multi-take aware so a hybrid rule is correctly
// reported as not class-isolated.
func classAndFailureDomainForRule(rule ruleSpec) (deviceClass, failureDomain string) {
	classes := map[string]struct{}{}
	for _, step := range rule.Steps {
		if failureDomain == "" && step.Type != "" {
			failureDomain = step.Type
		}
		if step.Operation != "take" {
			continue
		}
		_, class, found := strings.Cut(step.ItemName, "~")
		if found && class != "" {
			classes[class] = struct{}{}
		} else {
			// a take on the whole root means the rule is not class-isolated
			classes[""] = struct{}{}
		}
	}
	if len(classes) == 1 {
		for class := range classes {
			deviceClass = class
		}
	}
	return deviceClass, failureDomain
}

// DeviceClassPoolMap describes, from the live CRUSH map, how RADOS pools map to device classes.
// It is the single source of truth for both the device-class PDB isolation decision and the
// per-class PG cleanliness check, so the two can never disagree.
type DeviceClassPoolMap struct {
	// Pools maps each device class to the RADOS pools whose CRUSH rule isolates that class.
	Pools map[string][]string
	// FailureDomains maps each device class to the CRUSH failure-domain types its pools use
	// (one entry per pool; the caller reduces these to a single minimum failure domain).
	FailureDomains map[string][]string
	// HasSpanningPool is true if any pool's CRUSH rule spans device classes (or its rule could
	// not be resolved). When true the cluster is not class-isolated and the operator must use
	// the global, class-agnostic fallback behavior.
	HasSpanningPool bool
}

// poolCrushRule is the subset of `osd pool ls detail` we need: the pool name and its CRUSH rule id.
type poolCrushRule struct {
	Name   string `json:"pool_name"`
	RuleID int    `json:"crush_rule"`
}

// GetDeviceClassPools resolves the device-class-to-pool mapping entirely from Ceph (the CRUSH
// map joined to the pool list), so it does not depend on CR specs or on translating CR pool
// names into RADOS pool names. It reads two bounded queries (`osd crush dump` and
// `osd pool ls detail`) regardless of pool count.
func GetDeviceClassPools(context *clusterd.Context, clusterInfo *ClusterInfo) (*DeviceClassPoolMap, error) {
	crushMap, err := GetCrushMap(context, clusterInfo)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get crush map for device class pool mapping")
	}
	type ruleInfo struct {
		deviceClass   string
		failureDomain string
	}
	rules := map[int]ruleInfo{}
	for _, rule := range crushMap.Rules {
		class, failureDomain := classAndFailureDomainForRule(rule)
		rules[rule.ID] = ruleInfo{deviceClass: class, failureDomain: failureDomain}
	}

	args := []string{"osd", "pool", "ls", "detail"}
	buf, err := NewCephCommand(context, clusterInfo, args).Run()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to list pool details for device class pool mapping. %s", string(buf))
	}
	var pools []poolCrushRule
	if err := json.Unmarshal(buf, &pools); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal pool details. %s", string(buf))
	}

	result := &DeviceClassPoolMap{Pools: map[string][]string{}, FailureDomains: map[string][]string{}}
	for _, pool := range pools {
		info, ok := rules[pool.RuleID]
		if !ok || info.deviceClass == "" {
			// rule spans device classes (or could not be resolved): the cluster is not isolated
			result.HasSpanningPool = true
			continue
		}
		result.Pools[info.deviceClass] = append(result.Pools[info.deviceClass], pool.Name)
		result.FailureDomains[info.deviceClass] = append(result.FailureDomains[info.deviceClass], info.failureDomain)
	}
	return result, nil
}

// pgBriefEntry is the subset of a `pg ls-by-pool` entry we need to judge cleanliness.
type pgBriefEntry struct {
	State string `json:"state"`
}

// getPGStatesByPool returns a count of PGs per state name for a single pool.
func getPGStatesByPool(context *clusterd.Context, clusterInfo *ClusterInfo, poolName string) (map[string]int, error) {
	args := []string{"pg", "ls-by-pool", poolName}
	buf, err := NewCephCommand(context, clusterInfo, args).Run()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to list pgs for pool %q. %s", poolName, string(buf))
	}

	// `pg ls-by-pool` returns a bare JSON array on some Ceph versions and an object
	// with a "pg_stats" array on others. Accept both.
	var entries []pgBriefEntry
	if err := json.Unmarshal(buf, &entries); err != nil {
		var wrapper struct {
			PgStats []pgBriefEntry `json:"pg_stats"`
		}
		if wrapErr := json.Unmarshal(buf, &wrapper); wrapErr != nil {
			return nil, errors.Wrapf(err, "failed to unmarshal pg list for pool %q. %s", poolName, string(buf))
		}
		entries = wrapper.PgStats
	}

	states := map[string]int{}
	for _, entry := range entries {
		states[entry.State]++
	}
	return states, nil
}

// ArePoolsClean reports whether every PG of the given pools is in a healthy state,
// using the same pgHealthyRegex semantics as IsClusterClean. It is the per-device-class
// analog of IsClusterClean: the caller passes the pools belonging to one device class
// so that class can unblock independently of others.
func ArePoolsClean(context *clusterd.Context, clusterInfo *ClusterInfo, poolNames []string, pgHealthyRegex string) (string, bool, error) {
	healthyRegex := defaultPgHealthyRegexCompiled
	if pgHealthyRegex != "" {
		compiled, err := regexp.Compile(pgHealthyRegex)
		if err != nil {
			return "unable to compile pgHealthyRegex", false, err
		}
		healthyRegex = compiled
	}

	totalPGs, cleanPGs := 0, 0
	for _, poolName := range poolNames {
		states, err := getPGStatesByPool(context, clusterInfo, poolName)
		if err != nil {
			return "unable to get PG health", false, err
		}
		for state, count := range states {
			totalPGs += count
			if healthyRegex.MatchString(state) {
				cleanPGs += count
			}
		}
	}

	if totalPGs == 0 {
		return "pools have no PGs", true, nil
	}
	if cleanPGs == totalPGs {
		return "all PGs in pools are clean", true, nil
	}
	return fmt.Sprintf("pools are not fully clean. clean %d of %d PGs", cleanPGs, totalPGs), false, nil
}
