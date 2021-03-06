/*
Copyright 2015 The Kubernetes Authors.

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

package controller

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	compute "google.golang.org/api/compute/v1"

	api_v1 "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes/fake"

	"k8s.io/ingress-gce/pkg/annotations"
	"k8s.io/ingress-gce/pkg/context"
	"k8s.io/ingress-gce/pkg/firewalls"
	"k8s.io/ingress-gce/pkg/flags"
	"k8s.io/ingress-gce/pkg/loadbalancers"
	"k8s.io/ingress-gce/pkg/tls"
	"k8s.io/ingress-gce/pkg/utils"
)

const testClusterName = "testcluster"

var (
	testIPManager = testIP{}
)

func defaultBackendName(clusterName string) string {
	return fmt.Sprintf("%v-%v", "k8s-be", clusterName)
}

// newLoadBalancerController create a loadbalancer controller.
func newLoadBalancerController(t *testing.T, cm *fakeClusterManager) *LoadBalancerController {
	kubeClient := fake.NewSimpleClientset()
	stopCh := make(chan struct{})
	ctx := context.NewControllerContext(kubeClient, api_v1.NamespaceAll, 1*time.Second, true)
	lb, err := NewLoadBalancerController(kubeClient, stopCh, ctx, cm.ClusterManager, true, testDefaultBeNodePort)
	if err != nil {
		t.Fatalf("%v", err)
	}
	lb.hasSynced = func() bool { return true }
	return lb
}

// toHTTPIngressPaths converts the given pathMap to a list of HTTPIngressPaths.
func toHTTPIngressPaths(pathMap map[string]string) []extensions.HTTPIngressPath {
	httpPaths := []extensions.HTTPIngressPath{}
	for path, backend := range pathMap {
		httpPaths = append(httpPaths, extensions.HTTPIngressPath{
			Path: path,
			Backend: extensions.IngressBackend{
				ServiceName: backend,
				ServicePort: testBackendPort,
			},
		})
	}
	return httpPaths
}

// toIngressRules converts the given path map to a list of IngressRules.
func toIngressRules(paths utils.PrimitivePathMap) []extensions.IngressRule {
	rules := []extensions.IngressRule{}
	for host, pathMap := range paths {
		rules = append(rules, extensions.IngressRule{
			Host: host,
			IngressRuleValue: extensions.IngressRuleValue{
				HTTP: &extensions.HTTPIngressRuleValue{
					Paths: toHTTPIngressPaths(pathMap),
				},
			},
		})
	}
	return rules
}

// newIngress returns a new Ingress with the given path map.
func newIngress(paths utils.PrimitivePathMap) *extensions.Ingress {
	ret := &extensions.Ingress{
		TypeMeta: meta_v1.TypeMeta{
			Kind:       "Ingress",
			APIVersion: "extensions/v1beta1",
		},
		ObjectMeta: meta_v1.ObjectMeta{
			Name:      fmt.Sprintf("%v", uuid.NewUUID()),
			Namespace: "default",
		},
		Spec: extensions.IngressSpec{
			Backend: &extensions.IngressBackend{
				ServiceName: defaultBackendName(testClusterName),
				ServicePort: testBackendPort,
			},
			Rules: toIngressRules(paths),
		},
		Status: extensions.IngressStatus{
			LoadBalancer: api_v1.LoadBalancerStatus{
				Ingress: []api_v1.LoadBalancerIngress{
					{IP: testIPManager.ip()},
				},
			},
		},
	}
	ret.SelfLink = fmt.Sprintf("%s/%s", ret.Namespace, ret.Name)
	return ret
}

// validIngress returns a valid Ingress.
func validIngress() *extensions.Ingress {
	return newIngress(utils.PrimitivePathMap{
		"foo.bar.com": {
			"/foo": defaultBackendName(testClusterName),
		},
	})
}

// getKey returns the key for an ingress.
func getKey(ing *extensions.Ingress, t *testing.T) string {
	key, err := keyFunc(ing)
	if err != nil {
		t.Fatalf("Unexpected error getting key for Ingress %v: %v", ing.Name, err)
	}
	return key
}

// gceURLMapFromPrimitive returns a GCEURLMap that is populated from a primitive representation.
// It uses the passed in nodePortManager to construct a stubbed ServicePort based on service names.
func gceURLMapFromPrimitive(primitiveMap utils.PrimitivePathMap, pm *nodePortManager) *utils.GCEURLMap {
	urlMap := utils.NewGCEURLMap()
	for hostname, rules := range primitiveMap {
		pathRules := make([]utils.PathRule, 0)
		for path, backend := range rules {
			nodePort := pm.getNodePort(backend)
			stubSvcPort := utils.ServicePort{NodePort: int64(nodePort)}
			pathRules = append(pathRules, utils.PathRule{Path: path, Backend: stubSvcPort})
		}
		urlMap.PutPathRulesForHost(hostname, pathRules)
	}
	urlMap.DefaultBackend = testDefaultBeNodePort
	return urlMap
}

// nodePortManager is a helper to allocate ports to services and
// remember the allocations.
type nodePortManager struct {
	portMap map[string]int
	start   int
	end     int
	namer   *utils.Namer
}

// randPort generated pseudo random port numbers.
func (p *nodePortManager) getNodePort(svcName string) int {
	if port, ok := p.portMap[svcName]; ok {
		return port
	}
	p.portMap[svcName] = rand.Intn(p.end-p.start) + p.start
	return p.portMap[svcName]
}

func newPortManager(st, end int, namer *utils.Namer) *nodePortManager {
	return &nodePortManager{map[string]int{}, st, end, namer}
}

// addIngress adds an ingress to the loadbalancer controllers ingress store. If
// a nodePortManager is supplied, it also adds all backends to the service store
// with a nodePort acquired through it.
func addIngress(lbc *LoadBalancerController, ing *extensions.Ingress, pm *nodePortManager) {
	for _, rule := range ing.Spec.Rules {
		for _, path := range rule.HTTP.Paths {
			svc := &api_v1.Service{
				ObjectMeta: meta_v1.ObjectMeta{
					Name:      path.Backend.ServiceName,
					Namespace: ing.Namespace,
				},
			}
			var svcPort api_v1.ServicePort
			switch path.Backend.ServicePort.Type {
			case intstr.Int:
				svcPort = api_v1.ServicePort{Port: path.Backend.ServicePort.IntVal}
			default:
				svcPort = api_v1.ServicePort{Name: path.Backend.ServicePort.StrVal}
			}
			svcPort.NodePort = int32(pm.getNodePort(path.Backend.ServiceName))
			svc.Spec.Ports = []api_v1.ServicePort{svcPort}
			lbc.ctx.ServiceInformer.GetIndexer().Add(svc)
		}
	}
	lbc.client.Extensions().Ingresses(ing.Namespace).Create(ing)
	lbc.ctx.IngressInformer.GetIndexer().Add(ing)
}

func TestLbCreateDelete(t *testing.T) {
	testFirewallName := "quux"
	cm := NewFakeClusterManager(flags.DefaultClusterUID, testFirewallName)
	lbc := newLoadBalancerController(t, cm)
	pm := newPortManager(1, 65536, cm.Namer)
	inputMap1 := utils.PrimitivePathMap{
		"foo.example.com": {
			"/foo1": "foo1svc",
			"/foo2": "foo2svc",
		},
		"bar.example.com": {
			"/bar1": "bar1svc",
			"/bar2": "bar2svc",
		},
	}
	inputMap2 := utils.PrimitivePathMap{
		"baz.foobar.com": {
			"/foo": "foo1svc",
			"/bar": "bar1svc",
		},
	}
	ings := []*extensions.Ingress{}
	for _, m := range []utils.PrimitivePathMap{inputMap1, inputMap2} {
		newIng := newIngress(m)
		addIngress(lbc, newIng, pm)
		ingStoreKey := getKey(newIng, t)
		if err := lbc.sync(ingStoreKey); err != nil {
			t.Fatalf("lbc.sync(%v) = err %v", ingStoreKey, err)
		}
		l7, err := cm.l7Pool.Get(ingStoreKey)
		if err != nil {
			t.Fatalf("cm.l7Pool.Get(%q) = _, %v; want nil", ingStoreKey, err)
		}
		expectedUrlMap := gceURLMapFromPrimitive(m, pm)
		if err := cm.fakeLbs.CheckURLMap(l7, expectedUrlMap); err != nil {
			t.Fatalf("cm.fakeLbs.CheckURLMap(l7, expectedUrlMap) = %v, want nil", err)
		}
		ings = append(ings, newIng)
	}
	lbc.ingLister.Store.Delete(ings[0])
	if err := lbc.sync(getKey(ings[0], t)); err != nil {
		t.Fatalf("lbc.sync() = err %v", err)
	}

	// BackendServices associated with ports of deleted Ingress' should get gc'd
	// when the Ingress is deleted, regardless of the service. At the same time
	// we shouldn't pull shared backends out from existing loadbalancers.
	unexpected := []int{pm.portMap["foo2svc"], pm.portMap["bar2svc"]}
	expected := []int{pm.portMap["foo1svc"], pm.portMap["bar1svc"]}
	pm.namer.SetFirewall(testFirewallName)
	firewallName := pm.namer.FirewallRule()

	// Check existence of firewall rule
	_, err := cm.firewallPool.(*firewalls.FirewallRules).GetFirewall(firewallName)
	if err != nil {
		t.Fatalf("%v", err)
	}

	for _, port := range unexpected {
		beName := pm.namer.IGBackend(int64(port))
		if be, err := cm.backendPool.Get(beName, false); err == nil {
			t.Fatalf("Found backend %+v for port %v", be, port)
		}
	}

	lbc.ingLister.Store.Delete(ings[1])
	if err := lbc.sync(getKey(ings[1], t)); err != nil {
		t.Fatalf("lbc.sync() = err %v", err)
	}

	// No cluster resources (except the defaults used by the cluster manager)
	// should exist at this point.
	for _, port := range expected {
		beName := pm.namer.IGBackend(int64(port))
		if be, err := cm.backendPool.Get(beName, false); err == nil {
			t.Fatalf("Found backend %+v for port %v", be, port)
		}
	}
	if len(cm.fakeLbs.Fw) != 0 || len(cm.fakeLbs.Um) != 0 || len(cm.fakeLbs.Tp) != 0 {
		t.Errorf("Loadbalancer leaked resources")
	}
	for _, lbName := range []string{getKey(ings[0], t), getKey(ings[1], t)} {
		if l7, err := cm.l7Pool.Get(lbName); err == nil {
			t.Fatalf("Got loadbalancer %+v: %v, want none", l7, err)
		}
	}
	if firewallRule, err := cm.firewallPool.(*firewalls.FirewallRules).GetFirewall(firewallName); err == nil {
		t.Errorf("Got firewall rule %+v, want none", firewallRule)
	}
}

func TestLbFaultyUpdate(t *testing.T) {
	cm := NewFakeClusterManager(flags.DefaultClusterUID, DefaultFirewallName)
	lbc := newLoadBalancerController(t, cm)
	pm := newPortManager(1, 65536, cm.Namer)
	inputMap := utils.PrimitivePathMap{
		"foo.example.com": {
			"/foo1": "foo1svc",
			"/foo2": "foo2svc",
		},
		"bar.example.com": {
			"/bar1": "bar1svc",
			"/bar2": "bar2svc",
		},
	}
	ing := newIngress(inputMap)
	addIngress(lbc, ing, pm)
	ingStoreKey := getKey(ing, t)
	if err := lbc.sync(ingStoreKey); err != nil {
		t.Fatalf("lbc.sync() = err %v", err)
	}
	l7, err := cm.l7Pool.Get(ingStoreKey)
	if err != nil {
		t.Fatalf("%v", err)
	}
	expectedUrlMap := gceURLMapFromPrimitive(inputMap, pm)
	if err := cm.fakeLbs.CheckURLMap(l7, expectedUrlMap); err != nil {
		t.Fatalf("cm.fakeLbs.CheckURLMap(...) = %v, want nil", err)
	}

	// Change the urlmap directly, resync, and
	// make sure the controller corrects it.
	l7.RuntimeInfo().UrlMap = gceURLMapFromPrimitive(utils.PrimitivePathMap{
		"foo.example.com": {
			"/foo1": "foo2svc",
		},
	}, pm)
	l7.UpdateUrlMap()

	if err := lbc.sync(ingStoreKey); err != nil {
		t.Fatalf("lbc.sync() = err %v", err)
	}
	if err := cm.fakeLbs.CheckURLMap(l7, expectedUrlMap); err != nil {
		t.Fatalf("cm.fakeLbs.CheckURLMap(...) = %v, want nil", err)
	}
}

func TestLbDefaulting(t *testing.T) {
	cm := NewFakeClusterManager(flags.DefaultClusterUID, DefaultFirewallName)
	lbc := newLoadBalancerController(t, cm)
	pm := newPortManager(1, 65536, cm.Namer)
	// Make sure the controller plugs in the default values accepted by GCE.
	ing := newIngress(utils.PrimitivePathMap{"": {"": "foo1svc"}})

	addIngress(lbc, ing, pm)

	ingStoreKey := getKey(ing, t)
	if err := lbc.sync(ingStoreKey); err != nil {
		t.Fatalf("lbc.sync() = err %v", err)
	}
	l7, err := cm.l7Pool.Get(ingStoreKey)
	if err != nil {
		t.Fatalf("%v", err)
	}
	expected := utils.PrimitivePathMap{
		loadbalancers.DefaultHost: {
			loadbalancers.DefaultPath: "foo1svc",
		},
	}
	expectedUrlMap := gceURLMapFromPrimitive(expected, pm)
	if err := cm.fakeLbs.CheckURLMap(l7, expectedUrlMap); err != nil {
		t.Fatalf("cm.fakeLbs.CheckURLMap(...) = %v, want nil", err)
	}
}

func TestLbNoService(t *testing.T) {
	cm := NewFakeClusterManager(flags.DefaultClusterUID, DefaultFirewallName)
	pm := newPortManager(1, 65536, cm.Namer)
	lbc := newLoadBalancerController(t, cm)
	inputMap := utils.PrimitivePathMap{
		"foo.example.com": {
			"/foo1": "foo1svc",
		},
	}
	ing := newIngress(inputMap)
	ing.Namespace = "ns1"
	ingStoreKey := getKey(ing, t)

	// Adds ingress to store, but doesn't create an associated service.
	// This will still create the associated loadbalancer, it will just
	// have empty rules. The rules will get corrected when the service
	// pops up.
	addIngress(lbc, ing, pm)
	if err := lbc.sync(ingStoreKey); err != nil {
		t.Fatalf("lbc.sync() = err %v", err)
	}

	l7, err := cm.l7Pool.Get(ingStoreKey)
	if err != nil {
		t.Fatalf("%v", err)
	}

	// Creates the service, next sync should have complete url map.
	addIngress(lbc, ing, pm)
	svc := &api_v1.Service{
		ObjectMeta: meta_v1.ObjectMeta{
			Name:      "foo1svc",
			Namespace: ing.Namespace,
		},
	}

	lbc.enqueueIngressForService(svc)
	if err := lbc.sync(fmt.Sprintf("%s/%s", ing.Namespace, ing.Name)); err != nil {
		t.Fatalf("lbc.sync() = err %v", err)
	}

	expectedUrlMap := gceURLMapFromPrimitive(inputMap, pm)
	if err := cm.fakeLbs.CheckURLMap(l7, expectedUrlMap); err != nil {
		t.Fatalf("cm.fakeLbs.CheckURLMap(...) = %v, want nil", err)
	}
}

func TestLbChangeStaticIP(t *testing.T) {
	cm := NewFakeClusterManager(flags.DefaultClusterUID, DefaultFirewallName)
	lbc := newLoadBalancerController(t, cm)
	inputMap := utils.PrimitivePathMap{
		"foo.example.com": {
			"/foo1": "foo1svc",
		},
	}
	ing := newIngress(inputMap)
	ing.Spec.Backend.ServiceName = "foo1svc"
	cert := extensions.IngressTLS{SecretName: "foo"}
	ing.Spec.TLS = []extensions.IngressTLS{cert}

	// Add some certs so we get 2 forwarding rules, the changed static IP
	// should be assigned to both the HTTP and HTTPS forwarding rules.
	lbc.tlsLoader = &tls.FakeTLSSecretLoader{
		FakeCerts: map[string]*loadbalancers.TLSCerts{
			cert.SecretName: {Key: "foo", Cert: "bar"},
		},
	}

	pm := newPortManager(1, 65536, cm.Namer)
	addIngress(lbc, ing, pm)
	ingStoreKey := getKey(ing, t)

	// First sync creates forwarding rules and allocates an IP.
	if err := lbc.sync(ingStoreKey); err != nil {
		t.Fatalf("lbc.sync() = err %v", err)
	}

	// First allocate a static ip, then specify a userip in annotations.
	// The forwarding rules should contain the user ip.
	// The static ip should get cleaned up on lb tear down.
	oldIP := ing.Status.LoadBalancer.Ingress[0].IP
	oldRules := cm.fakeLbs.GetForwardingRulesWithIPs([]string{oldIP})
	if len(oldRules) != 2 || oldRules[0].IPAddress != oldRules[1].IPAddress {
		t.Fatalf("Expected 2 forwarding rules with the same IP.")
	}

	ing.Annotations = map[string]string{annotations.StaticIPNameKey: "testip"}
	cm.fakeLbs.ReserveGlobalAddress(&compute.Address{Name: "testip", Address: "1.2.3.4"})

	// Second sync reassigns 1.2.3.4 to existing forwarding rule (by recreating it)
	if err := lbc.sync(ingStoreKey); err != nil {
		t.Fatalf("lbc.sync() = err %v", err)
	}

	newRules := cm.fakeLbs.GetForwardingRulesWithIPs([]string{"1.2.3.4"})
	if len(newRules) != 2 || newRules[0].IPAddress != newRules[1].IPAddress || newRules[1].IPAddress != "1.2.3.4" {
		t.Fatalf("Found unexpected forwaring rules after changing static IP annotation.")
	}
}

type testIP struct {
	start int
}

func (t *testIP) ip() string {
	t.start++
	return fmt.Sprintf("0.0.0.%v", t.start)
}

// TODO: Test lb status update when annotation stabilize
