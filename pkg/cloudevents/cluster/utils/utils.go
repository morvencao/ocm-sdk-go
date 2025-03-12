package utils

import (
	"encoding/json"
	"fmt"

	jsonpatch "github.com/evanphx/json-patch"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	clusterv1 "open-cluster-management.io/api/cluster/v1"
)

// Patch applies the patch to a cluster with the patch type.
func Patch(patchType types.PatchType, cluster *clusterv1.ManagedCluster, patchData []byte) (*clusterv1.ManagedCluster, error) {
	clusterData, err := json.Marshal(cluster)
	if err != nil {
		return nil, err
	}

	var patchedData []byte
	switch patchType {
	case types.JSONPatchType:
		var patchObj jsonpatch.Patch
		patchObj, err = jsonpatch.DecodePatch(patchData)
		if err != nil {
			return nil, err
		}
		patchedData, err = patchObj.Apply(clusterData)
		if err != nil {
			return nil, err
		}

	case types.MergePatchType:
		patchedData, err = jsonpatch.MergePatch(clusterData, patchData)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported patch type: %s", patchType)
	}

	patchedCluster := &clusterv1.ManagedCluster{}
	if err := json.Unmarshal(patchedData, patchedCluster); err != nil {
		return nil, err
	}

	return patchedCluster, nil
}

// ListClustersWithOptions retrieves the managedclusters from store which matches the options.
func ListClustersWithOptions(store cache.Store, opts metav1.ListOptions) ([]*clusterv1.ManagedCluster, error) {
	var err error

	labelSelector := labels.Everything()
	fieldSelector := fields.Everything()

	if len(opts.LabelSelector) != 0 {
		labelSelector, err = labels.Parse(opts.LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid labels selector %q: %v", opts.LabelSelector, err)
		}
	}

	if len(opts.FieldSelector) != 0 {
		fieldSelector, err = fields.ParseSelector(opts.FieldSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid fields selector %q: %v", opts.FieldSelector, err)
		}
	}

	clusters := []*clusterv1.ManagedCluster{}
	// list with labels
	if err := cache.ListAll(store, labelSelector, func(obj interface{}) {
		cluster, ok := obj.(*clusterv1.ManagedCluster)
		if !ok {
			return
		}

		clusterFieldSet := fields.Set{
			"metadata.name": cluster.Name,
		}

		if !fieldSelector.Matches(clusterFieldSet) {
			return
		}

		clusters = append(clusters, cluster)
	}); err != nil {
		return nil, err
	}

	return clusters, nil
}
