package options

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	prom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/cloudevents/sdk-go/v2/binding"
	cloudeventstypes "github.com/cloudevents/sdk-go/v2/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	k8smetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"

	pbv1 "open-cluster-management.io/sdk-go/pkg/cloudevents/generic/options/grpc/protobuf/v1"
	grpcprotocol "open-cluster-management.io/sdk-go/pkg/cloudevents/generic/options/grpc/protocol"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/generic/types"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/server"
	grpcserver "open-cluster-management.io/sdk-go/pkg/cloudevents/server/grpc"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/server/grpc/authn"
	"open-cluster-management.io/sdk-go/pkg/cloudevents/server/grpc/authz"
)

// PreStartHook is an interface to start hook before grpc server is started.
type PreStartHook interface {
	// Start should be a non-blocking call
	Run(ctx context.Context)
}

// create prometheus metrics provider
var promMiddleware = prom.NewServerMetrics(
	prom.WithServerHandlingTimeHistogram(
		prom.WithHistogramBuckets(prometheus.DefBuckets), // or custom slice
	),
)

type Server struct {
	options        *GRPCServerOptions
	authenticators []authn.Authenticator
	authorizers    []authz.Authorizer
	services       map[types.CloudEventsDataType]server.Service
	hooks          []PreStartHook
}

func NewServer(opt *GRPCServerOptions) *Server {
	return &Server{options: opt, services: make(map[types.CloudEventsDataType]server.Service)}
}

func (s *Server) WithAuthenticator(authenticator authn.Authenticator) *Server {
	s.authenticators = append(s.authenticators, authenticator)
	return s
}

func (s *Server) WithAuthorizer(authorizer authz.Authorizer) *Server {
	s.authorizers = append(s.authorizers, authorizer)
	return s
}

func (s *Server) WithService(t types.CloudEventsDataType, service server.Service) *Server {
	s.services[t] = service
	return s
}

func (s *Server) WithPreStartHooks(hooks ...PreStartHook) *Server {
	s.hooks = append(s.hooks, hooks...)
	return s
}

func (s *Server) Run(ctx context.Context) error {
	var grpcServerOptions []grpc.ServerOption
	grpcServerOptions = append(grpcServerOptions, grpc.MaxRecvMsgSize(s.options.MaxReceiveMessageSize))
	grpcServerOptions = append(grpcServerOptions, grpc.MaxSendMsgSize(s.options.MaxSendMessageSize))
	grpcServerOptions = append(grpcServerOptions, grpc.MaxConcurrentStreams(s.options.MaxConcurrentStreams))
	grpcServerOptions = append(grpcServerOptions, grpc.ConnectionTimeout(s.options.ConnectionTimeout))
	grpcServerOptions = append(grpcServerOptions, grpc.WriteBufferSize(s.options.WriteBufferSize))
	grpcServerOptions = append(grpcServerOptions, grpc.ReadBufferSize(s.options.ReadBufferSize))
	grpcServerOptions = append(grpcServerOptions, grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
		MinTime:             s.options.ClientMinPingInterval,
		PermitWithoutStream: s.options.PermitPingWithoutStream,
	}))
	grpcServerOptions = append(grpcServerOptions, grpc.KeepaliveParams(keepalive.ServerParameters{
		MaxConnectionAge: s.options.MaxConnectionAge,
		Time:             s.options.ServerPingInterval,
		Timeout:          s.options.ServerPingTimeout,
	}))

	// Serve with TLS
	serverCerts, err := tls.LoadX509KeyPair(s.options.TLSCertFile, s.options.TLSKeyFile)
	if err != nil {
		return fmt.Errorf("failed to load broker certificates: %v", err)
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{serverCerts},
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
	}

	if s.options.ClientCAFile != "" {
		certPool, err := x509.SystemCertPool()
		if err != nil {
			return fmt.Errorf("failed to load system cert pool: %v", err)
		}
		caPEM, err := os.ReadFile(s.options.ClientCAFile)
		if err != nil {
			return fmt.Errorf("failed to read broker client CA file: %v", err)
		}
		if ok := certPool.AppendCertsFromPEM(caPEM); !ok {
			return fmt.Errorf("failed to append broker client CA to cert pool")
		}
		tlsConfig.ClientCAs = certPool
		tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
	}

	grpcServerOptions = append(grpcServerOptions, grpc.Creds(credentials.NewTLS(tlsConfig)))
	grpcServerOptions = append(grpcServerOptions, grpc.StatsHandler(&grpcMetricsHandler{}))

	grpcServerOptions = append(grpcServerOptions,
		grpc.ChainUnaryInterceptor(
			promMiddleware.UnaryServerInterceptor(),
			newMetricsUnaryInterceptor(),
			newAuthnUnaryInterceptor(s.authenticators...),
			newAuthzUnaryInterceptor(s.authorizers...)),
		grpc.ChainStreamInterceptor(
			promMiddleware.StreamServerInterceptor(),
			newMetricsStreamInterceptor(),
			newAuthnStreamInterceptor(s.authenticators...),
			newAuthzStreamInterceptor(s.authorizers...)))

	grpcServer := grpc.NewServer(grpcServerOptions...)
	// initialize grpc metrics and expose them
	promMiddleware.InitializeMetrics(grpcServer)
	grpcEventServer := grpcserver.NewGRPCBroker(grpcServer)

	for t, service := range s.services {
		grpcEventServer.RegisterService(t, service)
	}

	// start hook
	for _, hook := range s.hooks {
		hook.Run(ctx)
	}

	go grpcEventServer.Start(ctx, ":"+s.options.ServerBindPort)
	<-ctx.Done()
	return nil
}

