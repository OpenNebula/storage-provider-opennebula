/*
Copyright 2025, OpenNebula Project, OpenNebula Systems.

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

package driver

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"k8s.io/klog/v2"
)

const (
	udsProtocol = "unix"
	tcpProtocol = "tcp"
)

type GRPCServer struct {
	endpoint string
	listener net.Listener
	server   *grpc.Server
	wg       sync.WaitGroup
}

func NewGRPCServer() *GRPCServer {
	return &GRPCServer{}
}

// Server lifecycle
func (s *GRPCServer) Start(endpoint string, identityServer *IdentityServer, nodeServer *NodeServer, controllerServer *ControllerServer) {
	s.wg.Add(1)

	go s.run(endpoint, identityServer, nodeServer, controllerServer)
}

func (s *GRPCServer) Wait() {
	s.wg.Wait()
}

// Stop attempts a graceful shutdown of the GRPC server, and will force stop if the context is canceled or times out.
func (s *GRPCServer) Stop(ctx context.Context) {
	stopped := make(chan struct{})
	go func() {
		s.server.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
		// GracefulStop finished
	case <-ctx.Done():
		// Timeout or cancellation, force stop
		s.server.Stop()
	}

	err := s.listener.Close()
	if err != nil {
		klog.Warningf("error closing listener: %s", err)
	}
}

func (s *GRPCServer) ForceStop() {
	s.server.Stop()
	s.listener.Close()
}

func (s *GRPCServer) run(endpoint string, identityServer *IdentityServer, nodeServer *NodeServer, controllerServer *ControllerServer) {
	defer s.wg.Done()

	s.endpoint = endpoint

	protocol, address, err := parseGRPCServerEndpoint(endpoint)
	if err != nil {
		klog.Fatalf("could not initialize GRPC server: %s", err)
	}

	listener, err := net.Listen(protocol, address)
	if err != nil {
		klog.Fatalf("could not listen on %s: %s", address, err)
	}
	s.listener = listener

	serverOptions := []grpc.ServerOption{
		grpc.UnaryInterceptor(logInterceptor),
	}
	s.server = grpc.NewServer(serverOptions...)

	if identityServer != nil {
		csi.RegisterIdentityServer(s.server, identityServer)
	}

	if nodeServer != nil {
		csi.RegisterNodeServer(s.server, nodeServer)
	}

	if controllerServer != nil {
		csi.RegisterControllerServer(s.server, controllerServer)
	}

	if s.server != nil {
		reflection.Register(s.server)
	}

	klog.Infof("Starting GRPC server on %s ...", address)
	if err := s.server.Serve(listener); err != nil {
		klog.Fatalf("GRPC server failed: %s", err)
	}
}

func parseGRPCServerEndpoint(endpoint string) (string, string, error) {

	endpointUrl, err := url.Parse(endpoint)
	if err != nil {
		return "", "", fmt.Errorf("invalid grpc endpoint: %s", err)
	}

	protocol := endpointUrl.Scheme
	address := endpointUrl.Host + endpointUrl.Path
	switch protocol {
	case udsProtocol:
		if address == "" {
			return "", "", fmt.Errorf("invalid Unix Domain Socket path: %s", err)
		}
	case tcpProtocol:
		if !strings.Contains(address, ":") {
			return "", "", fmt.Errorf("invalid TCP address, port required: %s", err)
		}
	default:
		return "", "", fmt.Errorf("grpc endpoint protocol not supported: %s", protocol)
	}

	return protocol, address, nil
}

func logInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {

	klog.V(3).Infof("GRPC call: %s", info.FullMethod)
	klog.V(5).Infof("GRPC request: %s", protosanitizer.StripSecrets(req))

	resp, err := handler(ctx, req)
	if err != nil {
		klog.Errorf("GRPC error: %v", err)
	} else {
		klog.V(5).Infof("GRPC response: %s", protosanitizer.StripSecrets(resp))
	}
	return resp, err
}
