//nolint:dupl
package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	schemaConnectV1 "buf.build/gen/go/open-feature/flagd/bufbuild/connect-go/schema/v1/schemav1connect"
	"github.com/open-feature/flagd/core/pkg/eval"
	"github.com/open-feature/flagd/core/pkg/logger"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/cors"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

const ErrorPrefix = "FlagdError:"

type ConnectService struct {
	Logger                      *logger.Logger
	Eval                        eval.IEvaluator
	ConnectServiceConfiguration *ConnectServiceConfiguration
	eventingConfiguration       *eventingConfiguration
	server                      http.Server
	metrics                     FlagEvaluationRecorder
}
type ConnectServiceConfiguration struct {
	Port             int32
	MetricsPort      int32
	ServerCertPath   string
	ServerKeyPath    string
	ServerSocketPath string
	CORS             []string
}

func (s *ConnectService) Serve(ctx context.Context, eval eval.IEvaluator, svcConf Configuration) error {
	s.Eval = eval
	s.eventingConfiguration = &eventingConfiguration{
		subs: make(map[interface{}]chan Notification),
		mu:   &sync.RWMutex{},
	}
	lis, err := s.setupServer(svcConf)
	if err != nil {
		return err
	}

	errChan := make(chan error, 1)
	go func() {
		if s.ConnectServiceConfiguration.ServerCertPath != "" && s.ConnectServiceConfiguration.ServerKeyPath != "" {
			if err := s.server.ServeTLS(
				lis,
				s.ConnectServiceConfiguration.ServerCertPath,
				s.ConnectServiceConfiguration.ServerKeyPath,
			); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errChan <- err
			}
		} else {
			if err := s.server.Serve(
				lis,
			); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errChan <- err
			}
		}
		close(errChan)
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return s.server.Shutdown(ctx)
	}
}

func (s *ConnectService) setupServer(svcConf Configuration) (net.Listener, error) {
	var lis net.Listener
	var err error
	mux := http.NewServeMux()
	if s.ConnectServiceConfiguration.ServerSocketPath != "" {
		lis, err = net.Listen("unix", s.ConnectServiceConfiguration.ServerSocketPath)
	} else {
		address := fmt.Sprintf(":%d", s.ConnectServiceConfiguration.Port)
		fmt.Println(address)
		lis, err = net.Listen("tcp", address)
	}
	if err != nil {
		return nil, err
	}
	path, handler := schemaConnectV1.NewServiceHandler(s)
	mux.Handle(path, handler)
	exporter, err := prometheus.New()
	if err != nil {
		return nil, err
	}

	mdlw := New(middlewareConfig{
		Service:      "openfeature/flagd",
		MetricReader: exporter,
		Logger:       s.Logger,
	})
	s.metrics = mdlw
	h := Handler("", mdlw, mux)

	go bindMetrics(s, svcConf)

	if s.ConnectServiceConfiguration.ServerCertPath != "" && s.ConnectServiceConfiguration.ServerKeyPath != "" {
		handler = s.newCORS().Handler(h)
	} else {
		handler = h2c.NewHandler(
			s.newCORS().Handler(h),
			&http2.Server{},
		)
	}
	s.server = http.Server{
		ReadHeaderTimeout: time.Second,
		Handler:           handler,
	}
	return lis, nil
}

func (s *ConnectService) Notify(n Notification) {
	s.eventingConfiguration.mu.RLock()
	defer s.eventingConfiguration.mu.RUnlock()
	for _, send := range s.eventingConfiguration.subs {
		send <- n
	}
}

func (s *ConnectService) newCORS() *cors.Cors {
	return cors.New(cors.Options{
		AllowedMethods: []string{
			http.MethodHead,
			http.MethodGet,
			http.MethodPost,
			http.MethodPut,
			http.MethodPatch,
			http.MethodDelete,
		},
		AllowedOrigins: s.ConnectServiceConfiguration.CORS,
		AllowedHeaders: []string{"*"},
		ExposedHeaders: []string{
			// Content-Type is in the default safelist.
			"Accept",
			"Accept-Encoding",
			"Accept-Post",
			"Connect-Accept-Encoding",
			"Connect-Content-Encoding",
			"Content-Encoding",
			"Grpc-Accept-Encoding",
			"Grpc-Encoding",
			"Grpc-Message",
			"Grpc-Status",
			"Grpc-Status-Details-Bin",
		},
	})
}

func bindMetrics(s *ConnectService, svcConf Configuration) {
	s.Logger.Info(fmt.Sprintf("metrics and probes listening at %d", s.ConnectServiceConfiguration.MetricsPort))
	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", s.ConnectServiceConfiguration.MetricsPort),
		ReadHeaderTimeout: 3 * time.Second,
	}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/readyz":
			if svcConf.ReadinessProbe() {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusPreconditionFailed)
			}
		case "/metrics":
			promhttp.Handler().ServeHTTP(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	err := server.ListenAndServe()
	if err != nil {
		panic(err)
	}
}
