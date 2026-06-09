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
	"testing"

	"github.com/pkg/errors"
	"github.com/rook/rook/pkg/clusterd"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/stretchr/testify/assert"
)

// mockPGLSExecutor returns an executor that answers `pg ls-by-pool <pool>` from the given
// pool-name to JSON-response map and errors on any other command.
func mockPGLSExecutor(responses map[string]string) *exectest.MockExecutor {
	executor := &exectest.MockExecutor{}
	executor.MockExecuteCommandWithOutput = func(command string, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "pg" && args[1] == "ls-by-pool" {
			if resp, ok := responses[args[2]]; ok {
				return resp, nil
			}
			return "", errors.Errorf("no mock pg list for pool %q", args[2])
		}
		return "", errors.Errorf("unexpected command %q args %v", command, args)
	}
	return executor
}

func TestClassAndFailureDomainForRule(t *testing.T) {
	tests := []struct {
		name        string
		steps       []stepSpec
		expectClass string
		expectFD    string
	}{
		{
			name:        "class-isolated ssd rule on host",
			steps:       []stepSpec{{Operation: "take", ItemName: "default~ssd"}, {Operation: "chooseleaf_firstn", Type: "host"}, {Operation: "emit"}},
			expectClass: "ssd",
			expectFD:    "host",
		},
		{
			name:        "class-isolated hdd rule on zone",
			steps:       []stepSpec{{Operation: "take", ItemName: "default~hdd"}, {Operation: "chooseleaf_firstn", Type: "zone"}, {Operation: "emit"}},
			expectClass: "hdd",
			expectFD:    "zone",
		},
		{
			name:        "plain rule spans classes",
			steps:       []stepSpec{{Operation: "take", ItemName: "default"}, {Operation: "chooseleaf_firstn", Type: "host"}, {Operation: "emit"}},
			expectClass: "",
			expectFD:    "host",
		},
		{
			name:        "hybrid rule with two class takes is not isolated",
			steps:       []stepSpec{{Operation: "take", ItemName: "default~hdd"}, {Operation: "chooseleaf_firstn", Type: "host"}, {Operation: "emit"}, {Operation: "take", ItemName: "default~ssd"}, {Operation: "emit"}},
			expectClass: "",
			expectFD:    "host",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			class, fd := classAndFailureDomainForRule(ruleSpec{Steps: tc.steps})
			assert.Equal(t, tc.expectClass, class)
			assert.Equal(t, tc.expectFD, fd)
		})
	}
}

func TestGetDeviceClassPools(t *testing.T) {
	crushDump := `{"rules":[
		{"rule_id":0,"rule_name":"replicated_rule","steps":[{"op":"take","item_name":"default"},{"op":"chooseleaf_firstn","type":"host"},{"op":"emit"}]},
		{"rule_id":1,"rule_name":"hdd_rule","steps":[{"op":"take","item_name":"default~hdd"},{"op":"chooseleaf_firstn","type":"zone"},{"op":"emit"}]},
		{"rule_id":2,"rule_name":"ssd_rule","steps":[{"op":"take","item_name":"default~ssd"},{"op":"chooseleaf_firstn","type":"host"},{"op":"emit"}]}
	]}`

	mock := func(poolDetail string) *clusterd.Context {
		executor := &exectest.MockExecutor{}
		executor.MockExecuteCommandWithOutput = func(command string, args ...string) (string, error) {
			if len(args) >= 3 && args[0] == "osd" && args[1] == "crush" && args[2] == "dump" {
				return crushDump, nil
			}
			if len(args) >= 4 && args[0] == "osd" && args[1] == "pool" && args[2] == "ls" && args[3] == "detail" {
				return poolDetail, nil
			}
			return "", errors.Errorf("unexpected command %q args %v", command, args)
		}
		return &clusterd.Context{Executor: executor}
	}

	t.Run("all pools class-isolated", func(t *testing.T) {
		poolDetail := `[{"pool_name":"hdd-pool","crush_rule":1},{"pool_name":"ssd-pool","crush_rule":2}]`
		dc, err := GetDeviceClassPools(mock(poolDetail), AdminTestClusterInfo("mycluster"))
		assert.NoError(t, err)
		assert.False(t, dc.HasSpanningPool)
		assert.Equal(t, []string{"hdd-pool"}, dc.Pools["hdd"])
		assert.Equal(t, []string{"ssd-pool"}, dc.Pools["ssd"])
		assert.Equal(t, []string{"zone"}, dc.FailureDomains["hdd"])
		assert.Equal(t, []string{"host"}, dc.FailureDomains["ssd"])
	})

	t.Run("a spanning pool (e.g. .nfs/.mgr) flips HasSpanningPool", func(t *testing.T) {
		poolDetail := `[{"pool_name":"hdd-pool","crush_rule":1},{"pool_name":".nfs","crush_rule":0}]`
		dc, err := GetDeviceClassPools(mock(poolDetail), AdminTestClusterInfo("mycluster"))
		assert.NoError(t, err)
		assert.True(t, dc.HasSpanningPool, ".nfs uses the class-spanning replicated_rule")
	})
}

func TestArePoolsClean(t *testing.T) {
	// pgListClean is a bare-array `pg ls-by-pool` response with all PGs active+clean.
	pgListClean := `[{"pgid":"1.0","state":"active+clean"},{"pgid":"1.1","state":"active+clean"}]`
	// pgListWrappedClean uses the object form some Ceph versions return.
	pgListWrappedClean := `{"pg_stats":[{"pgid":"2.0","state":"active+clean"}]}`
	pgListDirty := `[{"pgid":"3.0","state":"active+clean"},{"pgid":"3.1","state":"active+undersized+degraded"}]`

	t.Run("all pools clean across both response shapes", func(t *testing.T) {
		executor := mockPGLSExecutor(map[string]string{"a": pgListClean, "b": pgListWrappedClean})
		context := &clusterd.Context{Executor: executor}
		_, clean, err := ArePoolsClean(context, AdminTestClusterInfo("mycluster"), []string{"a", "b"}, "")
		assert.NoError(t, err)
		assert.True(t, clean)
	})

	t.Run("one dirty pool makes the class dirty", func(t *testing.T) {
		executor := mockPGLSExecutor(map[string]string{"a": pgListClean, "b": pgListDirty})
		context := &clusterd.Context{Executor: executor}
		_, clean, err := ArePoolsClean(context, AdminTestClusterInfo("mycluster"), []string{"a", "b"}, "")
		assert.NoError(t, err)
		assert.False(t, clean)
	})

	t.Run("no pools means clean", func(t *testing.T) {
		executor := mockPGLSExecutor(map[string]string{})
		context := &clusterd.Context{Executor: executor}
		_, clean, err := ArePoolsClean(context, AdminTestClusterInfo("mycluster"), nil, "")
		assert.NoError(t, err)
		assert.True(t, clean)
	})
}
