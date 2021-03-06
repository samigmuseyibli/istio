// Copyright 2019 Istio Authors
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

package locality

import (
	"bytes"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"testing"
	"text/template"
	"time"

	envoyAdmin "github.com/envoyproxy/go-control-plane/envoy/admin/v2alpha"

	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/environment"
	"istio.io/istio/pkg/test/framework/components/galley"
	"istio.io/istio/pkg/test/framework/components/istio"
	"istio.io/istio/pkg/test/framework/components/namespace"
	"istio.io/istio/pkg/test/framework/components/pilot"
	"istio.io/istio/pkg/test/framework/label"
	"istio.io/istio/pkg/test/framework/resource"
	"istio.io/istio/pkg/test/util/retry"
	"istio.io/istio/pkg/test/util/structpath"
)

const (
	sendCount = 100

	deploymentYAML = `
apiVersion: networking.istio.io/v1alpha3
kind: ServiceEntry
metadata:
  name: {{.Name}}
  namespace: {{.Namespace}}
spec:
  hosts:
  - {{.Host}}
  exportTo:
  - "."
  ports:
  - number: 80
    name: http
    protocol: HTTP
  resolution: {{.Resolution}}
  location: MESH_EXTERNAL
  endpoints:
  {{ if ne .NonExistantService "" }}
  - address: {{.NonExistantService}}
    locality: {{.NonExistantServiceLocality}}
  {{ end }}
  - address: {{.ServiceBAddress}}
    locality: {{.ServiceBLocality}}
  - address: {{.ServiceCAddress}}
    locality: {{.ServiceCLocality}}
---
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: {{.Name}}-route
  namespace: {{.Namespace}}
spec:
  hosts:
  - {{.Host}}
  http:
  - route:
    - destination:
        host: {{.Host}}
    retries:
      attempts: 3
      perTryTimeout: 1s
      retryOn: gateway-error,connect-failure,refused-stream
---
apiVersion: networking.istio.io/v1alpha3
kind: DestinationRule
metadata:
  name: {{.Name}}-destination
  namespace: {{.Namespace}}
spec:
  host: {{.Host}}
  trafficPolicy:
    loadBalancer:
      simple: ROUND_ROBIN
      localityLbSetting:
        failover:
        - from: region
          to: closeregion
    outlierDetection:
      consecutiveErrors: 100
      interval: 1s
      baseEjectionTime: 3m
      maxEjectionPercent: 100
`
)

var (
	bHostnameMatcher   = regexp.MustCompile("^b-.*$")
	deploymentTemplate *template.Template

	ist istio.Instance
	p   pilot.Instance
	g   galley.Instance
	r   *rand.Rand
)

func init() {
	var err error
	deploymentTemplate, err = template.New("localityTemplate").Parse(deploymentYAML)
	if err != nil {
		panic(err)
	}
}

func TestMain(m *testing.M) {
	framework.NewSuite("locality_prioritized_failover_loadbalancing", m).
		Label(label.CustomSetup).
		SetupOnEnv(environment.Kube, istio.Setup(&ist, nil)).
		Setup(func(ctx resource.Context) (err error) {
			if g, err = galley.New(ctx, galley.Config{}); err != nil {
				return err
			}
			if p, err = pilot.New(ctx, pilot.Config{Galley: g}); err != nil {
				return err
			}
			r = rand.New(rand.NewSource(time.Now().UnixNano()))
			return nil
		}).
		Run()
}

func echoConfig(ns namespace.Instance, name string) echo.Config {
	return echo.Config{
		Service:   name,
		Namespace: ns,
		Locality:  "region.zone.subzone",
		Ports: []echo.Port{
			{
				Name:        "http",
				Protocol:    protocol.HTTP,
				ServicePort: 80,
			},
		},
		Galley: g,
		Pilot:  p,
	}
}

type serviceConfig struct {
	Name                       string
	Host                       string
	Namespace                  string
	Resolution                 string
	ServiceBAddress            string
	ServiceBLocality           string
	ServiceCAddress            string
	ServiceCLocality           string
	NonExistantService         string
	NonExistantServiceLocality string
}

func deploy(t test.Failer, ns namespace.Instance, se serviceConfig, from echo.Instance) {
	t.Helper()
	var buf bytes.Buffer
	if err := deploymentTemplate.Execute(&buf, se); err != nil {
		t.Fatal(err)
	}
	g.ApplyConfigOrFail(t, ns, buf.String())

	err := WaitUntilRoute(from, se.Host)
	if err != nil {
		t.Fatalf("Failed to get expected route: %v", err)
	}
}

// Wait for our route for the "fake" target to be established
func WaitUntilRoute(c echo.Instance, dest string) error {
	accept := func(cfg *envoyAdmin.ConfigDump) (bool, error) {
		validator := structpath.ForProto(cfg)
		if err := validator.
			Exists("{.configs[*].dynamicRouteConfigs[*].routeConfig.virtualHosts[?(@.name == '%s')]}", dest+":80").
			Check(); err != nil {
			return false, err
		}
		clusterName := fmt.Sprintf("outbound|%d||%s", 80, dest)
		if err := validator.
			Exists("{.configs[*].dynamicActiveClusters[?(@.cluster.name == '%s')]}", clusterName).
			Check(); err != nil {
			return false, err
		}
		return true, nil
	}

	workloads, _ := c.Workloads()
	// Wait for the outbound config to be received by each workload from Pilot.
	for _, w := range workloads {
		if w.Sidecar() != nil {
			if err := w.Sidecar().WaitForConfig(accept, retry.Timeout(time.Second*10)); err != nil {
				return err
			}
		}
	}

	return nil
}

func sendTraffic(from echo.Instance, host string) error {
	headers := http.Header{}
	headers.Add("Host", host)
	// This is a hack to remain infrastructure agnostic when running these tests
	// We actually call the host set above not the endpoint we pass
	resp, err := from.Call(echo.CallOptions{
		Target:   from,
		PortName: "http",
		Headers:  headers,
		Count:    sendCount,
	})
	if err != nil {
		return fmt.Errorf("%s->%s failed sending: %v", from.Config().Service, host, err)
	}
	if len(resp) != sendCount {
		return fmt.Errorf("%s->%s expected %d responses, received %d", from.Config().Service, host, sendCount, len(resp))
	}
	for i, r := range resp {
		if match := bHostnameMatcher.FindString(r.Hostname); len(match) == 0 {
			return fmt.Errorf("%s->%s request[%d] made to unexpected service: %s", from.Config().Service, host, i, r.Hostname)
		}
	}
	return nil
}
