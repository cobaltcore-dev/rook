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
	"testing"

	"github.com/stretchr/testify/assert"

	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
)

func TestGlobalFailureDomain(t *testing.T) {
	// the finest (lowest in the CRUSH hierarchy) of the in-use pools' failure domains
	layout := &cephclient.DeviceClassPDBLayout{FailureDomainTypes: []string{"region", "zone"}}
	assert.Equal(t, "zone", globalFailureDomain(layout))

	layout = &cephclient.DeviceClassPDBLayout{FailureDomainTypes: []string{"region", "zone", "host"}}
	assert.Equal(t, "host", globalFailureDomain(layout))

	// unknown levels and an empty list fall back to the default failure domain
	layout = &cephclient.DeviceClassPDBLayout{FailureDomainTypes: []string{"aaa", "bbb", "ccc"}}
	assert.Equal(t, "host", globalFailureDomain(layout))

	layout = &cephclient.DeviceClassPDBLayout{}
	assert.Equal(t, "host", globalFailureDomain(layout))
}
