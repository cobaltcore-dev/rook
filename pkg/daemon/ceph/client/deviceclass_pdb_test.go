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
	"fmt"
	"testing"

	"github.com/pkg/errors"
	"github.com/rook/rook/pkg/clusterd"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/stretchr/testify/assert"
)

// crushDumpFor builds an `osd crush dump` payload with one rule per entry, and
// returns the rule name to rule id mapping the pool mock needs. Each entry maps
// a rule name to its take item_name (e.g. "default~ssd" or the plain "default")
// and its chooseleaf failure domain type.
func crushDumpFor(rules map[string][2]string) (string, map[string]int) {
	ruleJSON := ""
	ruleIDs := map[string]int{}
	first := true
	id := 0
	for name, spec := range rules {
		if !first {
			ruleJSON += ","
		}
		first = false
		ruleJSON += fmt.Sprintf(`{"rule_id":%d,"rule_name":%q,"type":1,"steps":[`+
			`{"op":"take","item_name":%q},`+
			`{"op":"chooseleaf_firstn","num":0,"type":%q},`+
			`{"op":"emit"}]}`, id, name, spec[0], spec[1])
		ruleIDs[name] = id
		id++
	}
	return fmt.Sprintf(`{"devices":[],"types":[],"buckets":[],"rules":[%s]}`, ruleJSON), ruleIDs
}

// poolToRule maps each pool name to its crush rule name; a rule name absent from
// ruleIDs gets a dangling rule id, modeling a pool whose rule is missing.
func deviceClassExecutor(crushDump string, ruleIDs map[string]int, poolToRule map[string]string) *exectest.MockExecutor {
	executor := &exectest.MockExecutor{}
	executor.MockExecuteCommandWithOutput = func(command string, args ...string) (string, error) {
		switch {
		case args[0] == "osd" && args[1] == "crush" && args[2] == "dump":
			return crushDump, nil
		case args[0] == "osd" && args[1] == "pool" && args[2] == "ls" && args[3] == "detail":
			out := "["
			first := true
			num := 1
			for pool, rule := range poolToRule {
				if !first {
					out += ","
				}
				first = false
				id, ok := ruleIDs[rule]
				if !ok {
					id = 9999
				}
				out += fmt.Sprintf(`{"pool_id":%d,"pool_name":%q,"crush_rule":%d}`, num, pool, id)
				num++
			}
			return out + "]", nil
		}
		return "", errors.Errorf("unexpected ceph command '%v'", args)
	}
	return executor
}

func TestRuleDeviceClass(t *testing.T) {
	tests := []struct {
		name      string
		rule      ruleSpec
		wantClass string
		wantFD    string
		confined  bool
	}{
		{
			name: "class-confined ssd host rule",
			rule: ruleSpec{Steps: []stepSpec{
				{Operation: "take", ItemName: "default~ssd"},
				{Operation: "chooseleaf_firstn", Type: "host"},
				{Operation: "emit"},
			}},
			wantClass: "ssd", wantFD: "host", confined: true,
		},
		{
			name: "plain root rule spans classes",
			rule: ruleSpec{Steps: []stepSpec{
				{Operation: "take", ItemName: "default"},
				{Operation: "chooseleaf_firstn", Type: "host"},
			}},
			confined: false,
		},
		{
			name: "hybrid rule with two classes spans classes",
			rule: ruleSpec{Steps: []stepSpec{
				{Operation: "take", ItemName: "default~hdd"},
				{Operation: "chooseleaf_firstn", Type: "host"},
				{Operation: "emit"},
				{Operation: "take", ItemName: "default~ssd"},
				{Operation: "chooseleaf_firstn", Type: "host"},
				{Operation: "emit"},
			}},
			confined: false,
		},
		{
			name: "two-step class-confined rule: outermost choose wins over inner chooseleaf",
			rule: ruleSpec{Steps: []stepSpec{
				{Operation: "take", ItemName: "default~ssd"},
				{Operation: "choose_firstn", Type: "zone"},
				{Operation: "chooseleaf_firstn", Type: "host"},
				{Operation: "emit"},
			}},
			wantClass: "ssd", wantFD: "zone", confined: true,
		},
		{
			name: "EC rule with chooseleaf_indep zone",
			rule: ruleSpec{Steps: []stepSpec{
				{Operation: "set_chooseleaf_tries", Type: ""},
				{Operation: "take", ItemName: "default~hdd"},
				{Operation: "chooseleaf_indep", Type: "zone"},
				{Operation: "emit"},
			}},
			wantClass: "hdd", wantFD: "zone", confined: true,
		},
		{
			name: "confined rule without a failure domain is not confined",
			rule: ruleSpec{Steps: []stepSpec{
				{Operation: "take", ItemName: "default~ssd"},
				{Operation: "emit"},
			}},
			confined: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			class, fd, confined := ruleDeviceClass(tc.rule)
			assert.Equal(t, tc.confined, confined)
			if confined {
				assert.Equal(t, tc.wantClass, class)
				assert.Equal(t, tc.wantFD, fd)
			}
		})
	}
}

