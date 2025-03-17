package client

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubetypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog/v2"

	"open-cluster-management.io/sdk-go/pkg/cloudevents/common"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/csr/codec"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/csr/store"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/csr/utils"
	cloudeventserrors "open-cluster-management.io/sdk-go/pkg/cloudevents/errors"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/generic"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/generic/types"

	certificatev1 "k8s.io/api/certificates/v1"
	certificatesv1 "k8s.io/client-go/applyconfigurations/certificates/v1"
	certificatev1client "k8s.io/client-go/kubernetes/typed/certificates/v1"
)

// CSRAgentClient implements the CSRInterface. It sends the csr to source by
// CloudEventAgentClient.
type CSRAgentClient struct {
	sync.RWMutex

	cloudEventsClient *generic.CloudEventAgentClient[*certificatev1.CertificateSigningRequest]
	watcherStore      store.CSRClientWatcherStore
}

var _ certificatev1client.CertificateSigningRequestInterface = &CSRAgentClient{}

func NewCSRAgentClient(
	cloudEventsClient *generic.CloudEventAgentClient[*certificatev1.CertificateSigningRequest],
	watcherStore store.CSRClientWatcherStore,
	clusterName string,
) *CSRAgentClient {
	return &CSRAgentClient{
		cloudEventsClient: cloudEventsClient,
		watcherStore:      watcherStore,
	}
}

func (c *CSRAgentClient) Create(ctx context.Context, csr *certificatev1.CertificateSigningRequest, opts metav1.CreateOptions) (*certificatev1.CertificateSigningRequest, error) {
	klog.V(4).Infof("creating certificatesigningrequest %s", csr.Name)
	_, exists, err := c.watcherStore.Get(csr.Name)
	if err != nil {
		returnErr := errors.NewInternalError(err)
		return nil, returnErr
	}
	if exists {
		returnErr := errors.NewAlreadyExists(common.CSRGR, csr.Name)
		return nil, returnErr
	}

	eventType := types.CloudEventsType{
		CloudEventsDataType: codec.CSREventDataType,
		SubResource:         types.SubResourceSpec,
		Action:              common.CreateRequestAction,
	}

	// TODO: validate the csr

	if err := c.cloudEventsClient.Publish(ctx, eventType, csr); err != nil {
		returnErr := cloudeventserrors.NewPublishError(common.CSRGR, csr.Name, err)
		return nil, returnErr
	}

	// add the new csr to the local cache.
	if err := c.watcherStore.Add(csr); err != nil {
		returnErr := errors.NewInternalError(err)
		return nil, returnErr
	}

	return csr.DeepCopy(), nil

}

func (c *CSRAgentClient) Update(ctx context.Context, csr *certificatev1.CertificateSigningRequest, opts metav1.UpdateOptions) (*certificatev1.CertificateSigningRequest, error) {
	return nil, errors.NewMethodNotSupported(common.CSRGR, "update")
}

func (c *CSRAgentClient) UpdateStatus(ctx context.Context, csr *certificatev1.CertificateSigningRequest, opts metav1.UpdateOptions) (*certificatev1.CertificateSigningRequest, error) {
	return nil, errors.NewMethodNotSupported(common.CSRGR, "updatestatus")
}

func (c *CSRAgentClient) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	return errors.NewMethodNotSupported(common.CSRGR, "delete")
}

func (c *CSRAgentClient) DeleteCollection(ctx context.Context, opts metav1.DeleteOptions, listOpts metav1.ListOptions) error {
	return errors.NewMethodNotSupported(common.CSRGR, "deletecollection")
}

func (c *CSRAgentClient) Get(ctx context.Context, name string, opts metav1.GetOptions) (*certificatev1.CertificateSigningRequest, error) {
	klog.V(4).Infof("getting csr %s", name)
	csr, exists, err := c.watcherStore.Get(name)
	if err != nil {
		returnErr := errors.NewInternalError(err)
		return nil, returnErr
	}
	if !exists {
		returnErr := errors.NewNotFound(common.CSRGR, name)
		return nil, returnErr
	}

	return csr, nil
}

func (c *CSRAgentClient) List(ctx context.Context, opts metav1.ListOptions) (*certificatev1.CertificateSigningRequestList, error) {
	klog.V(4).Info("list csr")
	csrs, err := c.watcherStore.List(opts)
	if err != nil {
		returnErr := errors.NewInternalError(err)
		return nil, returnErr
	}

	return csrs, nil
}

func (c *CSRAgentClient) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	klog.V(4).Info("watch csr")
	watcher, err := c.watcherStore.GetWatcher(opts)
	if err != nil {
		returnErr := errors.NewInternalError(err)
		return nil, returnErr
	}

	return watcher, nil
}

