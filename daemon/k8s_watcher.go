// Copyright 2016-2017 Authors of Cilium
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

package main

import (
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/cilium/cilium/common/types"
	"github.com/cilium/cilium/pkg/k8s"
	cilium_api "github.com/cilium/cilium/pkg/k8s/apis/cilium.io"
	cilium_v1 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v1"
	cilium_v2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	clientset "github.com/cilium/cilium/pkg/k8s/client/clientset/versioned"
	informer "github.com/cilium/cilium/pkg/k8s/client/informers/externalversions"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logfields"
	"github.com/cilium/cilium/pkg/node"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
)

const (
	k8sErrLogTimeout = time.Minute

	k8sAPIGroupCRD               = "CustomResourceDefinition"
	k8sAPIGroupTPR               = "ThirdPartyResource"
	k8sAPIGroupNodeV1Core        = "core/v1::Node"
	k8sAPIGroupServiceV1Core     = "core/v1::Service"
	k8sAPIGroupEndpointV1Core    = "core/v1::Endpoint"
	k8sAPIGroupNetworkingV1Core  = "networking.k8s.io/v1::NetworkPolicy"
	k8sAPIGroupNetworkingV1Beta1 = "extensions/v1beta1::NetworkPolicy"
	k8sAPIGroupIngressV1Beta1    = "extensions/v1beta1::Ingress"
	k8sAPIGroupCiliumV1          = "cilium/v1::CiliumNetworkPolicy"
	k8sAPIGroupCiliumV2          = "cilium/v2::CiliumNetworkPolicy"
)

var (
	// k8sErrMsgMU guards additions and removals to k8sErrMsg, which stores a
	// time after which a repeat error message can be printed
	k8sErrMsgMU lock.Mutex
	k8sErrMsg   = map[string]time.Time{}

	k8sErrOnceV1API      sync.Once
	stopPolicyController = make(chan struct{})

	ciliumNPClient clientset.Interface
)

// k8sAPIGroupsUsed is a lockable map to hold which k8s API Groups we have
// enabled/in-use
// Note: We can replace it with a Go 1.9 map once we require that version
type k8sAPIGroupsUsed struct {
	sync.Mutex
	apis map[string]bool
}

func (m *k8sAPIGroupsUsed) addAPI(api string) {
	m.Lock()
	defer m.Unlock()
	if m.apis == nil {
		m.apis = make(map[string]bool)
	}
	m.apis[api] = true
}

func (m *k8sAPIGroupsUsed) removeAPI(api string) {
	m.Lock()
	defer m.Unlock()
	delete(m.apis, api)
}

func (m *k8sAPIGroupsUsed) Range(f func(key string, value bool) bool) {
	for k, v := range m.apis {
		if !f(k, v) {
			return
		}
	}
}

func init() {
	// Replace error handler with our own
	runtime.ErrorHandlers = []func(error){
		k8sErrorHandler,
	}
}

// k8sErrorUpdateCheckUnmuteTime returns a boolean indicating whether we should
// log errmsg or not. It manages once-per-k8sErrLogTimeout entry in k8sErrMsg.
// When errmsg is new or more than k8sErrLogTimeout has passed since the last
// invocation that returned true, it returns true.
func k8sErrorUpdateCheckUnmuteTime(errstr string, now time.Time) bool {
	k8sErrMsgMU.Lock()
	defer k8sErrMsgMU.Unlock()

	if unmuteDeadline, ok := k8sErrMsg[errstr]; !ok || now.After(unmuteDeadline) {
		k8sErrMsg[errstr] = now.Add(k8sErrLogTimeout)
		return true
	}

	return false
}

// k8sErrorHandler handles the error messages in a non verbose way by omitting
// repeated instances of the same error message for a timeout defined with
// k8sErrLogTimeout.
func k8sErrorHandler(e error) {
	if e == nil {
		return
	}

	// We rate-limit certain categories of error message. These are matched
	// below, with a default behaviour to print everything else without
	// rate-limiting.
	// Note: We also have side-effects in some of the special cases.
	now := time.Now()
	errstr := e.Error()
	switch {
	// This can occur when cilium comes up before the k8s API server, and keeps
	// trying to connect.
	case strings.Contains(errstr, "connection refused"):
		if k8sErrorUpdateCheckUnmuteTime(errstr, now) {
			log.WithError(e).Error("k8sError")
		}

	// This occurs when running against k8s version that do not support
	// networking.k8s.io/v1 NetworkPolicy specs, k8s <= 1.6. In newer k8s
	// versions both APIVersion: networking.k8s.io/v1 and extensions/v1beta1
	// NetworkPolicy are supported and we do not see an error.
	case strings.Contains(errstr, "Failed to list *v1.NetworkPolicy: the server could not find the requested resource"):
		log.WithError(e).Error("Cannot list v1 API NetworkPolicy resources")
		k8sErrOnceV1API.Do(func() {
			// Stop the v1 API policy controller, which is causing these error
			// messages to occur. This happens when we are talking to a k8s <1.7
			// installation
			log.Warn("k8s <1.7 detected. Some newer k8s API Groups are not available." +
				"For k8s API version compatibilty see http://cilium.readthedocs.io/en/latest/k8scompatibility")
			// This disables the matching watcher set up in EnableK8sWatcher below.
			close(stopPolicyController)
		})

	// k8s does not allow us to watch both ThirdPartyResource and
	// CustomResourceDefinition. This would occur when a user mixes these within
	// the k8s cluster, and might occur when upgrading from versions of cilium
	// that used ThirdPartyResource to define CiliumNetworkPolicy.
	case strings.Contains(errstr, "Failed to list *v2.CiliumNetworkPolicy: the server could not find the requested resource"):
		if k8sErrorUpdateCheckUnmuteTime(errstr, now) {
			log.WithError(e).Error("Conflicting TPR and CRD resources")
			log.Warn("Detected conflicting TPR and CRD, please migrate all ThirdPartyResource to CustomResourceDefinition! More info: https://cilium.link/migrate-tpr")
			log.Warn("Due to conflicting TPR and CRD rules, CiliumNetworkPolicy enforcement can't be guaranteed!")
		}

	// fromCIDR and toCIDR used to expect an "ip" subfield (so, they were a YAML
	// map with one field) but common usage and expectation would simply list the
	// CIDR ranges and IPs desired as a YAML list. In these cases we would see
	// this decode error. We have since changed the definition to be a simple
	// list of strings.
	case strings.Contains(errstr, "Unable to decode an event from the watch stream: unable to decode watch event"),
		strings.Contains(errstr, "Failed to list *v1.CiliumNetworkPolicy: only encoded map or array can be decoded into a struct"),
		strings.Contains(errstr, "Failed to list *v2.CiliumNetworkPolicy: only encoded map or array can be decoded into a struct"),
		strings.Contains(errstr, "Failed to list *v2.CiliumNetworkPolicy: v2.CiliumNetworkPolicyList:"):
		if k8sErrorUpdateCheckUnmuteTime(errstr, now) {
			log.WithError(e).Error("Unable to decode k8s watch event")
		}

	default:
		log.WithError(e).Error("k8sError")
	}
}

