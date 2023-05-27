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
	"reflect"
	"strings"
	"time"

	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	gwapi "sigs.k8s.io/gateway-api/apis/v1beta1"
	gwlisters "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1beta1"

	cmacme "github.com/cert-manager/cert-manager/pkg/apis/acme/v1"
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	controllerpkg "github.com/cert-manager/cert-manager/pkg/controller"
	shimhelper "github.com/cert-manager/cert-manager/pkg/controller/certificate-shim"
	logf "github.com/cert-manager/cert-manager/pkg/logs"
)

const (
	ControllerName = "gateway-shim"

	// resyncPeriod is set to 10 hours across cert-manager. These 10 hours come
	// from a discussion on the controller-runtime project that boils down to:
	// never change this without an explicit reason.
	// https://github.com/kubernetes-sigs/controller-runtime/pull/88#issuecomment-408500629
	resyncPeriod = 10 * time.Hour
)

type controller struct {
	gatewayLister   gwlisters.GatewayLister
	httpRouteLister gwlisters.HTTPRouteLister
	sync            shimhelper.SyncFn

	// For testing purposes.
	queue workqueue.RateLimitingInterface
}

func (c *controller) Register(ctx *controllerpkg.Context) (workqueue.RateLimitingInterface, []cache.InformerSynced, error) {
	gatewayAPI := ctx.GWShared.Gateway().V1beta1()
	c.gatewayLister = gatewayAPI.Gateways().Lister()
	c.httpRouteLister = gatewayAPI.HTTPRoutes().Lister()

	// We don't need to requeue Gateways on "Deleted" events, since our Sync
	// function does nothing when the Gateway lister returns "not found". But we
	// still do it for consistency with the rest of the controllers.
	gatewayAPI.Gateways().Informer().AddEventHandler(&controllerpkg.QueuingEventHandler{
		Queue: c.queue,
	})

	// Routes contain references to the parent Gateways they serve
	// under. Some routes, like HTTPRoute, contain hostnames that
	// should receive certificates if the Gateway listener doesn't
	// list any hostnames itself.
	for _, route := range []cache.SharedIndexInformer{gatewayAPI.HTTPRoutes().Informer()} {
		route.AddEventHandler(&blockingEventHandler{
			BlockingEventHandler: controllerpkg.BlockingEventHandler{
				WorkFunc: routeHandler(c.queue),
			},
		})
	}

	cmAPI := ctx.SharedInformerFactory.Certmanager().V1()

	// Even though the Gateway controller already re-queues the Gateway after
	// creating a child Certificate, we still re-queue the Gateway when we
	// receive an "Add" event for the Certificate (the workqueue de-duplicates
	// keys, so we should not worry).
	//
	// Regarding "Update" events on Certificates, we need to requeue the parent
	// Gateway because we need to check if the Certificate is still up to date.
	//
	// Regarding "Deleted" events on Certificates, we requeue the parent Gateway
	// to immediately recreate the Certificate when the Certificate is deleted.
	cmAPI.Certificates().Informer().AddEventHandler(&controllerpkg.BlockingEventHandler{
		WorkFunc: certificateHandler(c.queue),
	})

	log := logf.FromContext(ctx.RootContext, ControllerName)
	c.sync = shimhelper.SyncFnFor(ctx.Recorder, log, ctx.CMClient, cmAPI.Certificates().Lister(), ctx.IngressShimOptions, ctx.FieldManager)

	mustSync := []cache.InformerSynced{
		gatewayAPI.Gateways().Informer().HasSynced,
		gatewayAPI.HTTPRoutes().Informer().HasSynced,
		cmAPI.Certificates().Informer().HasSynced,
	}

	return c.queue, mustSync, nil
}

// blockingEventHandler is like BlockingEventHandler, except it runs
// the work function for both the old and new object on update. This
// is needed to find Gateways that have been removed from the parent
// list in HTTPRoutes.
type blockingEventHandler struct {
	controllerpkg.BlockingEventHandler
}

func (b *blockingEventHandler) OnUpdate(old, new interface{}) {
	if reflect.DeepEqual(old, new) {
		return
	}
	b.WorkFunc(old)
	b.WorkFunc(new)
}

func (c *controller) ProcessItem(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	roGateway, err := c.gatewayLister.Gateways(namespace).Get(name)
	if err != nil {
		if k8sErrors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("Gateway '%s' in work queue no longer exists", key))
			return nil
		}

		return err
	}

	gateway := roGateway
	if gatewayHasWildcardListener(gateway) {
		// At least one Listener is not specifying a hostname, which
		// means attached routes may dictate hostnames that matter.

		hroutes, err := c.httpRouteLister.List(labels.Everything())
		if err != nil {
			return err
		}

		newListeners := syntheticGatewayListeners(gateway, httpRoutes(hroutes))
		if len(newListeners) > 0 {
			// Make a copy, since Get returns a read-only object.
			if gateway == roGateway {
				gateway = gateway.DeepCopy()
			}

			gateway.Spec.Listeners = append(gateway.Spec.Listeners, newListeners...)
		}
	}

	return c.sync(ctx, gateway)
}