func newAuthnUnaryInterceptor(authenticators ...authn.Authenticator) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		var err error
		for _, authenticator := range authenticators {
			ctx, err = authenticator.Authenticate(ctx)
			if err == nil {
				return handler(ctx, req)
			}
		}

		if err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

func newAuthzUnaryInterceptor(authorizers ...authz.Authorizer) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		for _, authorizer := range authorizers {
			pReq, ok := req.(*pbv1.PublishRequest)
			if !ok {
				return nil, fmt.Errorf("unsupported request type %T", req)
			}

			eventsType, err := types.ParseCloudEventsType(pReq.Event.Type)
			if err != nil {
				return nil, err
			}

			// the event of grpc publish request is the original cloudevent data, we need a `ce-` prefix
			// to get the event attribute
			clusterAttr, ok := pReq.Event.Attributes[fmt.Sprintf("ce-%s", types.ExtensionClusterName)]
			if !ok {
				return nil, fmt.Errorf("missing ce-clustername in event attributes, %v", pReq.Event.Attributes)
			}

			if err := authorizer.Authorize(ctx, clusterAttr.GetCeString(), *eventsType); err != nil {
				return nil, err
			}
		}

		return handler(ctx, req)
	}
}

// wrappedAuthStream wraps a grpc.ServerStream associated with an incoming RPC, and
// a custom context containing the user and groups derived from the client certificate
// specified in the incoming RPC metadata
type wrappedAuthStream struct {
	grpc.ServerStream
	ctx context.Context
}

// Context returns the context associated with the stream
func (w *wrappedAuthStream) Context() context.Context {
	return w.ctx
}

// newWrappedAuthStream creates a new wrappedAuthStream
func newWrappedAuthStream(ctx context.Context, s grpc.ServerStream) grpc.ServerStream {
	return &wrappedAuthStream{s, ctx}
}

// newAuthnStreamInterceptor creates a stream interceptor that retrieves the user and groups
// based on the specified authentication type. It supports retrieving from either the access
// token or the client certificate depending on the provided authNType.
// The interceptor then adds the retrieved identity information (user and groups) to the
// context and invokes the provided handler.
func newAuthnStreamInterceptor(authenticators ...authn.Authenticator) grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		var err error
		ctx := ss.Context()
		for _, authenticator := range authenticators {
			ctx, err = authenticator.Authenticate(ctx)
			if err == nil {
				return handler(srv, newWrappedAuthStream(ctx, ss))
			}
		}

		if err != nil {
			return err
		}

		return handler(srv, newWrappedAuthStream(ctx, ss))
	}
}