// EnableK8sWatcher watches for policy, services and endpoint changes on the Kubernetes
// api server defined in the receiver's daemon k8sClient. Re-syncs all state from the
// Kubernetes api server at the given reSyncPeriod duration.
func (d *Daemon) EnableK8sWatcher(reSyncPeriod time.Duration) error {
	if !k8s.IsEnabled() {
		return nil
	}

	restConfig, err := k8s.CreateConfig()
	if err != nil {
		return fmt.Errorf("Unable to create rest configuration: %s", err)
	}

	apiextensionsclientset, err := apiextensionsclient.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("Unable to create rest configuration for k8s CRD: %s", err)
	}

	ciliumCLIVersion := cilium_api.V1
	err = cilium_v2.CreateCustomResourceDefinitions(apiextensionsclientset)
	switch {
	case errors.IsNotFound(err):
		// If CRD was not found it means we are running in k8s <1.7
		// then we should set up TPR instead
		log.Debug("Detected k8s <1.7, using TPR instead of CRD")
		err := cilium_v1.CreateThirdPartyResourcesDefinitions(k8s.Client())
		if err != nil {
			return fmt.Errorf("Unable to create third party resource: %s", err)
		}
		d.k8sAPIGroups.addAPI(k8sAPIGroupTPR)
		d.k8sAPIGroups.addAPI(k8sAPIGroupCiliumV1)

	case err != nil:
		return fmt.Errorf("Unable to create custom resource definition: %s", err)

	default:
		ciliumCLIVersion = cilium_api.V2
		d.k8sAPIGroups.addAPI(k8sAPIGroupCRD)
		d.k8sAPIGroups.addAPI(k8sAPIGroupCiliumV2)
	}

	ciliumNPClient, err = clientset.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("Unable to create cilium network policy client: %s", err)
	}

	_, policyControllerDeprecated := cache.NewInformer(
		cache.NewListWatchFromClient(k8s.Client().ExtensionsV1beta1().RESTClient(),
			"networkpolicies", v1.NamespaceAll, fields.Everything()),
		&v1beta1.NetworkPolicy{},
		reSyncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    d.addK8sNetworkPolicyV1beta1,
			UpdateFunc: d.updateK8sNetworkPolicyV1beta1,
			DeleteFunc: d.deleteK8sNetworkPolicyV1beta1,
		},
	)
	go policyControllerDeprecated.Run(wait.NeverStop)
	d.k8sAPIGroups.addAPI(k8sAPIGroupNetworkingV1Beta1)

	_, policyController := cache.NewInformer(
		cache.NewListWatchFromClient(k8s.Client().NetworkingV1().RESTClient(),
			"networkpolicies", v1.NamespaceAll, fields.Everything()),
		&networkingv1.NetworkPolicy{},
		reSyncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    d.addK8sNetworkPolicyV1,
			UpdateFunc: d.updateK8sNetworkPolicyV1,
			DeleteFunc: d.deleteK8sNetworkPolicyV1,
		},
	)
	go policyController.Run(stopPolicyController)
	d.k8sAPIGroups.addAPI(k8sAPIGroupNetworkingV1Core)
	// This is here because we turn this off in k8sErrorHandler but it does not
	// have a *Daemon pointer.
	// Note: We put stopPolicyController in the closure in case the global is
	// ever changed.
	go func(stop chan struct{}) {
		<-stop
		d.k8sAPIGroups.removeAPI(k8sAPIGroupNetworkingV1Core)
	}(stopPolicyController)

	_, svcController := cache.NewInformer(
		cache.NewListWatchFromClient(k8s.Client().CoreV1().RESTClient(),
			"services", v1.NamespaceAll, fields.Everything()),
		&v1.Service{},
		reSyncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    d.addK8sServiceV1,
			UpdateFunc: d.updateK8sServiceV1,
			DeleteFunc: d.deleteK8sServiceV1,
		},
	)
	go svcController.Run(wait.NeverStop)
	d.k8sAPIGroups.addAPI(k8sAPIGroupServiceV1Core)

	_, endpointController := cache.NewInformer(
		cache.NewListWatchFromClient(k8s.Client().CoreV1().RESTClient(),
			"endpoints", v1.NamespaceAll, fields.Everything()),
		&v1.Endpoints{},
		reSyncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    d.addK8sEndpointV1,
			UpdateFunc: d.updateK8sEndpointV1,
			DeleteFunc: d.deleteK8sEndpointV1,
		},
	)
	go endpointController.Run(wait.NeverStop)
	d.k8sAPIGroups.addAPI(k8sAPIGroupEndpointV1Core)

	_, ingressController := cache.NewInformer(
		cache.NewListWatchFromClient(k8s.Client().ExtensionsV1beta1().RESTClient(),
			"ingresses", v1.NamespaceAll, fields.Everything()),
		&v1beta1.Ingress{},
		reSyncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    d.addIngressV1beta1,
			UpdateFunc: d.updateIngressV1beta1,
			DeleteFunc: d.deleteIngressV1beta1,
		},
	)
	go ingressController.Run(wait.NeverStop)
	d.k8sAPIGroups.addAPI(k8sAPIGroupIngressV1Beta1)

	si := informer.NewSharedInformerFactory(ciliumNPClient, reSyncPeriod)

	switch ciliumCLIVersion {
	case cilium_api.V1:
		ciliumV1Controller := si.Cilium().V1().CiliumNetworkPolicies().Informer()
		cnpStore := ciliumV1Controller.GetStore()
		ciliumV1Controller.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				d.addCiliumNetworkPolicyV1(cnpStore, obj)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				d.updateCiliumNetworkPolicyV1(cnpStore, oldObj, newObj)
			},
			DeleteFunc: func(obj interface{}) {
				d.deleteCiliumNetworkPolicyV1(obj)
			},
		})

	default:
		ciliumV2Controller := si.Cilium().V2().CiliumNetworkPolicies().Informer()
		cnpStore := ciliumV2Controller.GetStore()
		cnpHandler := cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				d.addCiliumNetworkPolicyV2(cnpStore, obj)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				d.updateCiliumNetworkPolicyV2(cnpStore, oldObj, newObj)
			},
			DeleteFunc: func(obj interface{}) {
				d.deleteCiliumNetworkPolicyV2(obj)
			},
		}

		ciliumV2Controller.AddEventHandler(cnpHandler)
	}

	si.Start(wait.NeverStop)

	_, nodesController := cache.NewInformer(
		cache.NewListWatchFromClient(k8s.Client().CoreV1().RESTClient(),
			"nodes", v1.NamespaceAll, fields.Everything()),
		&v1.Node{},
		reSyncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    d.addK8sNodeV1,
			UpdateFunc: d.updateK8sNodeV1,
			DeleteFunc: d.deleteK8sNodeV1,
		},
	)
	go nodesController.Run(wait.NeverStop)
	d.k8sAPIGroups.addAPI(k8sAPIGroupNodeV1Core)

	return nil
}

func (d *Daemon) addK8sNetworkPolicyV1(obj interface{}) {
	// We don't need to deepcopy the object since we are creating a Cilium
	// Network Policy rule with ParseNetworkPolicy below.
	k8sNP, ok := obj.(*networkingv1.NetworkPolicy)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).
			Warn("Ignoring invalid k8s NetworkPolicy addition")
		return
	}

	scopedLog := log.WithField(logfields.K8sAPIVersion, k8sNP.TypeMeta.APIVersion)
	rules, err := k8s.ParseNetworkPolicy(k8sNP)
	if err != nil {
		scopedLog.WithError(err).WithFields(log.Fields{
			logfields.CiliumNetworkPolicy: logfields.Repr(k8sNP),
		}).Error("Error while parsing k8s kubernetes NetworkPolicy")
		return
	}
	scopedLog = scopedLog.WithField(logfields.K8sNetworkPolicyName, k8sNP.ObjectMeta.Name)

	opts := AddOptions{Replace: true}
	if _, err := d.PolicyAdd(rules, &opts); err != nil {
		scopedLog.WithError(err).WithFields(log.Fields{
			logfields.CiliumNetworkPolicy: logfields.Repr(rules),
		}).Error("Unable to add NetworkPolicy rules to policy repository")
		return
	}

	scopedLog.Info("NetworkPolicy successfully added")
}

func (d *Daemon) updateK8sNetworkPolicyV1(oldObj interface{}, newObj interface{}) {
	// We don't need to deepcopy the object since we are creating a Cilium
	// Network Policy rule with ParseNetworkPolicy below.
	oldk8sNP, ok := oldObj.(*networkingv1.NetworkPolicy)
	if !ok {
		log.WithField(logfields.Object+".old", logfields.Repr(oldObj)).
			Warn("Ignoring invalid k8s NetworkPolicy modification")
		return
	}
	newk8sNP, ok := newObj.(*networkingv1.NetworkPolicy)
	if !ok {
		log.WithField(logfields.Object+".new", logfields.Repr(newk8sNP)).
			Warn("Ignoring invalid k8s NetworkPolicy modification")
		return
	}
	log.WithFields(log.Fields{
		logfields.K8sAPIVersion:                 oldk8sNP.TypeMeta.APIVersion,
		logfields.K8sNetworkPolicyName + ".old": oldk8sNP.ObjectMeta.Name,
		logfields.K8sNamespace + ".old":         oldk8sNP.ObjectMeta.Namespace,
		logfields.K8sNetworkPolicyName + ".new": newk8sNP.ObjectMeta.Name,
		logfields.K8sNamespace + ".new":         newk8sNP.ObjectMeta.Namespace,
	}).Debug("Received policy update")

	d.addK8sNetworkPolicyV1(newObj)
}