func (c *CSRAgentClient) Patch(ctx context.Context, name string, pt kubetypes.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*certificatev1.CertificateSigningRequest, error) {
	klog.V(4).Infof("patching csr %s", name)
	lastCsr, exists, err := c.watcherStore.Get(name)
	if err != nil {
		returnErr := errors.NewInternalError(err)
		return nil, returnErr
	}
	if !exists {
		returnErr := errors.NewNotFound(common.CSRGR, name)
		return nil, returnErr
	}

	patchedCsr, err := utils.Patch(pt, lastCsr, data)
	if err != nil {
		returnErr := errors.NewInternalError(err)
		return nil, returnErr
	}

	eventType := types.CloudEventsType{
		CloudEventsDataType: codec.CSREventDataType,
		SubResource:         types.SubResourceStatus,
	}

	newCsr := patchedCsr.DeepCopy()

	statusUpdated, err := isStatusUpdate(subresources)
	if err != nil {
		returnErr := errors.NewGenericServerResponse(http.StatusMethodNotAllowed, "patch", common.CSRGR, name, err.Error(), 0, false)
		return nil, returnErr
	}

	if statusUpdated {
		// avoid race conditions among the agent's go routines
		c.Lock()
		defer c.Unlock()

		eventType.Action = common.UpdateRequestAction
		// publish the status update event to source, source will check the resource version
		// and reject the update if it's status update is outdated.
		if err := c.cloudEventsClient.Publish(ctx, eventType, newCsr); err != nil {
			returnErr := cloudeventserrors.NewPublishError(common.CSRGR, name, err)
			return nil, returnErr
		}

		// Fetch the latest csr from the store and verify the resource version to avoid updating the store
		// with outdated csr. Return a conflict error if the resource version is outdated.
		// Due to the lack of read-modify-write guarantees in the store, race conditions may occur between
		// this update operation and one from the agent informer after receiving the event from the source.
		lastCsr, exists, err := c.watcherStore.Get(name)
		if err != nil {
			returnErr := errors.NewInternalError(err)
			return nil, returnErr
		}
		if !exists {
			returnErr := errors.NewNotFound(common.CSRGR, name)
			return nil, returnErr
		}
		lastResourceVersion, err := strconv.ParseInt(lastCsr.GetResourceVersion(), 10, 64)
		if err != nil {
			returnErr := errors.NewInternalError(err)
			return nil, returnErr
		}
		newResourceVersion, err := strconv.ParseInt(newCsr.GetResourceVersion(), 10, 64)
		if err != nil {
			returnErr := errors.NewInternalError(err)
			return nil, returnErr
		}
		// ensure the resource version of the csr is not outdated
		if newResourceVersion < lastResourceVersion {
			// It's safe to return a conflict error here, even if the status update event
			// has already been sent. The source may reject the update due to an outdated resource version.
			returnErr := errors.NewConflict(common.CSRGR, name, fmt.Errorf("the resource version of the csr is outdated"))
			return nil, returnErr
		}
		if err := c.watcherStore.Update(newCsr); err != nil {
			returnErr := errors.NewInternalError(err)
			return nil, returnErr
		}

		return newCsr, nil
	}

	// the finalizers of a deleting csr are removed, marking the csr status to deleted and sending
	// it back to source
	// if !newCsr.DeletionTimestamp.IsZero() && len(newCsr.Finalizers) == 0 {
	// 	meta.SetStatusCondition(&newCsr.Status.Conditions, metav1.Condition{
	// 		Type:    common.ConditionDeleted,
	// 		Status:  metav1.ConditionTrue,
	// 		Reason:  "CertificateSigningRequestDeleted",
	// 		Message: fmt.Sprintf("The manifests are deleted from the cluster %s", newCsr.Namespace),
	// 	})

	// 	eventType.Action = common.DeleteRequestAction
	// 	if err := c.cloudEventsClient.Publish(ctx, eventType, newCsr); err != nil {
	// 		returnErr := cloudeventserrors.NewPublishError(common.CSRGR, name, err)
	// 		return nil, returnErr
	// 	}

	// 	if err := c.watcherStore.Delete(newCsr); err != nil {
	// 		returnErr := errors.NewInternalError(err)
	// 		return nil, returnErr
	// 	}

	// 	return newCsr, nil
	// }

	if err := c.watcherStore.Update(newCsr); err != nil {
		returnErr := errors.NewInternalError(err)
		return nil, returnErr
	}

	return newCsr, nil
}

func (c *CSRAgentClient) Apply(ctx context.Context, certificateSigningRequest *certificatesv1.CertificateSigningRequestApplyConfiguration, opts metav1.ApplyOptions) (*certificatev1.CertificateSigningRequest, error) {
	return nil, errors.NewMethodNotSupported(common.CSRGR, "apply")
}

func (c *CSRAgentClient) ApplyStatus(ctx context.Context, certificateSigningRequest *certificatesv1.CertificateSigningRequestApplyConfiguration, opts metav1.ApplyOptions) (*certificatev1.CertificateSigningRequest, error) {
	return nil, errors.NewMethodNotSupported(common.CSRGR, "applystatus")
}

func (c *CSRAgentClient) WatchStatus(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	return nil, errors.NewMethodNotSupported(common.CSRGR, "watchstatus")
}

func (c *CSRAgentClient) UpdateApproval(ctx context.Context, certificateSigningRequestName string, certificateSigningRequest *certificatev1.CertificateSigningRequest, opts metav1.UpdateOptions) (*certificatev1.CertificateSigningRequest, error) {
	return nil, errors.NewMethodNotSupported(common.CSRGR, "updateapproval")
}

func isStatusUpdate(subresources []string) (bool, error) {
	if len(subresources) == 0 {
		return false, nil
	}

	if len(subresources) == 1 && subresources[0] == "status" {
		return true, nil
	}

	return false, fmt.Errorf("unsupported subresources %v", subresources)
}