// wrappedAuthorizedStream caches the subscription request that is already read.
type wrappedAuthorizedStream struct {
	sync.Mutex

	grpc.ServerStream
	authorizedReq *pbv1.SubscriptionRequest
}

// RecvMsg set the msg from the cache.
func (c *wrappedAuthorizedStream) RecvMsg(m any) error {
	c.Lock()
	defer c.Unlock()

	msg, ok := m.(*pbv1.SubscriptionRequest)
	if !ok {
		return fmt.Errorf("unsupported request type %T", m)
	}

	msg.ClusterName = c.authorizedReq.ClusterName
	msg.Source = c.authorizedReq.Source
	msg.DataType = c.authorizedReq.DataType
	return nil
}

// newAuthzStreamInterceptor is a stream interceptor that authorizes the subscription request.
func newAuthzStreamInterceptor(authorizers ...authz.Authorizer) grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		if info.IsClientStream {
			return handler(srv, ss)
		}

		var req pbv1.SubscriptionRequest
		if err := ss.RecvMsg(&req); err != nil {
			return err
		}

		eventDataType, err := types.ParseCloudEventsDataType(req.DataType)
		if err != nil {
			return err
		}

		eventsType := types.CloudEventsType{
			CloudEventsDataType: *eventDataType,
			SubResource:         types.SubResourceSpec,
			Action:              types.WatchRequestAction,
		}
		for _, authorizer := range authorizers {
			if err := authorizer.Authorize(ss.Context(), req.ClusterName, eventsType); err != nil {
				return err
			}
		}

		if err := handler(srv, &wrappedAuthorizedStream{ServerStream: ss, authorizedReq: &req}); err != nil {
			klog.Error(err)
			return err
		}

		return nil
	}
}

type grpcMetricsHandler struct{}

func (h *grpcMetricsHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	ctx = context.WithValue(ctx, "fullMethod", info.FullMethodName)
	return ctx
}

func (h *grpcMetricsHandler) HandleRPC(ctx context.Context, s stats.RPCStats) {
	fullMethod, _ := ctx.Value("fullMethod").(string)
	service, method := splitMethod(fullMethod)
	grpcType := rpcType(s)

	switch st := s.(type) {
	case *stats.InPayload:
		grpcServerMsgRevBytes.WithLabelValues(method, service, grpcType).Add(float64(st.Length))
	case *stats.OutPayload:
		grpcServerMsgSentBytes.WithLabelValues(method, service, grpcType).Add(float64(st.Length))
	}
}

// splitMethod parses "/package.service/method"
func splitMethod(fullMethod string) (service, method string) {
	if fullMethod == "" {
		return "unknown", "unknown"
	}
	// Remove leading "/"
	if fullMethod[0] == '/' {
		fullMethod = fullMethod[1:]
	}
	// Split at last "/"
	for i := len(fullMethod) - 1; i >= 0; i-- {
		if fullMethod[i] == '/' {
			return fullMethod[:i], fullMethod[i+1:]
		}
	}
	return fullMethod, "unknown"
}

func rpcType(s stats.RPCStats) string {
	switch st := s.(type) {
	case *stats.Begin:
		// Determine the RPC type by the streaming flags
		if st.IsClientStream && st.IsServerStream {
			return "bidi_stream"
		} else if st.IsClientStream {
			return "client_stream"
		} else if st.IsServerStream {
			return "server_stream"
		}
		return "unary"
	default:
		return "unary" // For InPayload/OutPayload, we fallback to the type detected at Begin
	}
}

func (h *grpcMetricsHandler) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	ctx = context.WithValue(ctx, "remote_addr", info.RemoteAddr.String())
	if info.LocalAddr != nil {
		ctx = context.WithValue(ctx, "local_addr", info.LocalAddr.String())
	}

	return ctx
}

