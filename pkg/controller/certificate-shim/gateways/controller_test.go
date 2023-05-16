/*
Copyright 2020 The cert-manager Authors.

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
	"context"
	"fmt"
	"testing"
	"time"

	testpkg "github.com/cert-manager/cert-manager/pkg/controller/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/util/workqueue"
	gwapi "sigs.k8s.io/gateway-api/apis/v1beta1"
	gwclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"

	cmacme "github.com/cert-manager/cert-manager/pkg/apis/acme/v1"
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmclient "github.com/cert-manager/cert-manager/pkg/client/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var gatewayGVK = gwapi.SchemeGroupVersion.WithKind("Gateway")

func Test_controller_Register(t *testing.T) {
	tests := []struct {
		name           string
		existingCert   *cmapi.Certificate
		givenCall      func(*testing.T, cmclient.Interface, gwclient.Interface)
		expectAddCalls []interface{}
	}{
		{
			name: "gateway is re-queued when an 'Added' event is received for this gateway",
			givenCall: func(t *testing.T, _ cmclient.Interface, c gwclient.Interface) {
				_, err := c.GatewayV1beta1().Gateways("namespace-1").Create(context.Background(), &gwapi.Gateway{ObjectMeta: metav1.ObjectMeta{
					Namespace: "namespace-1", Name: "gateway-1",
				}}, metav1.CreateOptions{})
				require.NoError(t, err)
			},
			expectAddCalls: []interface{}{"namespace-1/gateway-1"},
		},
		{
			name: "gateway is re-queued when an 'Updated' event is received for this gateway",
			givenCall: func(t *testing.T, _ cmclient.Interface, c gwclient.Interface) {
				// We can't use the gateway-api fake.NewSimpleClientset due to
				// Gateway being pluralized as "gatewaies" instead of
				// "gateways". The trick is thus to use Create instead.
				_, err := c.GatewayV1beta1().Gateways("namespace-1").Create(context.Background(), &gwapi.Gateway{ObjectMeta: metav1.ObjectMeta{
					Namespace: "namespace-1", Name: "gateway-1",
				}}, metav1.CreateOptions{})
				require.NoError(t, err)

				_, err = c.GatewayV1beta1().Gateways("namespace-1").Update(context.Background(), &gwapi.Gateway{ObjectMeta: metav1.ObjectMeta{
					Namespace: "namespace-1", Name: "gateway-1", Labels: map[string]string{"foo": "bar"},
				}}, metav1.UpdateOptions{})
				require.NoError(t, err)
			},
			expectAddCalls: []interface{}{"namespace-1/gateway-1", "namespace-1/gateway-1"},
			//                                <----- Create ------>    <------ Update ----->
		},
		{
			name: "gateway is re-queued when a 'Deleted' event is received for this gateway",
			givenCall: func(t *testing.T, _ cmclient.Interface, c gwclient.Interface) {
				_, err := c.GatewayV1beta1().Gateways("namespace-1").Create(context.Background(), &gwapi.Gateway{ObjectMeta: metav1.ObjectMeta{
					Namespace: "namespace-1", Name: "gateway-1",
				}}, metav1.CreateOptions{})
				require.NoError(t, err)

				err = c.GatewayV1beta1().Gateways("namespace-1").Delete(context.Background(), "gateway-1", metav1.DeleteOptions{})
				require.NoError(t, err)
			},
			expectAddCalls: []interface{}{"namespace-1/gateway-1", "namespace-1/gateway-1"},
			//                                <----- Create ------>    <------ Delete ----->
		},
		{
			name: "gateway is re-queued when an 'Added' event is received for a child HTTPRoute",
			givenCall: func(t *testing.T, _ cmclient.Interface, c gwclient.Interface) {
				_, err := c.GatewayV1beta1().HTTPRoutes("namespace-1").Create(context.Background(), &gwapi.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace-1", Name: "httproute-1",
					},
					Spec: gwapi.HTTPRouteSpec{
						CommonRouteSpec: gwapi.CommonRouteSpec{
							ParentRefs: []gwapi.ParentReference{
								{Name: "gateway-1"},
								{Namespace: ptrTo(gwapi.Namespace("namespace-2")), Name: "gateway-2"},
								{Kind: ptrTo(gwapi.Kind("wrongkind")), Name: "wrong-kind"},
								{Group: ptrTo(gwapi.Group("wronggroup")), Name: "wrong-group"},
							},
						},
					},
				}, metav1.CreateOptions{})
				require.NoError(t, err)
			},
			expectAddCalls: []interface{}{"namespace-1/gateway-1", "namespace-2/gateway-2"},
		},
		{
			name: "gateway is re-queued when an 'Updated' event is received for a child HTTPRoute that adds the parent",
			givenCall: func(t *testing.T, _ cmclient.Interface, c gwclient.Interface) {
				_, err := c.GatewayV1beta1().HTTPRoutes("namespace-1").Create(context.Background(), &gwapi.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace-1", Name: "httproute-1",
					},
				}, metav1.CreateOptions{})
				require.NoError(t, err)

				_, err = c.GatewayV1beta1().HTTPRoutes("namespace-1").Update(context.Background(), &gwapi.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace-1", Name: "httproute-1",
					},
					Spec: gwapi.HTTPRouteSpec{
						CommonRouteSpec: gwapi.CommonRouteSpec{
							ParentRefs: []gwapi.ParentReference{
								{Name: "gateway-1"},
							},
						},
					},
				}, metav1.UpdateOptions{})
				require.NoError(t, err)
			},
			expectAddCalls: []interface{}{"namespace-1/gateway-1"},
		},
		{
			name: "gateway is re-queued when an 'Updated' event is received for a child HTTPRoute that removes the parent",
			givenCall: func(t *testing.T, _ cmclient.Interface, c gwclient.Interface) {
				_, err := c.GatewayV1beta1().HTTPRoutes("namespace-1").Create(context.Background(), &gwapi.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace-1", Name: "httproute-1",
					},
					Spec: gwapi.HTTPRouteSpec{
						CommonRouteSpec: gwapi.CommonRouteSpec{
							ParentRefs: []gwapi.ParentReference{
								{Name: "gateway-1"},
							},
						},
					},
				}, metav1.CreateOptions{})
				require.NoError(t, err)

				_, err = c.GatewayV1beta1().HTTPRoutes("namespace-1").Update(context.Background(), &gwapi.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace-1", Name: "httproute-1",
					},
				}, metav1.UpdateOptions{})
				require.NoError(t, err)
			},
			expectAddCalls: []interface{}{
				"namespace-1/gateway-1", // Create
				"namespace-1/gateway-1", // Update
			},
		},
		{
			name: "gateway is re-queued when an 'Deleted' event is received for a child HTTPRoute",
			givenCall: func(t *testing.T, _ cmclient.Interface, c gwclient.Interface) {
				_, err := c.GatewayV1beta1().HTTPRoutes("namespace-1").Create(context.Background(), &gwapi.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace-1", Name: "httproute-1",
					},
					Spec: gwapi.HTTPRouteSpec{
						CommonRouteSpec: gwapi.CommonRouteSpec{
							ParentRefs: []gwapi.ParentReference{
								{Name: "gateway-1"},
							},
						},
					},
				}, metav1.CreateOptions{})
				require.NoError(t, err)

				err = c.GatewayV1beta1().HTTPRoutes("namespace-1").Delete(context.Background(), "httproute-1", metav1.DeleteOptions{})
				require.NoError(t, err)
			},
			expectAddCalls: []interface{}{
				"namespace-1/gateway-1", // Create
				"namespace-1/gateway-1", // Delete
			},
		},
		{
			name: "gateway is re-queued when an 'Added' event is received for its child Certificate",
			givenCall: func(t *testing.T, c cmclient.Interface, _ gwclient.Interface) {
				_, err := c.CertmanagerV1().Certificates("namespace-1").Create(context.Background(), &cmapi.Certificate{ObjectMeta: metav1.ObjectMeta{
					Namespace: "namespace-1", Name: "cert-1",
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(&gwapi.Gateway{ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace-1", Name: "gateway-2",
					}}, gatewayGVK)},
				}}, metav1.CreateOptions{})
				require.NoError(t, err)
			},
			expectAddCalls: []interface{}{"namespace-1/gateway-2"},
		},
		{
			name: "gateway is re-queued when an 'Updated' event is received for its child Certificate",
			existingCert: &cmapi.Certificate{ObjectMeta: metav1.ObjectMeta{
				Namespace: "namespace-1", Name: "cert-1",
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(&gwapi.Gateway{ObjectMeta: metav1.ObjectMeta{
					Namespace: "namespace-1", Name: "gateway-2",
				}}, gatewayGVK)},
			}},
			givenCall: func(t *testing.T, c cmclient.Interface, _ gwclient.Interface) {
				_, err := c.CertmanagerV1().Certificates("namespace-1").Update(context.Background(), &cmapi.Certificate{ObjectMeta: metav1.ObjectMeta{
					Namespace: "namespace-1", Name: "cert-1",
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(&gwapi.Gateway{ObjectMeta: metav1.ObjectMeta{
						Namespace: "namespace-1", Name: "gateway-2",
					}}, gatewayGVK)},
				}}, metav1.UpdateOptions{})
				require.NoError(t, err)
			},
			expectAddCalls: []interface{}{"namespace-1/gateway-2"},
		},
		{
			name: "gateway is re-queued when a 'Deleted' event is received for its child Certificate",
			existingCert: &cmapi.Certificate{ObjectMeta: metav1.ObjectMeta{
				Namespace: "namespace-1", Name: "cert-1",
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(&gwapi.Gateway{ObjectMeta: metav1.ObjectMeta{
					Namespace: "namespace-1", Name: "gateway-2",
				}}, gatewayGVK)},
			}},
			givenCall: func(t *testing.T, c cmclient.Interface, _ gwclient.Interface) {
				// err := c.CertmanagerV1().Certificates("namespace-1").Delete(context.Background(), "cert-1", metav1.DeleteOptions{})
				// require.NoError(t, err)
			},
			expectAddCalls: []interface{}{"namespace-1/gateway-2"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {

			var o []runtime.Object
			if test.existingCert != nil {
				o = append(o, test.existingCert)
			}

			// NOTE(mael): we can't use Gateway with GWObjects because of a
			// limitation in client-go's NewSimpleClientset. It uses a heuristic
			// that wrongly guesses the resource from the Gateway kind
			// ("gatewaies" instead of "gateways"). To work around this, the
			// only way is to either use a real apiserver or to use call Create
			// instead of setting existing objects with NewSimpleClientset. See:
			// https://github.com/kubernetes/client-go/blob/7a90b0858/testing/fixture.go#L326-L331
			b := &testpkg.Builder{T: t, CertManagerObjects: o}

			b.Init()

			// We don't care about the HasSynced functions since we already know
			// whether they have been properly "used": if no Gateway or
			// Certificate event is received then HasSynced has not been setup
			// properly.
			mock := &mockWorkqueue{t: t}
			_, _, err := (&controller{queue: mock}).Register(b.Context)
			require.NoError(t, err)

			b.Start()
			defer b.Stop()

			test.givenCall(t, b.CMClient, b.GWClient)

			// We have no way of knowing when the informers will be done adding
			// items to the queue due to the "shared informer" architecture:
			// Start(stop) does not allow you to wait for the informers to be
			// done.
			time.Sleep(50 * time.Millisecond)

			// We only expect 0 or 1 keys received in the queue, or 2 keys when
			// we have to create a Gateway before deleting or updating it.
			assert.Equal(t, test.expectAddCalls, mock.callsToAdd)
		})
	}
}

func Test_syntheticGatewayListeners(t *testing.T) {
	makeListeners := func(n int) (out []gwapi.Listener) {
		for i := 0; i < n; i++ {
			out = append(out, gwapi.Listener{
				Name: gwapi.SectionName(fmt.Sprint("listener-", i+1)),
				Port: 42,
			})
		}
		return
	}
	makeListenersForHosts := func(i int, hostnames ...gwapi.Hostname) (out []gwapi.Listener) {
		for _, hostname := range hostnames {
			hostname := hostname
			out = append(out, gwapi.Listener{
				Name:     gwapi.SectionName(fmt.Sprint("listener-", i)),
				Port:     42,
				Hostname: ptrTo(hostname),
			})
		}
		return
	}
	makeGateway := func(name string, listeners []gwapi.Listener) gwapi.Gateway {
		return gwapi.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: gwapi.GatewaySpec{
				Listeners: listeners,
			},
		}
	}
	makeHTTPRoute := func(hostnames []gwapi.Hostname, parentNames []gwapi.ObjectName, parentAvails []bool) *gwapi.HTTPRoute {
		r := &gwapi.HTTPRoute{
			Spec: gwapi.HTTPRouteSpec{
				Hostnames: hostnames,
			},
		}
		for i, parentName := range parentNames {
			ref := gwapi.ParentReference{
				Name: parentName,
			}
			r.Spec.CommonRouteSpec.ParentRefs = append(r.Spec.CommonRouteSpec.ParentRefs, ref)

			status := metav1.ConditionTrue
			if !parentAvails[i] {
				status = metav1.ConditionFalse
			}
			r.Status.RouteStatus.Parents = append(r.Status.RouteStatus.Parents, gwapi.RouteParentStatus{
				ParentRef:  ref,
				Conditions: []metav1.Condition{{Type: "Available", Status: status}},
			})
		}

		return r
	}

	tests := []struct {
		name            string
		gateway         gwapi.Gateway
		routes          routes
		expectListeners []gwapi.Listener
	}{
		{
			name:   "empty yields empty",
			routes: httpRoutes{},
		},
		{
			name:    "HTTPRoute has a hostname",
			gateway: makeGateway("gateway-1", makeListeners(1)),
			routes: httpRoutes{
				makeHTTPRoute([]gwapi.Hostname{"hostname-1"}, []gwapi.ObjectName{"gateway-1"}, []bool{true}),
			},
			expectListeners: makeListenersForHosts(1, "hostname-1"),
		},
		{
			name:    "HTTPRoute has two hostnames",
			gateway: makeGateway("gateway-1", makeListeners(1)),
			routes: httpRoutes{
				makeHTTPRoute([]gwapi.Hostname{"hostname-1", "hostname-2"}, []gwapi.ObjectName{"gateway-1"}, []bool{true}),
			},
			expectListeners: makeListenersForHosts(1, "hostname-1", "hostname-2"),
		},
		{
			name:    "HTTPRoute serves two listeners",
			gateway: makeGateway("gateway-1", makeListeners(2)),
			routes: httpRoutes{
				makeHTTPRoute([]gwapi.Hostname{"hostname-1"}, []gwapi.ObjectName{"gateway-1"}, []bool{true}),
			},
			expectListeners: []gwapi.Listener{
				{Name: "listener-1", Port: 42, Hostname: ptrTo(gwapi.Hostname("hostname-1"))},
				{Name: "listener-2", Port: 42, Hostname: ptrTo(gwapi.Hostname("hostname-1"))},
			},
		},
		{
			name:    "HTTPRoute is unavailable",
			gateway: makeGateway("gateway-1", makeListeners(1)),
			routes: httpRoutes{
				makeHTTPRoute([]gwapi.Hostname{"hostname-1"}, []gwapi.ObjectName{"gateway-1"}, []bool{false}),
			},
			expectListeners: nil,
		},
		{
			name:    "HTTPRoute has a mismatching parent name",
			gateway: makeGateway("gateway-1", makeListeners(1)),
			routes: httpRoutes{
				makeHTTPRoute([]gwapi.Hostname{"hostname-1"}, []gwapi.ObjectName{"gateway-x"}, []bool{true}),
			},
			expectListeners: nil,
		},
		{
			name:    "HTTPRoute has a mismatching hostname",
			gateway: makeGateway("gateway-1", makeListenersForHosts(1, "hostname-1")),
			routes: httpRoutes{
				makeHTTPRoute([]gwapi.Hostname{"hostname-x"}, []gwapi.ObjectName{"gateway-1"}, []bool{true}),
			},
			expectListeners: nil,
		},
		{
			name:    "HTTPRoute has a mismatching section name",
			gateway: makeGateway("gateway-1", makeListeners(1)),
			routes: httpRoutes{
				{
					Spec: gwapi.HTTPRouteSpec{
						Hostnames: []gwapi.Hostname{"hostname-1"},
						CommonRouteSpec: gwapi.CommonRouteSpec{
							ParentRefs: []gwapi.ParentReference{{Name: "gateway-1", SectionName: ptrTo(gwapi.SectionName("listener-x"))}},
						},
					},
					Status: gwapi.HTTPRouteStatus{
						RouteStatus: gwapi.RouteStatus{
							Parents: []gwapi.RouteParentStatus{
								{
									ParentRef:  gwapi.ParentReference{Name: "gateway-1"},
									Conditions: []metav1.Condition{{Type: "Available", Status: metav1.ConditionTrue}},
								},
							},
						},
					},
				},
			},
			expectListeners: nil,
		},
		{
			name:    "HTTPRoute has a mismatching port",
			gateway: makeGateway("gateway-1", makeListeners(1)),
			routes: httpRoutes{
				{
					Spec: gwapi.HTTPRouteSpec{
						Hostnames: []gwapi.Hostname{"hostname-1"},
						CommonRouteSpec: gwapi.CommonRouteSpec{
							ParentRefs: []gwapi.ParentReference{{Name: "gateway-1", Port: ptrTo(gwapi.PortNumber(0))}},
						},
					},
					Status: gwapi.HTTPRouteStatus{
						RouteStatus: gwapi.RouteStatus{
							Parents: []gwapi.RouteParentStatus{
								{
									ParentRef:  gwapi.ParentReference{Name: "gateway-1"},
									Conditions: []metav1.Condition{{Type: "Available", Status: metav1.ConditionTrue}},
								},
							},
						},
					},
				},
			},
			expectListeners: nil,
		},
		{
			name:    "HTTPRoute is controlled by cert-manager",
			gateway: makeGateway("gateway-1", makeListeners(1)),
			routes: httpRoutes{
				{
					ObjectMeta: metav1.ObjectMeta{
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: cmacme.SchemeGroupVersion.String(),
								Kind:       "Challenge",
								Controller: ptrTo(true),
							},
						},
					},
					Spec: gwapi.HTTPRouteSpec{
						Hostnames: []gwapi.Hostname{"hostname-1"},
						CommonRouteSpec: gwapi.CommonRouteSpec{
							ParentRefs: []gwapi.ParentReference{{Name: "gateway-1", SectionName: ptrTo(gwapi.SectionName("listener-1"))}},
						},
					},
					Status: gwapi.HTTPRouteStatus{
						RouteStatus: gwapi.RouteStatus{
							Parents: []gwapi.RouteParentStatus{
								{
									ParentRef:  gwapi.ParentReference{Name: "gateway-1"},
									Conditions: []metav1.Condition{{Type: "Available", Status: metav1.ConditionTrue}},
								},
							},
						},
					},
				},
			},
			expectListeners: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := syntheticGatewayListeners(&test.gateway, test.routes)

			assert.Equal(t, test.expectListeners, got)
		})
	}
}

type mockWorkqueue struct {
	t          *testing.T
	callsToAdd []interface{}
}

var _ workqueue.Interface = &mockWorkqueue{}

func (m *mockWorkqueue) Add(arg0 interface{}) {
	m.callsToAdd = append(m.callsToAdd, arg0)
}

func (m *mockWorkqueue) AddAfter(arg0 interface{}, arg1 time.Duration) {
	m.t.Error("workqueue.AddAfter was called but was not expected to be called")
}

func (m *mockWorkqueue) AddRateLimited(arg0 interface{}) {
	m.t.Error("workqueue.AddRateLimited was called but was not expected to be called")
}

func (m *mockWorkqueue) Done(arg0 interface{}) {
	m.t.Error("workqueue.Done was called but was not expected to be called")
}

func (m *mockWorkqueue) Forget(arg0 interface{}) {
	m.t.Error("workqueue.Forget was called but was not expected to be called")
}

func (m *mockWorkqueue) Get() (interface{}, bool) {
	m.t.Error("workqueue.Get was called but was not expected to be called")
	return nil, false
}

func (m *mockWorkqueue) Len() int {
	m.t.Error("workqueue.Len was called but was not expected to be called")
	return 0
}

func (m *mockWorkqueue) NumRequeues(arg0 interface{}) int {
	m.t.Error("workqueue.NumRequeues was called but was not expected to be called")
	return 0
}

func (m *mockWorkqueue) ShutDown() {
	m.t.Error("workqueue.ShutDown was called but was not expected to be called")
}

func (m *mockWorkqueue) ShutDownWithDrain() {
	m.t.Error("workqueue.ShutDownWithDrain was called but was not expected to be called")

}

func (m *mockWorkqueue) ShuttingDown() bool {
	m.t.Error("workqueue.ShuttingDown was called but was not expected to be called")
	return false
}

func ptrTo[T any](a T) *T {
	return &a
}
