// Copyright 2018 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package v2_test

import (
	"context"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	xdsapi "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoy_api_v2_core1 "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	"google.golang.org/grpc"

	"istio.io/istio/pilot/pkg/bootstrap"
	"istio.io/istio/pilot/pkg/proxy/envoy/v2"

	"istio.io/istio/pilot/pkg/proxy/envoy/v1/mock"

	"fmt"

	"istio.io/istio/tests/util"
)

func connect(t *testing.T) xdsapi.EndpointDiscoveryService_StreamEndpointsClient {
	conn, err := grpc.Dial(util.MockPilotGrpcAddr, grpc.WithInsecure())
	if err != nil {
		t.Fatal("Connection failed", err)
	}

	xds := xdsapi.NewEndpointDiscoveryServiceClient(conn)
	edsstr, err := xds.StreamEndpoints(context.Background())
	if err != nil {
		t.Fatal("Rpc failed", err)
	}
	err = edsstr.Send(&xdsapi.DiscoveryRequest{
		Node: &envoy_api_v2_core1.Node{
			Id: "sidecar~a~b~c",
		},
		ResourceNames: []string{"hello.default.svc.cluster.local|http"}})
	if err != nil {
		t.Fatal("Send failed", err)
	}
	return edsstr
}

func reconnect(server *bootstrap.Server, res *xdsapi.DiscoveryResponse, t *testing.T) xdsapi.EndpointDiscoveryService_StreamEndpointsClient {
	conn, err := grpc.Dial(util.MockPilotGrpcAddr, grpc.WithInsecure())
	if err != nil {
		t.Fatal("Connection failed", err)
	}

	xds := xdsapi.NewEndpointDiscoveryServiceClient(conn)
	edsstr, err := xds.StreamEndpoints(context.Background())
	if err != nil {
		t.Fatal("Rpc failed", err)
	}
	err = edsstr.Send(&xdsapi.DiscoveryRequest{
		Node: &envoy_api_v2_core1.Node{
			Id: "sidecar~a~b~c",
		},
		VersionInfo:   res.VersionInfo,
		ResponseNonce: res.Nonce,
		ResourceNames: []string{"hello.default.svc.cluster.local|http"}})
	if err != nil {
		t.Fatal("Send failed", err)
	}
	return edsstr
}

// Regression for envoy restart and overlapping connections
func TestReconnectWithNonce(t *testing.T) {
	server := initLocalPilotTestEnv()
	edsstr := connect(t)
	res, _ := edsstr.Recv()

	// closes old process
	_ = edsstr.CloseSend()

	edsstr = reconnect(server, res, t)
	res, _ = edsstr.Recv()
	_ = edsstr.CloseSend()

	t.Log("Received ", res)
}

// Regression for envoy restart and overlapping connections
func TestReconnect(t *testing.T) {
	initLocalPilotTestEnv()
	edsstr := connect(t)
	_, _ = edsstr.Recv()

	// envoy restarts and reconnects
	edsstr2 := connect(t)
	_, _ = edsstr2.Recv()

	// closes old process
	_ = edsstr.CloseSend()

	time.Sleep(1 * time.Second)

	// event happens
	v2.PushAll()
	// will trigger recompute and push (we may need to make a change once diff is implemented

	done := make(chan struct{}, 1)
	go func() {
		t := time.NewTimer(3 * time.Second)
		select {
		case <-t.C:
			_ = edsstr2.CloseSend()
		case <-done:
			if !t.Stop() {
				<-t.C
			}
		}
	}()

	m, err := edsstr2.Recv()
	if err != nil {
		t.Fatal("Recv failed", err)
	}
	t.Log("Received ", m)
}

// Make a direct EDS grpc request to pilot, verify the result is as expected.
func directRequest(server *bootstrap.Server, t *testing.T) {
	edsstr := connect(t)

	res1, err := edsstr.Recv()
	if err != nil {
		t.Fatal("Recv failed", err)
	}

	if res1.TypeUrl != "type.googleapis.com/envoy.api.v2.ClusterLoadAssignment" {
		t.Error("Expecting type.googleapis.com/envoy.api.v2.ClusterLoadAssignment got ", res1.TypeUrl)
	}
	if res1.Resources[0].TypeUrl != "type.googleapis.com/envoy.api.v2.ClusterLoadAssignment" {
		t.Error("Expecting type.googleapis.com/envoy.api.v2.ClusterLoadAssignment got ", res1.Resources[0].TypeUrl)
	}
	cla := &xdsapi.ClusterLoadAssignment{}
	err = cla.Unmarshal(res1.Resources[0].Value)
	if err != nil {
		t.Fatal("Failed to parse proto ", err)
	}
	// TODO: validate VersionInfo and nonce once we settle on a scheme

	ep := cla.Endpoints
	if len(ep) == 0 {
		t.Fatal("No endpoints")
	}
	lbe := ep[0].LbEndpoints
	if len(lbe) == 0 {
		t.Fatal("No lb endpoints")
	}
	if "10.1.1.0" != lbe[0].Endpoint.Address.GetSocketAddress().Address {
		t.Error("Expecting 10.1.1.10 got ", lbe[0].Endpoint.Address.GetSocketAddress().Address)
	}
	t.Log(cla.String(), res1.String())

	server.MemoryServiceDiscovery.AddService("hello2.default.svc.cluster.local",
		mock.MakeService("hello2.default.svc.cluster.local", "10.1.1.1"))

	v2.PushAll() // will trigger recompute and push
	// This should happen in 15 seconds, for the periodic refresh
	// TODO: verify push works
	res1, err = edsstr.Recv()
	if err != nil {
		t.Fatal("Recv2 failed", err)
	}
	t.Log(res1.String())

	// Need to run the debug test before we close - close will remove the cluster since
	// nobody is watching.
	testEdsz(t)

	_ = edsstr.CloseSend()
}

func TestEds(t *testing.T) {
	initEnvoyTestEnv(t)
	server := util.EnsureTestServer()

	server.MemoryServiceDiscovery.AddService("hello2.default.svc.cluster.local",
		mock.MakeService("hello2.default.svc.cluster.local", "10.12.0.0"))

	// Verify services are set
	srv, err := server.ServiceController.Services()
	if err != nil {
		t.Fatal("Listing services", err)
	}
	log.Println(srv)

	//err := util.RunEnvoy("xds", "tests/testdata/envoy_local.json")
	//if err != nil {
	//	t.Error("Failed to start envoy", err)
	//}

	t.Run("DirectRequest", func(t *testing.T) {
		directRequest(server, t)
	})

}

// Verify the endpoint debug interface is installed and returns some string.
// TODO: parse response, check if data captured matches what we expect.
// TODO: use this in integration tests.
// TODO: refine the output
// TODO: dump the ServiceInstances as well
func testEdsz(t *testing.T) {
	edszURL := fmt.Sprintf("http://localhost:%d/debug/edsz", testEnv.Ports().PilotHTTPPort)
	res, err := http.Get(edszURL)
	if err != nil {
		t.Fatalf("Failed to fetch /edsz")
	}
	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("Failed to read /edsz")
	}
	statusStr := string(data)
	if !strings.Contains(statusStr, "\"hello.default.svc.cluster.local|http\"") {
		t.Fatal("Mock hello service not found ", statusStr)
	}
}