func (d *Daemon) deleteK8sNetworkPolicyV1(obj interface{}) {
	// We don't need to deepcopy the object since we are creating a Cilium
	// Network Policy rule with ParseNetworkPolicy below.
	k8sNP, ok := obj.(*networkingv1.NetworkPolicy)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).
			Warn("Ignoring invalid k8s NetworkPolicy deletion")
		return
	}

	labels := labels.ParseSelectLabelArray(k8s.ExtractPolicyName(k8sNP))

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sNetworkPolicyName: k8sNP.ObjectMeta.Name,
		logfields.K8sNamespace:         k8sNP.ObjectMeta.Namespace,
		logfields.K8sAPIVersion:        k8sNP.TypeMeta.APIVersion,
		logfields.Labels:               logfields.Repr(labels),
	})
	if _, err := d.PolicyDelete(labels); err != nil {
		scopedLog.WithError(err).Error("Error while deleting k8s NetworkPolicy")
	} else {
		scopedLog.Info("NetworkPolicy successfully removed")
	}
}

// addK8sNetworkPolicyV1beta1
// FIXME remove when we drop support to k8s Network Policy extensions/v1beta1
func (d *Daemon) addK8sNetworkPolicyV1beta1(obj interface{}) {
	// We don't need to deepcopy the object since we are creating a Cilium
	// Network Policy rule with ParseNetworkPolicy below.
	k8sNP, ok := obj.(*v1beta1.NetworkPolicy)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).
			Warn("Ignoring invalid k8s v1beta1 NetworkPolicy addition")
		return
	}
	scopedLog := log.WithField(logfields.K8sAPIVersion, k8sNP.TypeMeta.APIVersion)
	rules, err := k8s.ParseNetworkPolicyDeprecated(k8sNP)
	if err != nil {
		scopedLog.WithError(err).WithField(logfields.Object, logfields.Repr(obj)).Error("Error while parsing k8s NetworkPolicy")
		return
	}

	scopedLog = scopedLog.WithField(logfields.K8sNetworkPolicyName, k8sNP.ObjectMeta.Name)

	opts := AddOptions{Replace: true}
	if _, err := d.PolicyAdd(rules, &opts); err != nil {
		scopedLog.WithField(logfields.Object, logfields.Repr(rules)).Error("Error while parsing k8s NetworkPolicy")
		return
	}

	scopedLog.Info("NetworkPolicy successfully added")
}

// updateK8sNetworkPolicyV1beta1
// FIXME remove when we drop support to k8s Network Policy extensions/v1beta1
func (d *Daemon) updateK8sNetworkPolicyV1beta1(oldObj interface{}, newObj interface{}) {
	// We don't need to deepcopy the object since we are creating a Cilium
	// Network Policy rule with ParseNetworkPolicy below.
	oldk8sNP, ok := oldObj.(*v1beta1.NetworkPolicy)
	if !ok {
		log.WithField(logfields.Object+".old", logfields.Repr(oldObj)).
			Warn("Ignoring invalid k8s v1beta1 NetworkPolicy modification")
		return
	}
	newk8sNP, ok := newObj.(*v1beta1.NetworkPolicy)
	if !ok {
		log.WithField(logfields.Object+".new", logfields.Repr(newObj)).
			Warn("Ignoring invalid k8s v1beta1 NetworkPolicy modification")
		return
	}
	log.WithFields(log.Fields{
		logfields.K8sAPIVersion:                 oldk8sNP.TypeMeta.APIVersion,
		logfields.K8sNetworkPolicyName + ".old": oldk8sNP.ObjectMeta.Name,
		logfields.K8sNamespace + ".old":         oldk8sNP.ObjectMeta.Namespace,
		logfields.K8sNetworkPolicyName + ".new": newk8sNP.ObjectMeta.Name,
		logfields.K8sNamespace + ".new":         newk8sNP.ObjectMeta.Namespace,
	}).Debug("Received policy update")

	d.addK8sNetworkPolicyV1beta1(newObj)
}

// deleteK8sNetworkPolicyV1beta1
// FIXME remove when we drop support to k8s Network Policy extensions/v1beta1
func (d *Daemon) deleteK8sNetworkPolicyV1beta1(obj interface{}) {
	// We don't need to deepcopy the object since we are creating a Cilium
	// Network Policy rule with ParseNetworkPolicy below.
	k8sNP, ok := obj.(*v1beta1.NetworkPolicy)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).
			Warn("Ignoring invalid k8s v1beta1 NetworkPolicy deletion")
		return
	}

	labels := labels.ParseSelectLabelArray(k8s.ExtractPolicyNameDeprecated(k8sNP))

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sNetworkPolicyName: k8sNP.ObjectMeta.Name,
		logfields.K8sNamespace:         k8sNP.ObjectMeta.Namespace,
		logfields.K8sAPIVersion:        k8sNP.TypeMeta.APIVersion,
		logfields.Labels:               logfields.Repr(labels),
	})

	if _, err := d.PolicyDelete(labels); err != nil {
		scopedLog.WithError(err).Error("Error while deleting k8s NetworkPolicy")
	} else {
		scopedLog.Info("NetworkPolicy successfully removed")
	}
}

func (d *Daemon) addK8sServiceV1(obj interface{}) {
	// We don't need to deepcopy the object since we are creating a
	// types.K8sServiceInfo object with NewK8sServiceInfo below.
	svc, ok := obj.(*v1.Service)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).
			Warn("Ignoring invalid k8s Service addition")
		return
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sSvcName:    svc.ObjectMeta.Name,
		logfields.K8sNamespace:  svc.ObjectMeta.Namespace,
		logfields.K8sAPIVersion: svc.TypeMeta.APIVersion,
		logfields.K8sSvcType:    svc.Spec.Type,
	})

	switch svc.Spec.Type {
	case v1.ServiceTypeClusterIP, v1.ServiceTypeNodePort, v1.ServiceTypeLoadBalancer:
		break

	case v1.ServiceTypeExternalName:
		// External-name services must be ignored
		return

	default:
		scopedLog.Warn("Ignoring k8s service: unsupported type")
		return
	}

	if svc.Spec.ClusterIP == "" {
		scopedLog.Info("Ignoring k8s service: empty ClusterIP")
		return
	}

	svcns := types.K8sServiceNamespace{
		ServiceName: svc.ObjectMeta.Name,
		Namespace:   svc.ObjectMeta.Namespace,
	}

	clusterIP := net.ParseIP(svc.Spec.ClusterIP)
	headless := false
	if strings.ToLower(svc.Spec.ClusterIP) == "none" {
		headless = true
	}
	newSI := types.NewK8sServiceInfo(clusterIP, headless)

	// FIXME: Add support for
	//  - NodePort

	for _, port := range svc.Spec.Ports {
		p, err := types.NewFEPort(types.L4Type(port.Protocol), uint16(port.Port))
		if err != nil {
			scopedLog.WithError(err).WithField("port", port).Error("Unable to add service port")
			continue
		}
		if _, ok := newSI.Ports[types.FEPortName(port.Name)]; !ok {
			newSI.Ports[types.FEPortName(port.Name)] = p
		}
	}

	d.loadBalancer.K8sMU.Lock()
	defer d.loadBalancer.K8sMU.Unlock()

	d.loadBalancer.K8sServices[svcns] = newSI

	d.syncLB(&svcns, nil, nil)
}