func syntheticGatewayListeners(gateway *gwapi.Gateway, routes routes) []gwapi.Listener {
	// The assignment to listeners matters, because TLS is
	// configured per-listener. We inject synhtetic listeners
	// for every hostname we find in matching routes. The
	// important bit is that the TLS certificate points to the
	// same resource as the real listener.
	var newListeners []gwapi.Listener
	for i := 0; i < routes.Len(); i++ {
		ometa := routes.ObjectMeta(i)
		hostnames := routes.Hostnames(i)
		logf.V(logf.DebugLevel).Infof("route hostnames %v: %v", hostnames, ometa)
		if !gatewayIsAParent(ometa, routes.RouteStatus(i), gateway) {
			continue
		}
		if hasCertManagerOwner(ometa) {
			// Ignore routes added just to solve challenges.
			logf.V(logf.DebugLevel).Infof("Route is owned by cert-manager %s/%s: %s/%s", gateway.GetNamespace(), gateway.GetName(), ometa.GetNamespace(), ometa.GetName())
			continue
		}

		for i, l := range gateway.Spec.Listeners {
			if l.Hostname != nil {
				// This listener has a dedicated hostname, and no
				// route can change that.
				logf.V(logf.DebugLevel).Infof("Listeners[%d] has a hostname, so ignoring routes: %s/%s", i, gateway.GetNamespace(), gateway.GetName())
				continue
			}

			if !listenerIsParent(ometa, hostnames, routes.CommonRouteSpec(i), gateway, &l) {
				continue
			}

			logf.V(logf.DebugLevel).Infof("Gateway %s/%s route %s/%s hostnames: %v", i, gateway.GetNamespace(), gateway.GetName(), ometa.GetNamespace(), ometa.GetName(), hostnames)
			for _, hostname := range hostnames {
				hostname := hostname
				// We are operating on, and making, shallow copies of
				// l here.
				l.Hostname = &hostname
				newListeners = append(newListeners, l)
			}
		}
	}
	return newListeners
}

// hasCertManagerOwner returns true if ometa's controller comes from
// cert-manager.
func hasCertManagerOwner(ometa *metav1.ObjectMeta) bool {
	ref := metav1.GetControllerOf(ometa)

	return ref != nil && strings.HasPrefix(ref.APIVersion, cmacme.SchemeGroupVersion.Group)
}

// routes is an abstraction over []*HTTPRoute and []*GRPCRoute.
type routes interface {
	Len() int
	ObjectMeta(i int) *metav1.ObjectMeta
	Hostnames(i int) []gwapi.Hostname
	CommonRouteSpec(i int) *gwapi.CommonRouteSpec
	RouteStatus(i int) *gwapi.RouteStatus
}

// httpRoutes implements routes for []*HTTPRoute.
type httpRoutes []*gwapi.HTTPRoute

func (rs httpRoutes) Len() int                            { return len(rs) }
func (rs httpRoutes) ObjectMeta(i int) *metav1.ObjectMeta { return &rs[i].ObjectMeta }
func (rs httpRoutes) Hostnames(i int) []gwapi.Hostname    { return rs[i].Spec.Hostnames }
func (rs httpRoutes) CommonRouteSpec(i int) *gwapi.CommonRouteSpec {
	return &rs[i].Spec.CommonRouteSpec
}
func (rs httpRoutes) RouteStatus(i int) *gwapi.RouteStatus { return &rs[i].Status.RouteStatus }

// listenerIsParent returns true if any parent reference matches the
// listener of the gateway.
//
// A route may either match
//
//  1. all listeners,
//  2. listeners by hostname,
//  3. listeners by name, and/or
//  4. listeners by port number.
//
// Cases (1) and (2) is separated on whether both
// listeners and routes have hostnames specified, or not.
func listenerIsParent(ometa *metav1.ObjectMeta, hostnames []gwapi.Hostname, cspec *gwapi.CommonRouteSpec, gateway *gwapi.Gateway, listener *gwapi.Listener) bool {
	for _, ref := range cspec.ParentRefs {
		if !gatewayIsParent(ometa, &ref, gateway) {
			continue
		}

		if listener.Hostname != nil {
			var match bool
			for _, hostname := range hostnames {
				if *listener.Hostname == hostname {
					match = true
					break
				}
			}

			if !match {
				continue
			}
		}

		if ref.SectionName != nil && *ref.SectionName != listener.Name {
			continue
		}

		if ref.Port != nil && *ref.Port != listener.Port {
			continue
		}

		return true
	}

	return false
}

