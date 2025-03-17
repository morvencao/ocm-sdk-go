package store

import (
	"context"
	"time"

	certificatev1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog/v2"

	"open-cluster-management.io/sdk-go/pkg/cloudevents/generic/types"
)

const syncedPollPeriod = 100 * time.Millisecond

// StoreInitiated is a function that can be used to determine if a store has initiated.
type StoreInitiated func() bool

// CSRClientWatcherStore provides a watcher with a csr store.
type CSRClientWatcherStore interface {
	// GetWatcher returns a watcher to receive csr changes.
	GetWatcher(opts metav1.ListOptions) (watch.Interface, error)

	// HandleReceivedCSR handles the client received csr events.
	HandleReceivedCSR(action types.ResourceAction, csr *certificatev1.CertificateSigningRequest) error

	// Add will be called by csr client when adding csr. The implementation is based on the specific
	// watcher store, in some case, it does not need to update a store, but just send a watch event.
	Add(csr *certificatev1.CertificateSigningRequest) error

	// Update will be called by csr client when updating csr. The implementation is based on the specific
	// watcher store, in some case, it does not need to update a store, but just send a watch event.
	Update(csr *certificatev1.CertificateSigningRequest) error

	// Delete will be called by csr client when deleting csr. The implementation is based on the specific
	// watcher store, in some case, it does not need to update a store, but just send a watch event.
	Delete(csr *certificatev1.CertificateSigningRequest) error

	// List returns the csrs from store with list options
	List(opts metav1.ListOptions) (*certificatev1.CertificateSigningRequestList, error)

	// ListAll list all of the csrs from store
	ListAll() ([]*certificatev1.CertificateSigningRequest, error)

	// Get returns a csr from store with csr name
	Get(name string) (*certificatev1.CertificateSigningRequest, bool, error)

	// HasInitiated marks the store has been initiated, A resync may be required after the store is initiated
	// when building a csr client.
	HasInitiated() bool
}

func WaitForStoreInit(ctx context.Context, cacheSyncs ...StoreInitiated) bool {
	err := wait.PollUntilContextCancel(
		ctx,
		syncedPollPeriod,
		true,
		func(ctx context.Context) (bool, error) {
			for _, syncFunc := range cacheSyncs {
				if !syncFunc() {
					return false, nil
				}
			}
			return true, nil
		},
	)
	if err != nil {
		klog.Errorf("stop WaitForStoreInit, %v", err)
		return false
	}

	return true
}