func (d *Daemon) updateK8sServiceV1(oldObj interface{}, newObj interface{}) {
	// We don't need to deepcopy the object since we d.addK8sServiceV1 will do
	// that.
	oldSvc, ok := oldObj.(*v1.Service)
	if !ok {
		log.WithField(logfields.Object+".old", logfields.Repr(oldObj)).
			Warn("Ignoring invalid k8s Service modification")
		return
	}
	newSvc, ok := newObj.(*v1.Service)
	if !ok {
		log.WithField(logfields.Object+".new", logfields.Repr(newObj)).
			Warn("Ignoring invalid k8s Service modification")
		return
	}
	log.WithFields(log.Fields{
		logfields.K8sAPIVersion:         oldSvc.TypeMeta.APIVersion,
		logfields.K8sSvcName + ".old":   oldSvc.ObjectMeta.Name,
		logfields.K8sNamespace + ".old": oldSvc.ObjectMeta.Namespace,
		logfields.K8sSvcType + ".old":   oldSvc.Spec.Type,
		logfields.K8sSvcName + ".new":   newSvc.ObjectMeta.Name,
		logfields.K8sNamespace + ".new": newSvc.ObjectMeta.Namespace,
		logfields.K8sSvcType + ".new":   newSvc.Spec.Type,
	}).Debug("Received service update")

	d.addK8sServiceV1(newObj)
}

func (d *Daemon) deleteK8sServiceV1(obj interface{}) {
	// We don't need to deepcopy the object since we are creating a
	// types.K8sServiceNamespace below.
	svc, ok := obj.(*v1.Service)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).
			Warn("Ignoring invalid k8s Service deletion")
		return
	}
	log.WithFields(log.Fields{
		logfields.K8sSvcName:    svc.ObjectMeta.Name,
		logfields.K8sNamespace:  svc.ObjectMeta.Namespace,
		logfields.K8sAPIVersion: svc.TypeMeta.APIVersion,
	}).Debug("Deleting k8s service")

	svcns := &types.K8sServiceNamespace{
		ServiceName: svc.ObjectMeta.Name,
		Namespace:   svc.ObjectMeta.Namespace,
	}

	d.loadBalancer.K8sMU.Lock()
	defer d.loadBalancer.K8sMU.Unlock()
	d.syncLB(nil, nil, svcns)
}

func (d *Daemon) addK8sEndpointV1(obj interface{}) {
	// We don't need to deepcopy the object since we are creating a
	// types.K8sServiceEndpoint below.
	ep, ok := obj.(*v1.Endpoints)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).
			Warn("Ignoring invalid k8s Endpoint addition")
		return
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sEndpointName: ep.ObjectMeta.Name,
		logfields.K8sNamespace:    ep.ObjectMeta.Namespace,
		logfields.K8sAPIVersion:   ep.TypeMeta.APIVersion,
	})

	svcns := types.K8sServiceNamespace{
		ServiceName: ep.ObjectMeta.Name,
		Namespace:   ep.ObjectMeta.Namespace,
	}

	newSvcEP := types.NewK8sServiceEndpoint()

	for _, sub := range ep.Subsets {
		for _, addr := range sub.Addresses {
			newSvcEP.BEIPs[addr.IP] = true
		}
		for _, port := range sub.Ports {
			lbPort, err := types.NewL4Addr(types.L4Type(port.Protocol), uint16(port.Port))
			if err != nil {
				scopedLog.WithError(err).Error("Error while creating a new LB Port")
				continue
			}
			newSvcEP.Ports[types.FEPortName(port.Name)] = lbPort
		}
	}

	d.loadBalancer.K8sMU.Lock()
	defer d.loadBalancer.K8sMU.Unlock()

	d.loadBalancer.K8sEndpoints[svcns] = newSvcEP

	d.syncLB(&svcns, nil, nil)

	if d.conf.IsLBEnabled() {
		if err := d.syncExternalLB(&svcns, nil, nil); err != nil {
			scopedLog.WithError(err).Error("Unable to add endpoints on ingress service")
			return
		}
	}

	svc, ok := d.loadBalancer.K8sServices[svcns]
	if ok && svc.IsHeadless {
		translator := k8s.NewK8sTranslator(svcns, *newSvcEP, false)
		err := d.policy.TranslateRules(translator)
		if err != nil {
			log.Errorf("Unable to repopulate egress policies from ToService rules: %v", err)
		}
	}
}

func (d *Daemon) updateK8sEndpointV1(oldObj interface{}, newObj interface{}) {
	// We don't need to deepcopy the object since we are creating a
	// types.K8sServiceEndpoint in d.addK8sEndpointV1.
	_, ok := oldObj.(*v1.Endpoints)
	if !ok {
		log.WithField(logfields.Object+".old", logfields.Repr(oldObj)).
			Warn("Ignoring invalid k8s Endpoint modification")
		return
	}
	_, ok = newObj.(*v1.Endpoints)
	if !ok {
		log.WithField(logfields.Object+".new", logfields.Repr(newObj)).
			Warn("Ignoring invalid k8s Endpoint modification")
		return
	}
	// TODO only print debug message if the difference between the old endpoint
	// and the new endpoint are important to us.
	//log.WithFields(log.Fields{
	//	logfields.K8sAPIVersion:            oldEP.TypeMeta.APIVersion,
	//	logfields.K8sEndpointName + ".old": oldEP.ObjectMeta.Name,
	//	logfields.K8sNamespace + ".old":    oldEP.ObjectMeta.Namespace,
	//	logfields.K8sEndpointName + ".new": newEP.ObjectMeta.Name,
	//	logfields.K8sNamespace + ".new":    newEP.ObjectMeta.Namespace,
	//}).Debug("Received endpoint update")

	d.addK8sEndpointV1(newObj)
}

func (d *Daemon) deleteK8sEndpointV1(obj interface{}) {
	// We don't need to deepcopy the object since we are creating a
	// types.K8sServiceNamespace below.
	ep, ok := obj.(*v1.Endpoints)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).
			Warn("Ignoring invalid k8s Endpoint deletion")
		return
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sEndpointName: ep.ObjectMeta.Name,
		logfields.K8sNamespace:    ep.ObjectMeta.Namespace,
		logfields.K8sAPIVersion:   ep.TypeMeta.APIVersion,
	})

	svcns := types.K8sServiceNamespace{
		ServiceName: ep.ObjectMeta.Name,
		Namespace:   ep.ObjectMeta.Namespace,
	}

	d.loadBalancer.K8sMU.Lock()
	defer d.loadBalancer.K8sMU.Unlock()

	if endpoint, ok := d.loadBalancer.K8sEndpoints[svcns]; ok {
		svc, ok := d.loadBalancer.K8sServices[svcns]
		if ok && svc.IsHeadless {
			translator := k8s.NewK8sTranslator(svcns, *endpoint, true)
			err := d.policy.TranslateRules(translator)
			if err != nil {
				log.Errorf("Unable to depopulate egress policies from ToService rules: %v", err)
			}
		}
	}

	d.syncLB(nil, nil, &svcns)
	if d.conf.IsLBEnabled() {
		if err := d.syncExternalLB(nil, nil, &svcns); err != nil {
			scopedLog.WithError(err).Error("Unable to remove endpoints on ingress service")
			return
		}
	}
}

func areIPsConsistent(ipv4Enabled, isSvcIPv4 bool, svc types.K8sServiceNamespace, se *types.K8sServiceEndpoint) error {
	if isSvcIPv4 {
		if !ipv4Enabled {
			return fmt.Errorf("Received an IPv4 k8s service but IPv4 is "+
				"disabled in the cilium daemon. Ignoring service %+v", svc)
		}

		for epIP := range se.BEIPs {
			//is IPv6?
			if net.ParseIP(epIP).To4() == nil {
				return fmt.Errorf("Not all endpoints IPs are IPv4. Ignoring IPv4 service %+v", svc)
			}
		}
	} else {
		for epIP := range se.BEIPs {
			//is IPv4?
			if net.ParseIP(epIP).To4() != nil {
				return fmt.Errorf("Not all endpoints IPs are IPv6. Ignoring IPv6 service %+v", svc)
			}
		}
	}
	return nil
}