func (h *grpcMetricsHandler) HandleConn(ctx context.Context, s stats.ConnStats) {
	remoteAddr, _ := ctx.Value("remote_addr").(string)
	localAddr, _ := ctx.Value("local_addr").(string)
	if localAddr == "" {
		localAddr = "unknown"
	}

	switch s.(type) {
	case *stats.ConnBegin:
		grpcServerConnections.WithLabelValues(remoteAddr, localAddr).Inc()
	case *stats.ConnEnd:
		grpcServerConnections.WithLabelValues(remoteAddr, localAddr).Dec()
	}
}

func init() {
	// Register the metrics:
	RegisterGRPCMetrics()
}

// NewMetricsUnaryInterceptor creates a unary server interceptor for server metrics.
// Currently supports the Publish method with PublishRequest.
func newMetricsUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		// extract the type from the method name
		methodInfo := strings.Split(info.FullMethod, "/")
		if len(methodInfo) != 3 || methodInfo[2] != "Publish" {
			return nil, fmt.Errorf("invalid method name: %s", info.FullMethod)
		}
		method := methodInfo[2]
		pubReq, ok := req.(*pbv1.PublishRequest)
		if !ok {
			return nil, fmt.Errorf("invalid request type for Publish method")
		}
		// convert the request to cloudevent and extract the source
		evt, err := binding.ToEvent(ctx, grpcprotocol.NewMessage(pubReq.Event))
		if err != nil {
			return nil, fmt.Errorf("failed to convert to cloudevent: %v", err)
		}

		cluster, err := cloudeventstypes.ToString(evt.Context.GetExtensions()[types.ExtensionClusterName])
		if err != nil {
			return nil, fmt.Errorf("failed to get clustername extension: %v", err)
		}

		eventType, err := types.ParseCloudEventsType(evt.Type())
		if err != nil {
			return nil, fmt.Errorf("failed to parse cloud event type %s, %v", evt.Type(), err)
		}
		dataType := eventType.CloudEventsDataType.String()

		grpcCECalledCountMetric.WithLabelValues(cluster, dataType, method).Inc()
		grpcCEMessageReceivedCountMetric.WithLabelValues(cluster, dataType, method).Inc()
		startTime := time.Now()
		resp, err := handler(ctx, req)
		duration := time.Since(startTime).Seconds()
		grpcCEMessageSentCountMetric.WithLabelValues(cluster, dataType, method).Inc()

		// get status code from error
		status := statusFromError(err)
		code := status.Code()
		grpcCEProcessedCountMetric.WithLabelValues(cluster, dataType, method, code.String()).Inc()
		grpcCEProcessedDurationMetric.WithLabelValues(cluster, dataType, method, code.String()).Observe(duration)

		return resp, err
	}
}

// wrappedMetricsStream wraps a grpc.ServerStream, capturing the request source
// emitting metrics for the stream interceptor.
type wrappedMetricsStream struct {
	clusterName *string
	dataType    *string
	method      string
	grpc.ServerStream
	ctx context.Context
}

// RecvMsg wraps the RecvMsg method of the embedded grpc.ServerStream.
// It captures the cluster and data type from the SubscriptionRequest and emits metrics.
func (w *wrappedMetricsStream) RecvMsg(m interface{}) error {
	err := w.ServerStream.RecvMsg(m)
	subReq, ok := m.(*pbv1.SubscriptionRequest)
	if !ok {
		return fmt.Errorf("invalid request type for Subscribe method")
	}
	*w.clusterName = subReq.ClusterName
	*w.dataType = subReq.DataType
	grpcCECalledCountMetric.WithLabelValues(*w.clusterName, *w.dataType, w.method).Inc()
	grpcCEMessageReceivedCountMetric.WithLabelValues(*w.clusterName, *w.dataType, w.method).Inc()

	return err
}

// SendMsg wraps the SendMsg method of the embedded grpc.ServerStream.
func (w *wrappedMetricsStream) SendMsg(m interface{}) error {
	err := w.ServerStream.SendMsg(m)
	grpcCEMessageSentCountMetric.WithLabelValues(*w.clusterName, *w.dataType, w.method).Inc()
	return err
}

