package lister

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	certificatev1 "k8s.io/api/certificates/v1"

	"open-cluster-management.io/sdk-go/pkg/cloudevents/common"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/csr/store"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/generic/types"
)

// WatcherStoreLister list the csrs from CSRClientWatcherStore
type WatcherStoreLister struct {
	store store.CSRClientWatcherStore
}

func NewWatcherStoreLister(store store.CSRClientWatcherStore) *WatcherStoreLister {
	return &WatcherStoreLister{
		store: store,
	}
}

// List returns the csrs from a CSRClientWatcherStore with list options
func (l *WatcherStoreLister) List(options types.ListOptions) ([]*certificatev1.CertificateSigningRequest, error) {
	opts := metav1.ListOptions{}

	if options.Source != types.SourceAll {
		opts.LabelSelector = fmt.Sprintf("%s=%s", common.CloudEventsOriginalSourceLabelKey, options.Source)
	}

	list, err := l.store.List(opts)
	if err != nil {
		return nil, err
	}

	csrs := []*certificatev1.CertificateSigningRequest{}
	for _, csr := range list.Items {
		// TODO: check event data type
		csrs = append(csrs, &csr)
	}

	return csrs, nil
}
