/*
Copyright 2019, 2020 the Velero contributors.

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

package restore

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/vmware-tanzu/velero/pkg/util/boolptr"

	corev1api "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/vmware-tanzu/velero-plugin-for-csi/internal/util"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
)

const (
	AnnBindCompleted          = "pv.kubernetes.io/bind-completed"
	AnnBoundByController      = "pv.kubernetes.io/bound-by-controller"
	AnnBetaStorageProvisioner = "volume.beta.kubernetes.io/storage-provisioner"
	AnnStorageProvisioner     = "volume.kubernetes.io/storage-provisioner"
	AnnSelectedNode           = "volume.kubernetes.io/selected-node"
)

// PVCRestoreItemAction is a restore item action plugin for Velero
type PVCRestoreItemAction struct {
	Log logrus.FieldLogger
}

// AppliesTo returns information indicating that the PVCRestoreItemAction should be run while restoring PVCs.
func (p *PVCRestoreItemAction) AppliesTo() (velero.ResourceSelector, error) {
	return velero.ResourceSelector{
		IncludedResources: []string{"persistentvolumeclaims"},
		//TODO: add label selector volumeSnapshotLabel
	}, nil
}

func removePVCAnnotations(pvc *corev1api.PersistentVolumeClaim, remove []string) {
	if pvc.Annotations == nil {
		pvc.Annotations = make(map[string]string)
		return
	}
	for k := range pvc.Annotations {
		if util.Contains(remove, k) {
			delete(pvc.Annotations, k)
		}
	}
}

func resetPVCSpec(pvc *corev1api.PersistentVolumeClaim, vsName string) {
	var apiGroup = "snapshot.storage.k8s.io"
	// Restore operation for the PVC will use the volumesnapshot as the data source.
	// So clear out the volume name, which is a ref to the PV
	pvc.Spec.VolumeName = ""
	dataSourceRef := &corev1api.TypedLocalObjectReference{
		APIGroup: &apiGroup,
		Kind:     "VolumeSnapshot",
		Name:     vsName,
	}
	pvc.Spec.DataSource = dataSourceRef
	pvc.Spec.DataSourceRef = dataSourceRef
}

func setPVCStorageResourceRequest(pvc *corev1api.PersistentVolumeClaim, restoreSize resource.Quantity, log logrus.FieldLogger) {
	{
		if pvc.Spec.Resources.Requests == nil {
			pvc.Spec.Resources.Requests = corev1api.ResourceList{}
		}

		storageReq, exists := pvc.Spec.Resources.Requests[corev1api.ResourceStorage]
		if !exists || storageReq.Cmp(restoreSize) < 0 {
			pvc.Spec.Resources.Requests[corev1api.ResourceStorage] = restoreSize
			rs := pvc.Spec.Resources.Requests[corev1api.ResourceStorage]
			log.Infof("Resetting storage requests for PVC %s/%s to %s", pvc.Namespace, pvc.Name, rs.String())
		}
	}
}

// Execute modifies the PVC's spec to use the volumesnapshot object as the data source ensuring that the newly provisioned volume
// can be pre-populated with data from the volumesnapshot.
func (p *PVCRestoreItemAction) Execute(input *velero.RestoreItemActionExecuteInput) (*velero.RestoreItemActionExecuteOutput, error) {
	var pvc corev1api.PersistentVolumeClaim
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(input.Item.UnstructuredContent(), &pvc); err != nil {
		return nil, errors.WithStack(err)
	}
	p.Log.Infof("Starting PVCRestoreItemAction for PVC %s/%s", pvc.Namespace, pvc.Name)

	removePVCAnnotations(&pvc,
		[]string{AnnBindCompleted, AnnBoundByController, AnnBetaStorageProvisioner, AnnStorageProvisioner, AnnSelectedNode})

	// If cross-namespace restore is configured, change the namespace
	// for PVC object to be restored
	newNamespace, ok := input.Restore.Spec.NamespaceMapping[pvc.GetNamespace()]
    if !ok {
        // Use original namespace
        newNamespace = pvc.Namespace
    }

	volumeSnapshotName, ok := pvc.Annotations[util.VolumeSnapshotLabel]
	if !ok {
		p.Log.Infof("Skipping PVCRestoreItemAction for PVC %s/%s, PVC does not have a CSI volumesnapshot.", newNamespace, pvc.Name)
		return &velero.RestoreItemActionExecuteOutput{
			UpdatedItem: input.Item,
		}, nil
	}

	// check if PVCDataSourceKey set/
	// if set, it means we need to remove/replace the datasource.
	// So just return here, and ys1000-plugin will handle that case.
	if input.Restore.Annotations != nil {
		_, ok := input.Restore.Annotations[util.PVCDataSourceKey]
		if ok {
			return &velero.RestoreItemActionExecuteOutput{
				UpdatedItem: input.Item,
			}, nil
		}
	}

	if boolptr.IsSetToFalse(input.Restore.Spec.RestorePVs) {
		p.Log.Infof("Restore did not request for PVs to be restored from snapshot %s/%s.", input.Restore.Namespace, input.Restore.Name)
		pvc.Spec.VolumeName = ""
		pvc.Spec.DataSource = nil
		pvc.Spec.DataSourceRef = nil
	} else {
		_, snapClient, err := util.GetClients()
		if err != nil {
			return nil, errors.WithStack(err)
		}

		vs, err := snapClient.SnapshotV1beta1().VolumeSnapshots(newNamespace).Get(context.TODO(), volumeSnapshotName, metav1.GetOptions{})
		if err != nil {
			return nil, errors.Wrapf(err, fmt.Sprintf("Failed to get Volumesnapshot %s/%s to restore PVC %s/%s", newNamespace, volumeSnapshotName, newNamespace, pvc.Name))
		}

		if _, exists := vs.Annotations[util.VolumeSnapshotRestoreSize]; exists {
			restoreSize, err := resource.ParseQuantity(vs.Annotations[util.VolumeSnapshotRestoreSize])
			if err != nil {
				return nil, errors.Wrapf(err, fmt.Sprintf("Failed to parse %s from annotation on Volumesnapshot %s/%s into restore size",
					vs.Annotations[util.VolumeSnapshotRestoreSize], vs.Namespace, vs.Name))
			}
			// It is possible that the volume provider allocated a larger capacity volume than what was requested in the backed up PVC.
			// In this scenario the volumesnapshot of the PVC will endup being larger than its requested storage size.
			// Such a PVC, on restore as-is, will be stuck attempting to use a Volumesnapshot as a data source for a PVC that
			// is not large enough.
			// To counter that, here we set the storage request on the PVC to the larger of the PVC's storage request and the size of the
			// VolumeSnapshot
			setPVCStorageResourceRequest(&pvc, restoreSize, p.Log)
		}
		resetPVCSpec(&pvc, volumeSnapshotName)
	}

	util.RemoveAnnotations(&pvc.ObjectMeta, []string{util.VolumeSnapshotLabel})
	pvcMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&pvc)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	p.Log.Infof("Returning from PVCRestoreItemAction for PVC %s/%s", newNamespace, pvc.Name)

	return &velero.RestoreItemActionExecuteOutput{
		UpdatedItem: &unstructured.Unstructured{Object: pvcMap},
	}, nil
}