// newWrappedMetricsStream creates a wrappedMetricsStream with the specified type and cluster reference.
func newWrappedMetricsStream(clusterName, dataType *string, method string, ctx context.Context, ss grpc.ServerStream) grpc.ServerStream {
	return &wrappedMetricsStream{clusterName, dataType, method, ss, ctx}
}

// newMetricsStreamInterceptor creates a stream server interceptor for server metrics.
// Currently supports the Subscribe method with SubscriptionRequest.
func newMetricsStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		// extract the type from the method name
		if !info.IsServerStream || info.IsClientStream {
			return fmt.Errorf("invalid stream type for stream method: %s", info.FullMethod)
		}
		methodInfo := strings.Split(info.FullMethod, "/")
		if len(methodInfo) != 3 || methodInfo[2] != "Subscribe" {
			return fmt.Errorf("invalid method name for stream method: %s", info.FullMethod)
		}
		method := methodInfo[2]
		dataType := ""
		cluster := ""
		// create a wrapped stream to capture the source and emit metrics
		wrappedMetricsStream := newWrappedMetricsStream(&cluster, &dataType, method, stream.Context(), stream)
		err := handler(srv, wrappedMetricsStream)

		// get status code from error
		status := statusFromError(err)
		code := status.Code()
		grpcCEProcessedCountMetric.WithLabelValues(cluster, dataType, method, code.String()).Inc()

		return err
	}
}

// statusFromError returns a grpc status. If the error code is neither a valid grpc status
// nor a context error, codes.Unknown will be set.
func statusFromError(err error) *status.Status {
	s, ok := status.FromError(err)
	// Mirror what the grpc server itself does, i.e. also convert context errors to status
	if !ok {
		s = status.FromContextError(err)
	}
	return s
}

// Subsystem used to define the metrics for grpc:
const (
	grpcMetricsSubsystem   = "grpc_server"
	grpcCEMetricsSubsystem = "grpc_server_ce"
)

// Names of the labels added to metrics:
const (
	grpcMetricsMethodLabel     = "grpc_method"
	grpcMetricsServiceLabel    = "grpc_service"
	grpcMetricsTypeLabel       = "grpc_type"
	grpcCEMetricsCodeLabel     = "grpc_code"
	grpcCEMetricsClusterLabel  = "cluster"
	grpcCEMetricsDataTypeLabel = "data_type"
	grpcCEMetricsMethodLabel   = "method"
)

// grpcMetricsLabels - Array of labels added to grpc server metrics:
var grpcMetricsLabels = []string{
	grpcMetricsMethodLabel,
	grpcMetricsServiceLabel,
	grpcMetricsTypeLabel,
}

// grpcMetricsAllLabels - Array of all labels added to grpc server metrics:
// var grpcMetricsAllLabels = []string{
// 	grpcMetricsMethodLabel,
// 	grpcMetricsServiceLabel,
// 	grpcMetricsTypeLabel,
// 	grpcCEMetricsCodeLabel,
// }

// grpcCEMetricsLabels - Array of labels added to grpc server metrics for cloudevents:
var grpcCEMetricsLabels = []string{
	grpcCEMetricsClusterLabel,
	grpcCEMetricsDataTypeLabel,
	grpcCEMetricsMethodLabel,
}

// grpcCEMetricsAllLabels - Array of all labels added to grpc server metrics for cloudevents:
var grpcCEMetricsAllLabels = []string{
	grpcCEMetricsClusterLabel,
	grpcCEMetricsDataTypeLabel,
	grpcCEMetricsMethodLabel,
	grpcCEMetricsCodeLabel,
}

// Names of the grpc server metrics:
const (
	activeConnectionsMetric = "active_connections"
	msgRevBytesCountMetric  = "msg_received_bytes_total"
	msgSentBytesCountMetric = "msg_sent_bytes_total"
)

// Names of the grpc server metrics for cloudevents:
const (
	calledCountMetric          = "called_total"
	processedCountMetric       = "processed_total"
	processedDurationMetric    = "processed_duration_seconds"
	messageReceivedCountMetric = "message_received_total"
	messageSentCountMetric     = "message_sent_total"
)

