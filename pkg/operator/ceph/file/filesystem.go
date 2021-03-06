/*
Copyright 2016 The Rook Authors. All rights reserved.

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

package file

import (
	"fmt"
	"github.com/rook/rook/pkg/operator/k8sutil"

	"github.com/pkg/errors"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/daemon/ceph/client"
	cephconfig "github.com/rook/rook/pkg/daemon/ceph/config"
	"github.com/rook/rook/pkg/operator/ceph/file/mds"
	"github.com/rook/rook/pkg/operator/ceph/pool"
	cephver "github.com/rook/rook/pkg/operator/ceph/version"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	dataPoolSuffix     = "data"
	metaDataPoolSuffix = "metadata"
)

// Filesystem represents an instance of a Ceph filesystem (CephFS)
type Filesystem struct {
	Name      string
	Namespace string
}

// createFilesystem creates a Ceph filesystem with metadata servers
func createFilesystem(
	clusterInfo *cephconfig.ClusterInfo,
	context *clusterd.Context,
	fs cephv1.CephFilesystem,
	clusterSpec *cephv1.ClusterSpec,
	ownerRefs metav1.OwnerReference,
	dataDirHostPath string,
	scheme *runtime.Scheme,
) error {

	if len(fs.Spec.DataPools) != 0 {
		f := newFS(fs.Name, fs.Namespace)
		if err := f.doFilesystemCreate(context, clusterInfo.CephVersion, fs.Spec); err != nil {
			return errors.Wrapf(err, "failed to create filesystem %q", fs.Name)
		}
	}

	filesystem, err := client.GetFilesystem(context, fs.Namespace, fs.Name)
	if err != nil {
		return errors.Wrapf(err, "failed to get filesystem %q", fs.Name)
	}

	if fs.Spec.MetadataServer.ActiveStandby {
		if err = client.AllowStandbyReplay(context, fs.Namespace, fs.Name, fs.Spec.MetadataServer.ActiveStandby); err != nil {
			return errors.Wrapf(err, "failed to set allow_standby_replay to filesystem %q", fs.Name)
		}
	}

	// set the number of active mds instances
	if fs.Spec.MetadataServer.ActiveCount > 1 {
		if err = client.SetNumMDSRanks(context, clusterInfo.CephVersion, fs.Namespace, fs.Name, fs.Spec.MetadataServer.ActiveCount); err != nil {
			logger.Warningf("failed setting active mds count to %d. %v", fs.Spec.MetadataServer.ActiveCount, err)
		}
	}

	logger.Infof("start running mdses for filesystem %q", fs.Name)
	c := mds.NewCluster(clusterInfo, context, clusterSpec, fs, filesystem, ownerRefs, dataDirHostPath, scheme)
	if err := c.Start(); err != nil {
		return err
	}

	return nil
}

// deleteFileSystem deletes the filesystem from Ceph
func deleteFilesystem(
	clusterInfo *cephconfig.ClusterInfo,
	context *clusterd.Context,
	fs cephv1.CephFilesystem,
	clusterSpec *cephv1.ClusterSpec,
	ownerRefs metav1.OwnerReference,
	dataDirHostPath string,
	scheme *runtime.Scheme,
) error {
	filesystem, err := client.GetFilesystem(context, fs.Namespace, fs.Name)
	if err != nil {
		return errors.Wrapf(err, "failed to get filesystem %q", fs.Name)
	}
	c := mds.NewCluster(clusterInfo, context, clusterSpec, fs, filesystem, ownerRefs, dataDirHostPath, scheme)

	// Delete mds CephX keys and configuration in centralized mon database
	replicas := fs.Spec.MetadataServer.ActiveCount * 2
	for i := 0; i < int(replicas); i++ {
		daemonLetterID := k8sutil.IndexToName(i)
		daemonName := fmt.Sprintf("%s-%s", fs.Name, daemonLetterID)

		err = c.DeleteMdsCephObjects(daemonName)
		if err != nil {
			return errors.Wrapf(err, "failed to delete mds ceph objects for filesystem %q", fs.Name)
		}
	}

	// The most important part of deletion is that the filesystem gets removed from Ceph
	// The K8s resources will already be removed with the K8s owner references
	if err := downFilesystem(context, fs.Namespace, fs.Name); err != nil {
		// If the fs isn't deleted from Ceph, leave the daemons so it can still be used.
		return errors.Wrapf(err, "failed to down filesystem %q", fs.Name)
	}

	// Permanently remove the filesystem if it was created by rook
	if len(fs.Spec.DataPools) != 0 {
		if err := client.RemoveFilesystem(context, fs.Namespace, fs.Name, fs.Spec.PreservePoolsOnDelete); err != nil {
			return errors.Wrapf(err, "failed to remove filesystem %q", fs.Name)
		}
	}
	return nil
}

func validateFilesystem(context *clusterd.Context, f *cephv1.CephFilesystem) error {
	if f.Name == "" {
		return errors.New("missing name")
	}
	if f.Namespace == "" {
		return errors.New("missing namespace")
	}
	if f.Spec.MetadataServer.ActiveCount < 1 {
		return errors.New("MetadataServer.ActiveCount must be at least 1")
	}
	// No data pool means that we expect the fs to exist already
	if len(f.Spec.DataPools) == 0 {
		return nil
	}
	if err := pool.ValidatePoolSpec(context, f.Namespace, &f.Spec.MetadataPool); err != nil {
		return errors.Wrapf(err, "invalid metadata pool")
	}
	for _, p := range f.Spec.DataPools {
		if err := pool.ValidatePoolSpec(context, f.Namespace, &p); err != nil {
			return errors.Wrapf(err, "Invalid data pool")
		}
	}

	return nil
}

// newFS creates a new instance of the file (MDS) service
func newFS(name, namespace string) *Filesystem {
	return &Filesystem{
		Name:      name,
		Namespace: namespace,
	}
}

// SetPoolSize function sets the sizes for MetadataPool and dataPool
func SetPoolSize(f *Filesystem, context *clusterd.Context, spec cephv1.FilesystemSpec) error {
	// generating the metadata pool's name
	metadataPoolName := generateMetaDataPoolName(f)
	err := client.CreatePoolWithProfile(context, f.Namespace, metadataPoolName, spec.MetadataPool, "")
	if err != nil {
		return errors.Wrapf(err, "failed to update metadata pool %q", metadataPoolName)
	}
	// generating the data pool's name
	dataPoolNames := generateDataPoolNames(f, spec)
	for i, pool := range spec.DataPools {
		poolName := dataPoolNames[i]
		err := client.CreatePoolWithProfile(context, f.Namespace, poolName, pool, "")
		if err != nil {
			return errors.Wrapf(err, "failed to update datapool  %q", poolName)
		}
	}
	return nil
}

// doFilesystemCreate starts the Ceph file daemons and creates the filesystem in Ceph.
func (f *Filesystem) doFilesystemCreate(context *clusterd.Context, cephVersion cephver.CephVersion, spec cephv1.FilesystemSpec) error {

	_, err := client.GetFilesystem(context, f.Namespace, f.Name)
	if err == nil {
		logger.Infof("filesystem %s already exists", f.Name)
		// Even if the fs already exists, the num active mdses may have changed

		if err := client.SetNumMDSRanks(context, cephVersion, f.Namespace, f.Name, spec.MetadataServer.ActiveCount); err != nil {
			logger.Errorf(
				fmt.Sprintf("failed to set num mds ranks (max_mds) to %d for filesystem %s, still continuing. ", spec.MetadataServer.ActiveCount, f.Name) +
					"this error is not critical, but mdses may not be as failure tolerant as desired. " +
					fmt.Sprintf("USER should verify that the number of active mdses is %d with 'ceph fs get %s'", spec.MetadataServer.ActiveCount, f.Name) +
					fmt.Sprintf(". %v", err),
			)
		}
		if err := SetPoolSize(f, context, spec); err != nil {
			return errors.Wrap(err, "failed to set pools size")
		}
		return nil
	}
	if len(spec.DataPools) == 0 {
		return errors.New("at least one data pool must be specified")
	}

	fslist, err := client.ListFilesystems(context, f.Namespace)
	if err != nil {
		return errors.Wrapf(err, "Unable to list existing filesystem")
	}
	if len(fslist) > 0 && !client.IsMultiFSEnabled() {
		return errors.Errorf("cannot create multiple filesystems. enable %s env variable to create more than one", client.MultiFsEnv)
	}

	poolNames, err := client.GetPoolNamesByID(context, f.Namespace)
	if err != nil {
		return errors.Wrapf(err, "failed to get pool names")
	}

	logger.Infof("Creating filesystem %s", f.Name)

	// Make easy to locate a pool by name and avoid repeated searches
	reversedPoolMap := make(map[string]int)
	for key, value := range poolNames {
		reversedPoolMap[value] = key
	}

	poolsCreated := false
	metadataPoolName := generateMetaDataPoolName(f)
	if _, poolFound := reversedPoolMap[metadataPoolName]; !poolFound {
		poolsCreated = true
		err = client.CreatePoolWithProfile(context, f.Namespace, metadataPoolName, spec.MetadataPool, "")
		if err != nil {
			return errors.Wrapf(err, "failed to create metadata pool %q", metadataPoolName)
		}
	}

	dataPoolNames := generateDataPoolNames(f, spec)
	for i, pool := range spec.DataPools {
		poolName := dataPoolNames[i]
		if _, poolFound := reversedPoolMap[poolName]; !poolFound {
			poolsCreated = true
			err = client.CreatePoolWithProfile(context, f.Namespace, poolName, pool, "")
			if err != nil {
				return errors.Wrapf(err, "failed to create data pool %q", poolName)
			}
			if pool.IsErasureCoded() {
				// An erasure coded data pool used for a filesystem must allow overwrites
				if err := client.SetPoolProperty(context, f.Namespace, poolName, "allow_ec_overwrites", "true"); err != nil {
					logger.Warningf("failed to set ec pool property. %v", err)
				}
			}
		}
	}

	// create the filesystem ('fs new' needs to be forced in order to reuse pre-existing pools)
	// if only one pool is created new it wont work (to avoid inconsistencies).
	if err := client.CreateFilesystem(context, f.Namespace, f.Name, metadataPoolName, dataPoolNames, !poolsCreated); err != nil {
		return err
	}

	logger.Infof("created filesystem %s on %d data pool(s) and metadata pool %s", f.Name, len(dataPoolNames), metadataPoolName)
	return nil
}

// downFilesystem marks the filesystem as down and the MDS' as failed
func downFilesystem(context *clusterd.Context, namespace, filesystemName string) error {
	logger.Infof("Downing filesystem %s", filesystemName)

	if err := client.FailFilesystem(context, namespace, filesystemName); err != nil {
		return err
	}
	logger.Infof("Downed filesystem %s", filesystemName)
	return nil
}

// generateDataPoolName generates DataPool name by prefixing the filesystem name to the constant DataPoolSuffix
func generateDataPoolNames(f *Filesystem, spec cephv1.FilesystemSpec) []string {
	var dataPoolNames []string
	for i := range spec.DataPools {
		poolName := fmt.Sprintf("%s-%s%d", f.Name, dataPoolSuffix, i)
		dataPoolNames = append(dataPoolNames, poolName)
	}
	return dataPoolNames
}

// generateMetaDataPoolName generates MetaDataPool name by prefixing the filesystem name to the constant metaDataPoolSuffix
func generateMetaDataPoolName(f *Filesystem) string {
	return fmt.Sprintf("%s-%s", f.Name, metaDataPoolSuffix)
}