func getUniqPorts(svcPorts map[types.FEPortName]*types.FEPort) map[uint16]bool {
	// We are not discriminating the different L4 protocols on the same L4
	// port so we create the number of unique sets of service IP + service
	// port.
	uniqPorts := map[uint16]bool{}
	for _, svcPort := range svcPorts {
		uniqPorts[svcPort.Port] = true
	}
	return uniqPorts
}

func (d *Daemon) delK8sSVCs(svc types.K8sServiceNamespace, svcInfo *types.K8sServiceInfo, se *types.K8sServiceEndpoint) error {
	// If east-west load balancing is disabled, we should not sync(add or delete)
	// K8s service to a cilium service.
	if lb := viper.GetBool("disable-k8s-services"); lb == true {
		return nil
	}
	isSvcIPv4 := svcInfo.FEIP.To4() != nil
	if err := areIPsConsistent(!d.conf.IPv4Disabled, isSvcIPv4, svc, se); err != nil {
		return err
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sSvcName:   svc.ServiceName,
		logfields.K8sNamespace: svc.Namespace,
	})

	repPorts := getUniqPorts(svcInfo.Ports)

	for _, svcPort := range svcInfo.Ports {
		if !repPorts[svcPort.Port] {
			continue
		}
		repPorts[svcPort.Port] = false

		if svcPort.ID != 0 {
			if err := DeleteL3n4AddrIDByUUID(uint32(svcPort.ID)); err != nil {
				scopedLog.WithError(err).Warn("Error while cleaning service ID")
			}
		}

		fe, err := types.NewL3n4Addr(svcPort.Protocol, svcInfo.FEIP, svcPort.Port)
		if err != nil {
			scopedLog.WithError(err).Error("Error while creating a New L3n4AddrID. Ignoring service")
			continue
		}

		if err := d.svcDeleteByFrontend(fe); err != nil {
			scopedLog.WithError(err).WithField(logfields.Object, logfields.Repr(fe)).
				Warn("Error deleting service by frontend")

		} else {
			scopedLog.Debugf("# cilium lb delete-service %s %d 0", svcInfo.FEIP, svcPort.Port)
		}

		if err := d.RevNATDelete(svcPort.ID); err != nil {
			scopedLog.WithError(err).WithField(logfields.ServiceID, svcPort.ID).Warn("Error deleting reverse NAT")
		} else {
			scopedLog.Debugf("# cilium lb delete-rev-nat %d", svcPort.ID)
		}
	}
	return nil
}

func (d *Daemon) addK8sSVCs(svc types.K8sServiceNamespace, svcInfo *types.K8sServiceInfo, se *types.K8sServiceEndpoint) error {
	// If east-west load balancing is disabled, we should not sync(add or delete)
	// K8s service to a cilium service.
	if lb := viper.GetBool("disable-k8s-services"); lb == true {
		return nil
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sSvcName:   svc.ServiceName,
		logfields.K8sNamespace: svc.Namespace,
	})

	isSvcIPv4 := svcInfo.FEIP.To4() != nil
	if err := areIPsConsistent(!d.conf.IPv4Disabled, isSvcIPv4, svc, se); err != nil {
		return err
	}

	uniqPorts := getUniqPorts(svcInfo.Ports)

	for fePortName, fePort := range svcInfo.Ports {
		if !uniqPorts[fePort.Port] {
			continue
		}

		k8sBEPort := se.Ports[fePortName]
		uniqPorts[fePort.Port] = false

		if fePort.ID == 0 {
			feAddr, err := types.NewL3n4Addr(fePort.Protocol, svcInfo.FEIP, fePort.Port)
			if err != nil {
				scopedLog.WithError(err).WithFields(log.Fields{
					logfields.ServiceID: fePortName,
					logfields.IPAddr:    svcInfo.FEIP,
					logfields.Port:      fePort.Port,
					logfields.Protocol:  fePort.Protocol,
				}).Error("Error while creating a new L3n4Addr. Ignoring service...")
				continue
			}
			feAddrID, err := PutL3n4Addr(*feAddr, 0)
			if err != nil {
				scopedLog.WithError(err).WithFields(log.Fields{
					logfields.ServiceID: fePortName,
					logfields.IPAddr:    svcInfo.FEIP,
					logfields.Port:      fePort.Port,
					logfields.Protocol:  fePort.Protocol,
				}).Error("Error while getting a new service ID. Ignoring service...")
				continue
			}
			scopedLog.WithFields(log.Fields{
				logfields.ServiceName: fePortName,
				logfields.ServiceID:   feAddrID.ID,
				logfields.Object:      logfields.Repr(svc),
			}).Debug("Got feAddr ID for service")
			fePort.ID = feAddrID.ID
		}

		besValues := []types.LBBackEnd{}

		if k8sBEPort != nil {
			for epIP := range se.BEIPs {
				bePort := types.LBBackEnd{
					L3n4Addr: types.L3n4Addr{IP: net.ParseIP(epIP), L4Addr: *k8sBEPort},
					Weight:   0,
				}
				besValues = append(besValues, bePort)
			}
		}

		fe, err := types.NewL3n4AddrID(fePort.Protocol, svcInfo.FEIP, fePort.Port, fePort.ID)
		if err != nil {
			scopedLog.WithError(err).WithFields(log.Fields{
				logfields.IPAddr: svcInfo.FEIP,
				logfields.Port:   svcInfo.Ports,
			}).Error("Error while creating a New L3n4AddrID. Ignoring service...")
			continue
		}
		if _, err := d.svcAdd(*fe, besValues, true); err != nil {
			scopedLog.WithError(err).Error("Error while inserting service in LB map")
		}
	}
	return nil
}

func (d *Daemon) syncLB(newSN, modSN, delSN *types.K8sServiceNamespace) {
	deleteSN := func(delSN types.K8sServiceNamespace) {
		svc, ok := d.loadBalancer.K8sServices[delSN]
		if !ok {
			delete(d.loadBalancer.K8sEndpoints, delSN)
			return
		}

		endpoint, ok := d.loadBalancer.K8sEndpoints[delSN]
		if !ok {
			delete(d.loadBalancer.K8sServices, delSN)
			return
		}

		if err := d.delK8sSVCs(delSN, svc, endpoint); err != nil {
			log.WithError(err).WithFields(log.Fields{
				logfields.K8sSvcName:   delSN.ServiceName,
				logfields.K8sNamespace: delSN.Namespace,
			}).Error("Unable to delete k8s service")
			return
		}

		delete(d.loadBalancer.K8sServices, delSN)
		delete(d.loadBalancer.K8sEndpoints, delSN)
	}

	addSN := func(addSN types.K8sServiceNamespace) {
		svcInfo, ok := d.loadBalancer.K8sServices[addSN]
		if !ok {
			return
		}

		endpoint, ok := d.loadBalancer.K8sEndpoints[addSN]
		if !ok {
			return
		}

		if err := d.addK8sSVCs(addSN, svcInfo, endpoint); err != nil {
			log.WithError(err).WithFields(log.Fields{
				logfields.K8sSvcName:   addSN.ServiceName,
				logfields.K8sNamespace: addSN.Namespace,
			}).Error("Unable to add k8s service")
		}
	}

	if delSN != nil {
		// Clean old services
		deleteSN(*delSN)
	}
	if modSN != nil {
		// Re-add modified services
		addSN(*modSN)
	}
	if newSN != nil {
		// Add new services
		addSN(*newSN)
	}
}