var grpcServerConnections = k8smetrics.NewGaugeVec(&k8smetrics.GaugeOpts{
	Subsystem:      grpcMetricsSubsystem,
	Name:           activeConnectionsMetric,
	StabilityLevel: k8smetrics.ALPHA,
	Help:           "Current number of active gRPC server connections.",
}, []string{"remote_addr", "local_addr"})

var grpcServerMsgRevBytes = k8smetrics.NewCounterVec(&k8smetrics.CounterOpts{
	Subsystem:      grpcMetricsSubsystem,
	Name:           msgRevBytesCountMetric,
	StabilityLevel: k8smetrics.ALPHA,
	Help:           "Total number of bytes received by the gRPC server.",
}, grpcMetricsLabels)

var grpcServerMsgSentBytes = k8smetrics.NewCounterVec(&k8smetrics.CounterOpts{
	Subsystem:      grpcMetricsSubsystem,
	Name:           msgSentBytesCountMetric,
	StabilityLevel: k8smetrics.ALPHA,
	Help:           "Total number of bytes sent by the gRPC server.",
}, grpcMetricsLabels)

// Description of the gRPC called count metric for for cloudevents:
var grpcCECalledCountMetric = k8smetrics.NewCounterVec(&k8smetrics.CounterOpts{
	Subsystem:      grpcCEMetricsSubsystem,
	Name:           calledCountMetric,
	StabilityLevel: k8smetrics.ALPHA,
	Help:           "Total number of RPC requests for cloudevents called on the grpc server.",
}, grpcCEMetricsLabels)

// Description of the gRPC message received count metric for cloudevents:
var grpcCEMessageReceivedCountMetric = k8smetrics.NewCounterVec(&k8smetrics.CounterOpts{
	Subsystem: grpcCEMetricsSubsystem,
	Name:      messageReceivedCountMetric,
	Help:      "Total number of messages for cloudevents received on the gRPC server.",
}, grpcCEMetricsLabels)

// Description of the gRPC message sent count metric for cloudevents:
var grpcCEMessageSentCountMetric = k8smetrics.NewCounterVec(&k8smetrics.CounterOpts{
	Subsystem: grpcCEMetricsSubsystem,
	Name:      messageSentCountMetric,
	Help:      "Total number of messages for cloudevents sent by the gRPC server.",
}, grpcCEMetricsLabels)

// Description of the gRPC processed count metric for cloudevents:
var grpcCEProcessedCountMetric = k8smetrics.NewCounterVec(&k8smetrics.CounterOpts{
	Subsystem:      grpcCEMetricsSubsystem,
	Name:           processedCountMetric,
	StabilityLevel: k8smetrics.ALPHA,
	Help:           "Total number of RPC requests for cloudevents processed on the server, regardless of success or failure.",
}, grpcCEMetricsAllLabels,
)

// Description of the gRPC processed duration metric for cloudevents:
var grpcCEProcessedDurationMetric = k8smetrics.NewHistogramVec(&k8smetrics.HistogramOpts{
	Subsystem: grpcCEMetricsSubsystem,
	Name:      processedDurationMetric,
	Help:      "Histogram of the duration of RPC requests for cloudevents processed on the server.",
	Buckets:   k8smetrics.ExponentialBuckets(10e-7, 10, 10),
}, grpcCEMetricsAllLabels)

// Register the grpc server metrics:
func RegisterGRPCMetrics() {
	metrics := []k8smetrics.Registerable{
		grpcServerConnections,
		grpcServerMsgRevBytes,
		grpcServerMsgSentBytes,
		grpcCECalledCountMetric,
		grpcCEProcessedCountMetric,
		grpcCEProcessedDurationMetric,
		grpcCEMessageReceivedCountMetric,
		grpcCEMessageSentCountMetric,
	}

	for _, m := range metrics {
		if m != nil {
			legacyregistry.MustRegister(m)
		} else {
			klog.Errorf("failed to register nil grpc server metric %s", m.FQName())
		}
	}
	legacyregistry.RawMustRegister(promMiddleware)
}
