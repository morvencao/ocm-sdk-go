package store

import (
	"fmt"

	certificatev1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"open-cluster-management.io/sdk-go/pkg/cloudevents/generic/types"
)

// AgentInformerWatcherStore extends the baseStore.
// It gets/lists the csrs from the given informer store and send
// the cs add/update/delete event to the watch channel directly.
//
// It is used for building csr agent client.
type AgentInformerWatcherStore struct {
	baseStore
	informer cache.SharedIndexInformer
	watcher  *csrWatcher
}

var _ CSRClientWatcherStore = &AgentInformerWatcherStore{}

func NewAgentInformerWatcherStore() *AgentInformerWatcherStore {
	return &AgentInformerWatcherStore{
		baseStore: baseStore{},
		watcher:   newCSRWatcher(),
	}
}

func (s *AgentInformerWatcherStore) Add(csr *certificatev1.CertificateSigningRequest) error {
	s.watcher.Receive(watch.Event{Type: watch.Added, Object: csr})
	return nil
}

func (s *AgentInformerWatcherStore) Update(csr *certificatev1.CertificateSigningRequest) error {
	s.watcher.Receive(watch.Event{Type: watch.Modified, Object: csr})
	return nil
}

func (s *AgentInformerWatcherStore) Delete(csr *certificatev1.CertificateSigningRequest) error {
	s.watcher.Receive(watch.Event{Type: watch.Deleted, Object: csr})
	return nil
}

func (s *AgentInformerWatcherStore) HandleReceivedCSR(action types.ResourceAction, csr *certificatev1.CertificateSigningRequest) error {
	switch action {
	case types.Added:
		return s.Add(csr.DeepCopy())
	case types.Modified:
		lastCSR, exists, err := s.Get(csr.Name)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("the csr %s does not exist", csr.Name)
		}
		// prevent the csr from being updated if it is deleting
		if !lastCSR.GetDeletionTimestamp().IsZero() {
			klog.Warningf("the csr %s is deleting, ignore the update", csr.Name)
			return nil
		}

		updatedCSR := csr.DeepCopy()

		// restore the fields that are maintained by local agent
		updatedCSR.Labels = lastCSR.Labels
		updatedCSR.Annotations = lastCSR.Annotations
		updatedCSR.Finalizers = lastCSR.Finalizers
		updatedCSR.Status = lastCSR.Status

		return s.Update(updatedCSR)
	case types.Deleted:
		// the csr is deleting on the source, we just update its deletion timestamp.
		lastCSR, exists, err := s.Get(csr.Name)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}

		updatedCSR := lastCSR.DeepCopy()
		updatedCSR.DeletionTimestamp = csr.DeletionTimestamp
		return s.Update(updatedCSR)
	default:
		return fmt.Errorf("unsupported resource action %s", action)
	}
}

func (s *AgentInformerWatcherStore) GetWatcher(opts metav1.ListOptions) (watch.Interface, error) {
	return s.watcher, nil
}

func (s *AgentInformerWatcherStore) HasInitiated() bool {
	return s.initiated && s.informer.HasSynced()
}

func (s *AgentInformerWatcherStore) SetInformer(informer cache.SharedIndexInformer) {
	s.informer = informer
	s.store = informer.GetStore()
	s.initiated = true
}