func (d *Daemon) addIngressV1beta1(obj interface{}) {
	if !d.conf.IsLBEnabled() {
		// Add operations don't matter to non-LB nodes.
		return
	}
	ingress, ok := obj.(*v1beta1.Ingress)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).
			Warn("Ignoring invalid k8s v1beta1 Ingress addition")
		return
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sIngressName: ingress.ObjectMeta.Name,
		logfields.K8sAPIVersion:  ingress.TypeMeta.APIVersion,
		logfields.K8sNamespace:   ingress.ObjectMeta.Namespace,
	})

	if ingress.Spec.Backend == nil {
		// We only support Single Service Ingress for now
		scopedLog.Warn("Cilium only supports Single Service Ingress for now, ignoring ingress")
		return
	}

	svcName := types.K8sServiceNamespace{
		ServiceName: ingress.Spec.Backend.ServiceName,
		Namespace:   ingress.ObjectMeta.Namespace,
	}

	ingressPort := ingress.Spec.Backend.ServicePort.IntValue()
	fePort, err := types.NewFEPort(types.TCP, uint16(ingressPort))
	if err != nil {
		return
	}

	var host net.IP
	if d.conf.IPv4Disabled {
		host = d.conf.HostV6Addr
	} else {
		host = d.conf.HostV4Addr
	}
	ingressSvcInfo := types.NewK8sServiceInfo(host, false)
	ingressSvcInfo.Ports[types.FEPortName(ingress.Spec.Backend.ServicePort.StrVal)] = fePort

	syncIngress := func(ingressSvcInfo *types.K8sServiceInfo) error {
		d.loadBalancer.K8sIngress[svcName] = ingressSvcInfo

		if err := d.syncExternalLB(&svcName, nil, nil); err != nil {
			return fmt.Errorf("Unable to add ingress service %s: %s", svcName, err)
		}
		return nil
	}

	d.loadBalancer.K8sMU.Lock()
	err = syncIngress(ingressSvcInfo)
	d.loadBalancer.K8sMU.Unlock()
	if err != nil {
		scopedLog.WithError(err).Error("Error in syncIngress")
		return
	}

	hostname, _ := os.Hostname()
	dpyCopyIngress := ingress.DeepCopy()
	dpyCopyIngress.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{
		{
			IP:       host.String(),
			Hostname: hostname,
		},
	}

	_, err = k8s.Client().ExtensionsV1beta1().Ingresses(dpyCopyIngress.ObjectMeta.Namespace).UpdateStatus(dpyCopyIngress)
	if err != nil {
		scopedLog.WithError(err).WithFields(log.Fields{
			logfields.K8sIngress: dpyCopyIngress,
		}).Error("Unable to update status of ingress")
		return
	}
}

func (d *Daemon) updateIngressV1beta1(oldObj interface{}, newObj interface{}) {
	// We don't need to deepcopy the objects since that copy will be done
	// on the addIngressV1beta1.
	oldIngress, ok := oldObj.(*v1beta1.Ingress)
	if !ok {
		log.WithField(logfields.Object+".old", logfields.Repr(oldIngress)).
			Warn("Ignoring invalid k8s v1beta1 Ingress modification")
		return
	}
	newIngress, ok := newObj.(*v1beta1.Ingress)
	if !ok {
		log.WithField(logfields.Object+".new", logfields.Repr(newIngress)).
			Warn("Ignoring invalid k8s v1beta1 Ingress modification")
		return
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sIngressName + ".old": oldIngress.ObjectMeta.Name,
		logfields.K8sAPIVersion + ".old":  oldIngress.TypeMeta.APIVersion,
		logfields.K8sNamespace + ".old":   oldIngress.ObjectMeta.Namespace,
		logfields.K8sIngressName + ".new": newIngress.ObjectMeta.Name,
		logfields.K8sAPIVersion + ".new":  newIngress.TypeMeta.APIVersion,
		logfields.K8sNamespace + ".new":   newIngress.ObjectMeta.Namespace,
	})

	if oldIngress.Spec.Backend == nil || newIngress.Spec.Backend == nil {
		// We only support Single Service Ingress for now
		scopedLog.Warn("Cilium only supports Single Service Ingress for now, ignoring ingress")
		return
	}

	// Add RevNAT to the BPF Map for non-LB nodes when a LB node update the
	// ingress status with its address.
	if !d.conf.IsLBEnabled() {
		port := newIngress.Spec.Backend.ServicePort.IntValue()
		for _, loadbalancer := range newIngress.Status.LoadBalancer.Ingress {
			ingressIP := net.ParseIP(loadbalancer.IP)
			if ingressIP == nil {
				continue
			}
			feAddr, err := types.NewL3n4Addr(types.TCP, ingressIP, uint16(port))
			if err != nil {
				scopedLog.WithError(err).Error("Error while creating a new L3n4Addr. Ignoring ingress...")
				continue
			}
			feAddrID, err := PutL3n4Addr(*feAddr, 0)
			if err != nil {
				scopedLog.WithError(err).Error("Error while getting a new service ID. Ignoring ingress...")
				continue
			}
			scopedLog.WithFields(log.Fields{
				logfields.ServiceID: feAddrID.ID,
			}).Debug("Got service ID for ingress")

			if err := d.RevNATAdd(feAddrID.ID, feAddrID.L3n4Addr); err != nil {
				scopedLog.WithError(err).WithFields(log.Fields{
					logfields.ServiceID: feAddrID.ID,
					logfields.IPAddr:    feAddrID.L3n4Addr.IP,
					logfields.Port:      feAddrID.L3n4Addr.Port,
					logfields.Protocol:  feAddrID.L3n4Addr.Protocol,
				}).Error("Unable to add reverse NAT ID for ingress")
			}
		}
		return
	}

	if oldIngress.Spec.Backend.ServiceName == newIngress.Spec.Backend.ServiceName &&
		oldIngress.Spec.Backend.ServicePort == newIngress.Spec.Backend.ServicePort {
		return
	}

	d.addIngressV1beta1(newObj)
}

func (d *Daemon) deleteIngressV1beta1(obj interface{}) {
	// We don't need to deepcopy the object since we are reading the ingress
	// attributes.
	ingress, ok := obj.(*v1beta1.Ingress)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).
			Warn("Ignoring invalid k8s v1beta1 Ingress deletion")
		return
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sIngressName: ingress.ObjectMeta.Name,
		logfields.K8sAPIVersion:  ingress.TypeMeta.APIVersion,
		logfields.K8sNamespace:   ingress.ObjectMeta.Namespace,
	})

	if ingress.Spec.Backend == nil {
		// We only support Single Service Ingress for now
		scopedLog.Warn("Cilium only supports Single Service Ingress for now, ignoring ingress deletion")
		return
	}

	svcName := types.K8sServiceNamespace{
		ServiceName: ingress.Spec.Backend.ServiceName,
		Namespace:   ingress.ObjectMeta.Namespace,
	}

	// Remove RevNAT from the BPF Map for non-LB nodes.
	if !d.conf.IsLBEnabled() {
		port := ingress.Spec.Backend.ServicePort.IntValue()
		for _, loadbalancer := range ingress.Status.LoadBalancer.Ingress {
			ingressIP := net.ParseIP(loadbalancer.IP)
			if ingressIP == nil {
				continue
			}
			feAddr, err := types.NewL3n4Addr(types.TCP, ingressIP, uint16(port))
			if err != nil {
				scopedLog.WithError(err).Error("Error while creating a new L3n4Addr. Ignoring ingress...")
				continue
			}
			// This is the only way that we can get the service's ID
			// without accessing the KVStore.
			svc := d.svcGetBySHA256Sum(feAddr.SHA256Sum())
			if svc != nil {
				if err := d.RevNATDelete(svc.FE.ID); err != nil {
					scopedLog.WithError(err).WithFields(log.Fields{
						logfields.ServiceID: svc.FE.ID,
					}).Error("Error while removing RevNAT for ingress")
				}
			}
		}
		return
	}

	d.loadBalancer.K8sMU.Lock()
	defer d.loadBalancer.K8sMU.Unlock()

	ingressSvcInfo, ok := d.loadBalancer.K8sIngress[svcName]
	if !ok {
		return
	}

	// Get all active endpoints for the service specified in ingress
	k8sEP, ok := d.loadBalancer.K8sEndpoints[svcName]
	if !ok {
		return
	}

	err := d.delK8sSVCs(svcName, ingressSvcInfo, k8sEP)
	if err != nil {
		scopedLog.WithError(err).Error("Unable to delete K8s ingress")
		return
	}
	delete(d.loadBalancer.K8sIngress, svcName)
}