// gatewayHasWildcardListener returns true if at least one Listener
// lacks a hostname.
func gatewayHasWildcardListener(gateway *gwapi.Gateway) bool {
	for _, l := range gateway.Spec.Listeners {
		if l.Hostname == nil {
			return true
		}
	}
	return false
}

// gatewayIsAParent returns true if `gateway` is a parent of
// the route, and its controller has marked the route as
// Available. This implies the route has passed validation.
func gatewayIsAParent(ometa *metav1.ObjectMeta, status *gwapi.RouteStatus, gateway *gwapi.Gateway) bool {
	for _, pstatus := range status.Parents {
		if gatewayIsParent(ometa, &pstatus.ParentRef, gateway) {
			return true
		}
	}

	return false
}

func conditionStatusByType(conds []metav1.Condition, typ string) metav1.ConditionStatus {
	for _, cond := range conds {
		if cond.Type == typ {
			return cond.Status
		}
	}

	return metav1.ConditionUnknown
}

func gatewayIsParent(ometa *metav1.ObjectMeta, ref *gwapi.ParentReference, gateway *gwapi.Gateway) bool {
	if ref.Group != nil && *ref.Group != "gateway.networking.k8s.io" {
		return false
	}
	if ref.Kind != nil && *ref.Kind != "Gateway" {
		return false
	}
	ns := ometa.GetNamespace()
	if ref.Namespace != nil {
		ns = string(*ref.Namespace)
	}
	if ns != gateway.GetNamespace() {
		return false
	}

	return string(ref.Name) == gateway.GetName()
}

func routeHandler(queue workqueue.RateLimitingInterface) func(obj interface{}) {
	return func(obj interface{}) {
		var ometa *metav1.ObjectMeta
		var cspec *gwapi.CommonRouteSpec
		switch route := obj.(type) {
		case *gwapi.HTTPRoute:
			ometa = &route.ObjectMeta
			cspec = &route.Spec.CommonRouteSpec

		default:
			runtime.HandleError(fmt.Errorf("not a known route object: %#v", obj))
			return
		}

		for _, ref := range cspec.ParentRefs {
			if ref.Group != nil && *ref.Group != "gateway.networking.k8s.io" {
				continue
			}
			if ref.Kind != nil && *ref.Kind != "Gateway" {
				continue
			}

			ns := ometa.GetNamespace()
			if ref.Namespace != nil {
				ns = string(*ref.Namespace)
			}

			// This may add duplicates, if two parent refs point to
			// the same Gateway, but different sections or ports thereof.
			if ns == "" {
				queue.Add(ref.Name)
			} else {
				queue.Add(ns + "/" + string(ref.Name))
			}
		}
	}
}

// Whenever a Certificate gets updated, added or deleted, we want to reconcile
// its parent Gateway. This parent Gateway is called "controller object". For
// example, the following Certificate "cert-1" is controlled by the Gateway
// "gateway-1":
//
//	kind: Certificate
//	metadata:                                           Note that the owner
//	  namespace: cert-1                                 reference does not
//	  ownerReferences:                                  have a namespace,
//	  - controller: true                                since owner refs
//	    apiVersion: networking.x-k8s.io/v1alpha1        only work inside
//	    kind: Gateway                                   the same namespace.
//	    name: gateway-1
//	    blockOwnerDeletion: true
//	    uid: 7d3897c2-ce27-4144-883a-e1b5f89bd65a
func certificateHandler(queue workqueue.RateLimitingInterface) func(obj interface{}) {
	return func(obj interface{}) {
		crt, ok := obj.(*cmapi.Certificate)
		if !ok {
			runtime.HandleError(fmt.Errorf("not a Certificate object: %#v", obj))
			return
		}

		ref := metav1.GetControllerOf(crt)
		if ref == nil {
			// No controller should care about orphans being deleted or
			// updated.
			return
		}

		// We don't check the apiVersion e.g. "networking.x-k8s.io/v1alpha1"
		// because there is no chance that another object called "Gateway" be
		// the controller of a Certificate.
		if ref.Kind != "Gateway" {
			return
		}

		queue.Add(crt.Namespace + "/" + ref.Name)
	}
}

func init() {
	controllerpkg.Register(ControllerName, func(ctx *controllerpkg.ContextFactory) (controllerpkg.Interface, error) {
		return controllerpkg.NewBuilder(ctx, ControllerName).
			For(&controller{queue: workqueue.NewNamedRateLimitingQueue(controllerpkg.DefaultItemBasedRateLimiter(), ControllerName)}).
			Complete()
	})
}
