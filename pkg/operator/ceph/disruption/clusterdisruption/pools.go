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
	"fmt"

	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"

	"github.com/pkg/errors"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// cephBlockPoolsExist reports whether any CephBlockPool CR exists in the namespace.
func (r *ReconcileClusterDisruption) cephBlockPoolsExist(request reconcile.Request) (bool, error) {
	cephBlockPoolList := &cephv1.CephBlockPoolList{}
	if err := r.client.List(r.context.OpManagerContext, cephBlockPoolList, client.InNamespace(request.Namespace)); err != nil {
		return false, errors.Wrapf(err, "could not list the CephBlockPools %v", request.NamespacedName)
	}
	return len(cephBlockPoolList.Items) > 0, nil
}

// reconcileCephObjectStore reconciles the RGW PDB (naive minAvailable n-1) for every
// CephObjectStore in the namespace and reports whether any exist.
func (r *ReconcileClusterDisruption) reconcileCephObjectStore(request reconcile.Request) (bool, error) {
	cephObjectStoreList := &cephv1.CephObjectStoreList{}
	if err := r.client.List(r.context.OpManagerContext, cephObjectStoreList, client.InNamespace(request.Namespace)); err != nil {
		return false, errors.Wrapf(err, "could not list the CephObjectStores %v", request.NamespacedName)
	}
	for _, objectStore := range cephObjectStoreList.Items {
		storeName := objectStore.ObjectMeta.Name
		namespace := objectStore.ObjectMeta.Namespace
		pdbName := fmt.Sprintf("rook-ceph-rgw-%s", storeName)
		labelSelector := &metav1.LabelSelector{
			MatchLabels: map[string]string{"rgw": storeName},
		}

		rgwCount := objectStore.Spec.Gateway.Instances
		minAvailable := &intstr.IntOrString{IntVal: rgwCount - 1}
		if minAvailable.IntVal < 1 {
			continue
		}
		blockOwnerDeletion := false
		objectMeta := metav1.ObjectMeta{
			Name:      pdbName,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         objectStore.APIVersion,
					Kind:               objectStore.Kind,
					Name:               objectStore.ObjectMeta.Name,
					UID:                objectStore.UID,
					BlockOwnerDeletion: &blockOwnerDeletion,
				},
			},
		}
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: objectMeta,
			Spec: policyv1.PodDisruptionBudgetSpec{
				Selector:     labelSelector,
				MinAvailable: minAvailable,
			},
		}
		pdbRequest := types.NamespacedName{Name: pdbName, Namespace: namespace}
		if err := r.reconcileStaticPDB(pdbRequest, pdb); err != nil {
			return false, errors.Wrapf(err, "failed to reconcile cephobjectstore pdb %v", pdbRequest)
		}
	}
	return len(cephObjectStoreList.Items) > 0, nil
}

// reconcileCephFilesystem reconciles the MDS PDB (naive minAvailable n-1, from
// spec.metadataServer.activeCount) for every CephFilesystem in the namespace and
// reports whether any exist.
func (r *ReconcileClusterDisruption) reconcileCephFilesystem(request reconcile.Request) (bool, error) {
	cephFilesystemList := &cephv1.CephFilesystemList{}
	if err := r.client.List(r.context.OpManagerContext, cephFilesystemList, client.InNamespace(request.Namespace)); err != nil {
		return false, errors.Wrapf(err, "could not list the CephFilesystems %v", request.NamespacedName)
	}
	for _, filesystem := range cephFilesystemList.Items {
		fsName := filesystem.ObjectMeta.Name
		namespace := filesystem.ObjectMeta.Namespace
		pdbName := fmt.Sprintf("rook-ceph-mds-%s", fsName)
		labelSelector := &metav1.LabelSelector{
			MatchLabels: map[string]string{"rook_file_system": fsName},
		}

		activeCount := filesystem.Spec.MetadataServer.ActiveCount
		minAvailable := &intstr.IntOrString{IntVal: activeCount - 1}
		if filesystem.Spec.MetadataServer.ActiveStandby {
			minAvailable.IntVal++
		}
		if minAvailable.IntVal < 1 {
			continue
		}
		blockOwnerDeletion := false
		objectMeta := metav1.ObjectMeta{
			Name:      pdbName,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         filesystem.APIVersion,
					Kind:               filesystem.Kind,
					Name:               filesystem.ObjectMeta.Name,
					UID:                filesystem.UID,
					BlockOwnerDeletion: &blockOwnerDeletion,
				},
			},
		}
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: objectMeta,
			Spec: policyv1.PodDisruptionBudgetSpec{
				Selector:     labelSelector,
				MinAvailable: minAvailable,
			},
		}
		pdbRequest := types.NamespacedName{Name: pdbName, Namespace: namespace}
		if err := r.reconcileStaticPDB(pdbRequest, pdb); err != nil {
			return false, errors.Wrapf(err, "failed to reconcile cephfs pdb %v", pdbRequest)
		}
	}
	return len(cephFilesystemList.Items) > 0, nil
}