func (d *Daemon) syncExternalLB(newSN, modSN, delSN *types.K8sServiceNamespace) error {
	deleteSN := func(delSN types.K8sServiceNamespace) error {
		ingSvc, ok := d.loadBalancer.K8sIngress[delSN]
		if !ok {
			return nil
		}

		endpoint, ok := d.loadBalancer.K8sEndpoints[delSN]
		if !ok {
			return nil
		}

		if err := d.delK8sSVCs(delSN, ingSvc, endpoint); err != nil {
			return err
		}

		delete(d.loadBalancer.K8sServices, delSN)
		return nil
	}

	addSN := func(addSN types.K8sServiceNamespace) error {
		ingressSvcInfo, ok := d.loadBalancer.K8sIngress[addSN]
		if !ok {
			return nil
		}

		k8sEP, ok := d.loadBalancer.K8sEndpoints[addSN]
		if !ok {
			return nil
		}

		err := d.addK8sSVCs(addSN, ingressSvcInfo, k8sEP)
		if err != nil {
			return err
		}
		return nil
	}

	if delSN != nil {
		// Clean old services
		return deleteSN(*delSN)
	}
	if modSN != nil {
		// Re-add modified services
		return addSN(*modSN)
	}
	if newSN != nil {
		// Add new services
		return addSN(*newSN)
	}
	return nil
}

// Deprecated: use addCiliumNetworkPolicyV2
func (d *Daemon) addCiliumNetworkPolicyV1(ciliumV1Store cache.Store, obj interface{}) {
	rule, ok := obj.(*cilium_v1.CiliumNetworkPolicy)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).
			Warn("Ignoring invalid k8s CiliumNetworkPolicy addition")
		return
	}
	ruleCpy := rule.DeepCopy()

	scopedLog := log.WithFields(log.Fields{
		logfields.CiliumNetworkPolicyName: ruleCpy.ObjectMeta.Name,
		logfields.K8sAPIVersion:           ruleCpy.TypeMeta.APIVersion,
		logfields.K8sNamespace:            ruleCpy.ObjectMeta.Namespace,
	})

	scopedLog.Debug("Adding CiliumNetworkPolicy")

	rules, err := ruleCpy.Parse()
	if err == nil && len(rules) > 0 {
		err = k8s.PreprocessRules(rules, d.loadBalancer.K8sEndpoints, d.loadBalancer.K8sServices)
		if err == nil {
			_, err = d.PolicyAdd(rules, &AddOptions{Replace: true})
		}
	}

	var cnpns cilium_v1.CiliumNetworkPolicyNodeStatus
	if err != nil {
		cnpns = cilium_v1.CiliumNetworkPolicyNodeStatus{
			OK:          false,
			Error:       fmt.Sprintf("%s", err),
			LastUpdated: cilium_v1.NewTimestamp(),
		}
		scopedLog.WithError(err).Warn("Unable to add CiliumNetworkPolicy")
	} else {
		cnpns = cilium_v1.CiliumNetworkPolicyNodeStatus{
			OK:          true,
			LastUpdated: cilium_v1.NewTimestamp(),
		}
		scopedLog.Info("Imported CiliumNetworkPolicy")
	}

	go func() {
		k8s.UpdateCNPStatusV1(ciliumNPClient.CiliumV1(), ciliumV1Store,
			k8s.BackOffLoopTimeout, node.GetName(), ruleCpy, cnpns)
	}()
}

// Deprecated: use deleteCiliumNetworkPolicyV2
func (d *Daemon) deleteCiliumNetworkPolicyV1(obj interface{}) {
	rule, ok := obj.(*cilium_v1.CiliumNetworkPolicy)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).
			Warn("Ignoring invalid k8s CiliumNetworkPolicy deletion")
		return
	}
	ruleCpy := rule.DeepCopy()

	scopedLog := log.WithFields(log.Fields{
		logfields.CiliumNetworkPolicyName: ruleCpy.ObjectMeta.Name,
		logfields.K8sAPIVersion:           ruleCpy.TypeMeta.APIVersion,
		logfields.K8sNamespace:            ruleCpy.ObjectMeta.Namespace,
	})

	scopedLog.Debug("Deleting CiliumNetworkPolicy")

	rules, err := ruleCpy.Parse()
	if err == nil {
		if len(rules) > 0 {
			// On a CNP, the transformed rule is stored in the local repository
			// with a set of labels. On a CNP with multiple rules all rules are
			// stored in the local repository with the same set of labels.
			// Therefore the deletion on the local repository can be done with
			// the set of labels of the first rule.
			_, err = d.PolicyDelete(rules[0].Labels)
		}
	}
	if err == nil {
		scopedLog.Info("Deleted CiliumNetworkPolicy")
	} else {
		scopedLog.WithError(err).Warn("Unable to delete CiliumNetworkPolicy")
	}
}

// Deprecated: use updateCiliumNetworkPolicyV2
func (d *Daemon) updateCiliumNetworkPolicyV1(ciliumV1Store cache.Store,
	oldObj interface{}, newObj interface{}) {

	oldRule, ok := oldObj.(*cilium_v1.CiliumNetworkPolicy)
	if !ok {
		log.WithField(logfields.Object+".old", logfields.Repr(oldObj)).
			Warn("Ignoring invalid k8s CiliumNetworkPolicy modification")
		return
	}
	newRules, ok := newObj.(*cilium_v1.CiliumNetworkPolicy)
	if !ok {
		log.WithField(logfields.Object+".new", logfields.Repr(newObj)).
			Warn("Ignoring invalid k8s CiliumNetworkPolicy modification")
		return
	}

	oldRuleCpy := oldRule.DeepCopy()
	_, err := oldRuleCpy.Parse()
	if err != nil {
		log.WithError(err).WithField(logfields.Object, logfields.Repr(oldRuleCpy)).
			Warn("Error parsing old CiliumNetworkPolicy rule")
		return
	}

	newRuleCpy := newRules.DeepCopy()
	_, err = newRules.Parse()
	if err != nil {
		log.WithError(err).WithField(logfields.Object, logfields.Repr(newRuleCpy)).
			Warn("Error parsing new CiliumNetworkPolicy rule")
		return
	}

	// Ignore updates of the spec remains unchanged.
	if oldRuleCpy.SpecEquals(newRuleCpy) {
		return
	}

	log.WithFields(log.Fields{
		logfields.K8sAPIVersion:                    oldRuleCpy.TypeMeta.APIVersion,
		logfields.CiliumNetworkPolicyName + ".old": oldRuleCpy.ObjectMeta.Name,
		logfields.K8sNamespace + ".old":            oldRuleCpy.ObjectMeta.Namespace,
		logfields.CiliumNetworkPolicyName + ".new": newRuleCpy.ObjectMeta.Name,
		logfields.K8sNamespace + ".new":            newRuleCpy.ObjectMeta.Namespace,
	}).Debug("Modified CiliumNetworkPolicy")

	d.deleteCiliumNetworkPolicyV1(oldObj)
	d.addCiliumNetworkPolicyV1(ciliumV1Store, newObj)
}

