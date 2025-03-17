package csr

import (
	"context"
	"fmt"

	certificatev1 "k8s.io/api/certificates/v1"
	kubeclientset "k8s.io/client-go/kubernetes"
	certificatev1client "k8s.io/client-go/kubernetes/typed/certificates/v1"
	"k8s.io/klog/v2"

	csrclient "open-cluster-management.io/sdk-go/pkg/cloudevents/csr/client"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/csr/internal"
	clusterlister "open-cluster-management.io/sdk-go/pkg/cloudevents/csr/lister"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/csr/statushash"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/csr/store"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/generic"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/generic/types"
)

// ClientHolder holds a csr client that implements the CSRInterface based on different configuration
//
// ClientHolder also implements the CertificateSigningRequestsGetter interface.
type ClientHolder struct {
	kubeClientSet kubeclientset.Interface
}

var _ certificatev1client.CertificateSigningRequestsGetter = &ClientHolder{}

// kubeClientInterface returns a kubeclientset Interface
func (h *ClientHolder) kubeClientInterface() kubeclientset.Interface {
	return h.kubeClientSet
}

// CertificateSigningRequests returns a certificatev1client Interface
func (h *ClientHolder) CertificateSigningRequests() certificatev1client.CertificateSigningRequestInterface {
	return h.kubeClientSet.CertificatesV1().CertificateSigningRequests()
}

// ClientHolderBuilder builds the ClientHolder with different configuration.
type ClientHolderBuilder struct {
	config       any
	watcherStore store.CSRClientWatcherStore
	codec        generic.Codec[*certificatev1.CertificateSigningRequest]
	clusterName  string
	clientID     string
	resync       bool
}

// NewClientHolderBuilder returns a ClientHolderBuilder with a given configuration.
//
// Available configurations:
//   - MQTTOptions (*mqtt.MQTTOptions): builds a csr client based on cloudevents with MQTT
//   - GRPCOptions (*grpc.GRPCOptions): builds a csr client based on cloudevents with GRPC
//   - KafkaOptions (*kafka.KafkaOptions): builds a csr client based on cloudevents with Kafka
//
// TODO using a specified config instead of any
func NewClientHolderBuilder(config any) *ClientHolderBuilder {
	return &ClientHolderBuilder{
		config: config,
		resync: true,
	}
}

// WithClientID set the client ID for source/agent cloudevents client.
func (b *ClientHolderBuilder) WithClientID(clientID string) *ClientHolderBuilder {
	b.clientID = clientID
	return b
}

// WithClusterName set the managed cluster name when building a csr client for an agent.
func (b *ClientHolderBuilder) WithClusterName(clusterName string) *ClientHolderBuilder {
	b.clusterName = clusterName
	return b
}

// WithCodec add codec when building a csr client based on cloudevents.
func (b *ClientHolderBuilder) WithCodec(codec generic.Codec[*certificatev1.CertificateSigningRequest]) *ClientHolderBuilder {
	b.codec = codec
	return b
}

// WithCSRClientWatcherStore set the CSRClientWatcherStore. The client will use this store to caches the csrs and
// watch the csr events.
func (b *ClientHolderBuilder) WithCSRClientWatcherStore(store store.CSRClientWatcherStore) *ClientHolderBuilder {
	b.watcherStore = store
	return b
}

// WithResyncEnabled control the client resync (Default is true), if it's true, the resync happens when
//  1. after the client's store is initiated
//  2. the client reconnected
func (b *ClientHolderBuilder) WithResyncEnabled(resync bool) *ClientHolderBuilder {
	b.resync = resync
	return b
}

// NewAgentClientHolder returns a ClientHolder for an agent
func (b *ClientHolderBuilder) NewAgentClientHolder(ctx context.Context) (*ClientHolder, error) {
	if len(b.clientID) == 0 {
		return nil, fmt.Errorf("client id is required")
	}

	if len(b.clusterName) == 0 {
		return nil, fmt.Errorf("cluster name is required")
	}

	if b.watcherStore == nil {
		return nil, fmt.Errorf("watcher store is required")
	}

	options, err := generic.BuildCloudEventsAgentOptions(b.config, b.clusterName, b.clientID)
	if err != nil {
		return nil, err
	}

	cloudEventsClient, err := generic.NewCloudEventAgentClient[*certificatev1.CertificateSigningRequest](
		ctx,
		options,
		clusterlister.NewWatcherStoreLister(b.watcherStore),
		statushash.CSRStatusHash,
		b.codec,
	)
	if err != nil {
		return nil, err
	}

	// start to subscribe
	cloudEventsClient.Subscribe(ctx, b.watcherStore.HandleReceivedCSR)

	csrClient := csrclient.NewCSRAgentClient(cloudEventsClient, b.watcherStore, b.clusterName)
	certificateClient := &internal.CertificateSigningRequestV1ClientWrapper{CertificateSigningRequestClient: csrClient}
	certificateClientSet := &internal.KubeClientSetWrapper{CertificateSigningRequestV1ClientWrapper: certificateClient}

	// start a go routine to receive client reconnect signal
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-cloudEventsClient.ReconnectedChan():
				if !b.resync {
					klog.V(4).Infof("resync is disabled, do nothing")
					continue
				}

				// when receiving a client reconnected signal, we resync all sources for this agent
				// TODO after supporting multiple sources, we should only resync agent known sources
				if err := cloudEventsClient.Resync(ctx, types.SourceAll); err != nil {
					klog.Errorf("failed to send resync request, %v", err)
				}
			}
		}
	}()

	if !b.resync {
		return &ClientHolder{kubeClientSet: certificateClientSet}, nil
	}

	// start a go routine to resync the csrs after this client's store is initiated
	go func() {
		if store.WaitForStoreInit(ctx, b.watcherStore.HasInitiated) {
			if err := cloudEventsClient.Resync(ctx, types.SourceAll); err != nil {
				klog.Errorf("failed to send resync request, %v", err)
			}
		}
	}()

	return &ClientHolder{kubeClientSet: certificateClientSet}, nil
}