func TestGetDeviceClassPDBLayout(t *testing.T) {
	t.Run("two confined classes is eligible", func(t *testing.T) {
		crush, ruleIDs := crushDumpFor(map[string][2]string{
			"ssd_rule": {"default~ssd", "host"},
			"hdd_rule": {"default~hdd", "zone"},
		})
		executor := deviceClassExecutor(crush, ruleIDs, map[string]string{
			"ssd-pool":  "ssd_rule",
			"hdd-pool":  "hdd_rule",
			"hdd-pool2": "hdd_rule",
		})
		layout, err := GetDeviceClassPDBLayout(&clusterd.Context{Executor: executor}, AdminTestClusterInfo("mycluster"))
		assert.NoError(t, err)
		assert.Len(t, layout.Classes, 2)
		assert.Equal(t, "host", layout.Classes["ssd"].FailureDomainType)
		assert.Equal(t, "zone", layout.Classes["hdd"].FailureDomainType)
		assert.ElementsMatch(t, []string{"ssd-pool"}, layout.Classes["ssd"].Pools)
		assert.ElementsMatch(t, []string{"hdd-pool", "hdd-pool2"}, layout.Classes["hdd"].Pools)
		// one entry per in-use pool's outermost selected level, for the global FD
		assert.ElementsMatch(t, []string{"host", "zone", "zone"}, layout.FailureDomainTypes)
	})

	t.Run("a pool spanning classes clears the partition", func(t *testing.T) {
		crush, ruleIDs := crushDumpFor(map[string][2]string{
			"ssd_rule":   {"default~ssd", "host"},
			"replicated": {"default", "host"},
			"hdd_rule":   {"default~hdd", "host"},
		})
		executor := deviceClassExecutor(crush, ruleIDs, map[string]string{
			"ssd-pool": "ssd_rule",
			"hdd-pool": "hdd_rule",
			".mgr":     "replicated", // spans all classes
		})
		layout, err := GetDeviceClassPDBLayout(&clusterd.Context{Executor: executor}, AdminTestClusterInfo("mycluster"))
		assert.NoError(t, err)
		assert.Empty(t, layout.Classes)
	})

	t.Run("single class is not enough on its own", func(t *testing.T) {
		crush, ruleIDs := crushDumpFor(map[string][2]string{
			"ssd_rule": {"default~ssd", "host"},
		})
		executor := deviceClassExecutor(crush, ruleIDs, map[string]string{
			"ssd-pool": "ssd_rule",
		})
		layout, err := GetDeviceClassPDBLayout(&clusterd.Context{Executor: executor}, AdminTestClusterInfo("mycluster"))
		assert.NoError(t, err)
		assert.Len(t, layout.Classes, 1)
	})

	t.Run("missing crush rule fails closed, clearing the partition", func(t *testing.T) {
		crush, ruleIDs := crushDumpFor(map[string][2]string{
			"ssd_rule": {"default~ssd", "host"},
		})
		executor := deviceClassExecutor(crush, ruleIDs, map[string]string{
			"ssd-pool":      "ssd_rule",
			"orphaned-pool": "missing_rule",
		})
		layout, err := GetDeviceClassPDBLayout(&clusterd.Context{Executor: executor}, AdminTestClusterInfo("mycluster"))
		assert.NoError(t, err)
		assert.Empty(t, layout.Classes)
	})
}

func TestIsDeviceClassClean(t *testing.T) {
	pgResp := func(states ...string) string {
		out := `{"pg_stats":[`
		for i, s := range states {
			if i > 0 {
				out += ","
			}
			out += fmt.Sprintf(`{"state":%q}`, s)
		}
		return out + "]}"
	}

	t.Run("all pgs clean", func(t *testing.T) {
		executor := &exectest.MockExecutor{}
		executor.MockExecuteCommandWithOutput = func(command string, args ...string) (string, error) {
			if args[0] == "pg" && args[1] == "ls-by-pool" {
				return pgResp("active+clean", "active+clean"), nil
			}
			return "", errors.Errorf("unexpected ceph command '%v'", args)
		}
		_, clean, err := IsDeviceClassClean(&clusterd.Context{Executor: executor}, AdminTestClusterInfo("mycluster"), []string{"ssd-pool"}, "")
		assert.NoError(t, err)
		assert.True(t, clean)
	})

	t.Run("a non-clean pg makes the class unclean", func(t *testing.T) {
		executor := &exectest.MockExecutor{}
		executor.MockExecuteCommandWithOutput = func(command string, args ...string) (string, error) {
			if args[0] == "pg" && args[1] == "ls-by-pool" {
				if args[2] == "ssd-pool-a" {
					return pgResp("active+clean"), nil
				}
				return pgResp("active+clean", "active+recovering"), nil
			}
			return "", errors.Errorf("unexpected ceph command '%v'", args)
		}
		_, clean, err := IsDeviceClassClean(&clusterd.Context{Executor: executor}, AdminTestClusterInfo("mycluster"), []string{"ssd-pool-a", "ssd-pool-b"}, "")
		assert.NoError(t, err)
		assert.False(t, clean)
	})

	t.Run("no pgs counts as clean", func(t *testing.T) {
		executor := &exectest.MockExecutor{}
		executor.MockExecuteCommandWithOutput = func(command string, args ...string) (string, error) {
			if args[0] == "pg" && args[1] == "ls-by-pool" {
				return `{"pg_stats":[]}`, nil
			}
			return "", errors.Errorf("unexpected ceph command '%v'", args)
		}
		_, clean, err := IsDeviceClassClean(&clusterd.Context{Executor: executor}, AdminTestClusterInfo("mycluster"), []string{"empty-pool"}, "")
		assert.NoError(t, err)
		assert.True(t, clean)
	})
}