func (d *Daemon) addCiliumNetworkPolicyV2(ciliumV2Store cache.Store, obj interface{}) {
	rule, ok := obj.(*cilium_v2.CiliumNetworkPolicy)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).
			Warn("Ignoring invalid k8s CiliumNetworkPolicy addition")
		return
	}
	ruleCpy := rule.DeepCopy()

	scopedLog := log.WithFields(log.Fields{
		logfields.CiliumNetworkPolicyName: ruleCpy.ObjectMeta.Name,
		logfields.K8sAPIVersion:           ruleCpy.TypeMeta.APIVersion,
		logfields.K8sNamespace:            ruleCpy.ObjectMeta.Namespace,
	})

	scopedLog.Debug("Adding CiliumNetworkPolicy")

	rules, err := ruleCpy.Parse()
	if err == nil && len(rules) > 0 {
		err = k8s.PreprocessRules(rules, d.loadBalancer.K8sEndpoints, d.loadBalancer.K8sServices)
		if err == nil {
			_, err = d.PolicyAdd(rules, &AddOptions{Replace: true})
		}
	}

	var cnpns cilium_v2.CiliumNetworkPolicyNodeStatus
	if err != nil {
		cnpns = cilium_v2.CiliumNetworkPolicyNodeStatus{
			OK:          false,
			Error:       fmt.Sprintf("%s", err),
			LastUpdated: cilium_v2.NewTimestamp(),
		}
		scopedLog.WithError(err).Warn("Unable to add CiliumNetworkPolicy")
	} else {
		cnpns = cilium_v2.CiliumNetworkPolicyNodeStatus{
			OK:          true,
			LastUpdated: cilium_v2.NewTimestamp(),
		}
		scopedLog.Info("Imported CiliumNetworkPolicy")
	}

	go func() {
		k8s.UpdateCNPStatusV2(ciliumNPClient.CiliumV2(), ciliumV2Store,
			k8s.BackOffLoopTimeout, node.GetName(), ruleCpy, cnpns)
	}()
}

func (d *Daemon) deleteCiliumNetworkPolicyV2(obj interface{}) {
	rule, ok := obj.(*cilium_v2.CiliumNetworkPolicy)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).
			Warn("Ignoring invalid k8s CiliumNetworkPolicy deletion")
		return
	}
	ruleCpy := rule.DeepCopy()

	scopedLog := log.WithFields(log.Fields{
		logfields.CiliumNetworkPolicyName: ruleCpy.ObjectMeta.Name,
		logfields.K8sAPIVersion:           ruleCpy.TypeMeta.APIVersion,
		logfields.K8sNamespace:            ruleCpy.ObjectMeta.Namespace,
	})

	scopedLog.Debug("Deleting CiliumNetworkPolicy")

	rules, err := ruleCpy.Parse()
	if err == nil {
		if len(rules) > 0 {
			// On a CNP, the transformed rule is stored in the local repository
			// with a set of labels. On a CNP with multiple rules all rules are
			// stored in the local repository with the same set of labels.
			// Therefore the deletion on the local repository can be done with
			// the set of labels of the first rule.
			_, err = d.PolicyDelete(rules[0].Labels)
		}
	}
	if err == nil {
		scopedLog.Info("Deleted CiliumNetworkPolicy")
	} else {
		scopedLog.WithError(err).Warn("Unable to delete CiliumNetworkPolicy")
	}
}

func (d *Daemon) updateCiliumNetworkPolicyV2(ciliumV2Store cache.Store,
	oldObj interface{}, newObj interface{}) {

	// We don't need to deepcopy the objects since they are being copied
	// on each d.deleteCiliumNetworkPolicyV2 and d.addCiliumNetworkPolicyV2 calls.
	oldRule, ok := oldObj.(*cilium_v2.CiliumNetworkPolicy)
	if !ok {
		log.WithField(logfields.Object+".old", logfields.Repr(oldObj)).
			Warn("Ignoring invalid k8s CiliumNetworkPolicy modification")
		return
	}
	newRules, ok := newObj.(*cilium_v2.CiliumNetworkPolicy)
	if !ok {
		log.WithField(logfields.Object+".new", logfields.Repr(newObj)).
			Warn("Ignoring invalid k8s CiliumNetworkPolicy modification")
		return
	}

	oldRuleCpy := oldRule.DeepCopy()
	_, err := oldRuleCpy.Parse()
	if err != nil {
		log.WithError(err).WithField(logfields.Object, logfields.Repr(oldRuleCpy)).
			Warn("Error parsing old CiliumNetworkPolicy rule")
		return
	}

	newRuleCpy := newRules.DeepCopy()
	_, err = newRules.Parse()
	if err != nil {
		log.WithError(err).WithField(logfields.Object, logfields.Repr(newRuleCpy)).
			Warn("Error parsing new CiliumNetworkPolicy rule")
		return
	}

	// Ignore updates of the spec remains unchanged.
	if oldRuleCpy.SpecEquals(newRuleCpy) {
		return
	}

	log.WithFields(log.Fields{
		logfields.K8sAPIVersion:                    oldRuleCpy.TypeMeta.APIVersion,
		logfields.CiliumNetworkPolicyName + ".old": oldRuleCpy.ObjectMeta.Name,
		logfields.K8sNamespace + ".old":            oldRuleCpy.ObjectMeta.Namespace,
		logfields.CiliumNetworkPolicyName + ".new": newRuleCpy.ObjectMeta.Name,
		logfields.K8sNamespace + ".new":            newRuleCpy.ObjectMeta.Namespace,
	}).Debug("Modified CiliumNetworkPolicy")

	d.deleteCiliumNetworkPolicyV2(oldObj)
	d.addCiliumNetworkPolicyV2(ciliumV2Store, newObj)
}

func (d *Daemon) addK8sNodeV1(obj interface{}) {
	// We don't need to deepcopy the object since we are creating a node.Node
	// below.
	k8sNode, ok := obj.(*v1.Node)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).
			Warn("Invalid objected, expected v1.Node")
		return
	}
	ni := node.Identity{Name: k8sNode.ObjectMeta.Name}
	n := k8s.ParseNode(k8sNode)

	routeTypes := node.TunnelRoute

	// Add IPv6 routing only in non encap. With encap we do it with bpf tunnel
	// FIXME create a function to know on which mode is the daemon running on
	var ownAddr net.IP
	if autoIPv6NodeRoutes && d.conf.Device != "undefined" {
		// ignore own node
		if n.Name != node.GetName() {
			ownAddr = node.GetIPv6()
			routeTypes |= node.DirectRoute
		}
	}

	node.UpdateNode(ni, n, routeTypes, ownAddr)

	log.WithFields(log.Fields{
		logfields.K8sNodeID:     ni,
		logfields.K8sAPIVersion: k8sNode.TypeMeta.APIVersion,
		logfields.Node:          logfields.Repr(n),
	}).Debug("Added node")
}

func (d *Daemon) updateK8sNodeV1(_ interface{}, newObj interface{}) {
	// We don't need to deepcopy the object since we are transforming it to
	// a node.Node
	k8sNode, ok := newObj.(*v1.Node)
	if !ok {
		log.WithField(logfields.Object+".new", logfields.Repr(newObj)).
			Warn("Ignoring invalid k8s v1.Node modification")
		return
	}

	newNode := k8s.ParseNode(k8sNode)
	ni := node.Identity{Name: k8sNode.ObjectMeta.Name}

	oldNode := node.GetNode(ni)

	// If node is the same don't even change it on the map
	// TODO: Run the DeepEqual only for the metadata that we care about?
	if reflect.DeepEqual(oldNode, newNode) {
		return
	}

	routeTypes := node.TunnelRoute
	// Always re-add the routing tables as they might be accidentally removed
	var ownAddr net.IP
	if autoIPv6NodeRoutes && d.conf.Device != "undefined" {
		// ignore own node
		if newNode.Name != node.GetName() {
			ownAddr = node.GetIPv6()
			routeTypes |= node.DirectRoute
		}
	}

	node.UpdateNode(ni, newNode, routeTypes, ownAddr)

	log.WithFields(log.Fields{
		logfields.K8sNodeID:     ni,
		logfields.K8sAPIVersion: k8sNode.TypeMeta.APIVersion,
		logfields.Node:          logfields.Repr(newNode),
	}).Debug("Updated node")
}

func (d *Daemon) deleteK8sNodeV1(obj interface{}) {
	// We don't need to deepcopy the object since we are only reading the
	// node's name.
	k8sNode, ok := obj.(*v1.Node)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).
			Warn("Ignoring invalid k8s v1.Node deletion")
		return
	}

	ni := node.Identity{Name: k8sNode.ObjectMeta.Name}

	node.DeleteNode(ni, node.TunnelRoute|node.DirectRoute)

	log.WithFields(log.Fields{
		logfields.K8sNodeID:     ni,
		logfields.K8sAPIVersion: k8sNode.TypeMeta.APIVersion,
	}).Debug("Removed node")
}
