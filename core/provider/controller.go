/*
Copyright 2017 Caicloud authors. All rights reserved.

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

package provider

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/caicloud/clientset/informers"
	lblisters "github.com/caicloud/clientset/listers/loadbalance/v1alpha2"
	lbapi "github.com/caicloud/clientset/pkg/apis/loadbalance/v1alpha2"
	"github.com/caicloud/clientset/util/syncqueue"
	log "github.com/zoumo/logdog"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
)

// GenericProvider holds the boilerplate code required to build an LoadBalancer Provider.
type GenericProvider struct {
	cfg *Configuration

	factory  informers.SharedInformerFactory
	lbLister lblisters.LoadBalancerLister
	queue    *syncqueue.SyncQueue

	// stopLock is used to enforce only a single call to Stop is active.
	// Needed because we allow stopping through an http endpoint and
	// allowing concurrent stoppers leads to stack traces.
	stopLock *sync.Mutex
	stopCh   chan struct{}
	shutdown bool
}

// NewLoadBalancerProvider returns a configured LoadBalancer controller
func NewLoadBalancerProvider(cfg *Configuration) *GenericProvider {

	gp := &GenericProvider{
		cfg:      cfg,
		factory:  informers.NewSharedInformerFactory(cfg.KubeClient, 0),
		stopLock: &sync.Mutex{},
		stopCh:   make(chan struct{}),
	}

	lbinformer := gp.factory.Loadbalance().V1alpha2().LoadBalancers()
	cminformer := gp.factory.Core().V1().ConfigMaps()
	lbinformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    gp.addLoadBalancer,
		UpdateFunc: gp.updateLoadBalancer,
		DeleteFunc: gp.deleteLoadBalancer,
	})
	cminformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: gp.updateConfigMap,
	})

	// sync nodes
	nodeinformer := gp.factory.Core().V1().Nodes()
	nodeinformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{})

	// sync secrets
	secretinformer := gp.factory.Core().V1().Secrets()
	secretinformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{})

	gp.cfg.Backend.SetListers(StoreLister{
		Node:         nodeinformer.Lister(),
		LoadBalancer: lbinformer.Lister(),
		ConfigMap:    cminformer.Lister(),
		Secret:       secretinformer.Lister(),
	})

	gp.queue = syncqueue.NewSyncQueue(&lbapi.LoadBalancer{}, gp.syncLoadBalancer)
	gp.lbLister = lbinformer.Lister()

	return gp
}

// Start starts the LoadBalancer Provider.
func (p *GenericProvider) Start() {
	defer utilruntime.HandleCrash()
	log.Info("Startting provider")

	p.factory.Start(p.stopCh)

	// wait cache synced
	log.Info("Wait for all caches synced")
	synced := p.factory.WaitForCacheSync(p.stopCh)
	for tpy, sync := range synced {
		if !sync {
			log.Error("Wait for cache sync timeout", log.Fields{"type": tpy})
			return
		}
	}
	log.Info("All caches have synced, Running LoadBalancer Controller ...")

	// start backend
	p.cfg.Backend.Start()
	if !p.cfg.Backend.WaitForStart() {
		log.Error("Wait for backend start timeout")
		return
	}

	// start worker
	p.queue.Run(1)

	<-p.stopCh

}

// Stop stops the LoadBalancer Provider.
func (p *GenericProvider) Stop() error {
	log.Info("Shutting down provider")
	p.stopLock.Lock()
	defer p.stopLock.Unlock()
	// Only try draining the workqueue if we haven't already.
	if !p.shutdown {
		p.shutdown = true
		log.Info("close channel")
		close(p.stopCh)
		// stop backend
		log.Info("stop backend")
		_ = p.cfg.Backend.Stop()
		// stop syncing
		log.Info("shutting down controller queue")
		p.queue.ShutDown()
		return nil
	}

	return fmt.Errorf("shutdown already in progress")
}

func (p *GenericProvider) addLoadBalancer(obj interface{}) {
	lb := obj.(*lbapi.LoadBalancer)
	if p.filterLoadBalancer(lb) {
		return
	}
	log.Info("Adding LoadBalancer ")
	p.queue.Enqueue(lb)
}

func (p *GenericProvider) updateLoadBalancer(oldObj, curObj interface{}) {
	old := oldObj.(*lbapi.LoadBalancer)
	cur := curObj.(*lbapi.LoadBalancer)

	if old.ResourceVersion == cur.ResourceVersion {
		// Periodic resync will send update events for all known LoadBalancer.
		// Two different versions of the same LoadBalancer will always have different RVs.
		return
	}

	if p.filterLoadBalancer(cur) {
		return
	}

	// ignore change of status
	if reflect.DeepEqual(old.Spec, cur.Spec) &&
		reflect.DeepEqual(old.Finalizers, cur.Finalizers) &&
		reflect.DeepEqual(old.DeletionTimestamp, cur.DeletionTimestamp) &&
		reflect.DeepEqual(old.Status.ProxyStatus.TCPConfigMap, cur.Status.ProxyStatus.TCPConfigMap) &&
		reflect.DeepEqual(old.Status.ProxyStatus.UDPConfigMap, cur.Status.ProxyStatus.UDPConfigMap) {
		return
	}

	log.Info("Updating LoadBalancer")

	p.queue.Enqueue(cur)

}

func (p *GenericProvider) deleteLoadBalancer(obj interface{}) {
	lb, ok := obj.(*lbapi.LoadBalancer)

	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("Couldn't get object from tombstone %#v", obj))
			return
		}
		lb, ok = tombstone.Obj.(*lbapi.LoadBalancer)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("Tombstone contained object that is not a LoadBalancer %#v", obj))
			return
		}
	}

	if p.filterLoadBalancer(lb) {
		return
	}

	log.Info("Deleting LoadBalancer")

	p.queue.Enqueue(lb)
}

func (p *GenericProvider) updateConfigMap(oldObj, curObj interface{}) {
	old := oldObj.(*v1.ConfigMap)
	cur := curObj.(*v1.ConfigMap)

	if old.ResourceVersion == cur.ResourceVersion {
		// Periodic resync will send update events for all known LoadBalancer.
		// Two different versions of the same LoadBalancer will always have different RVs.
		return
	}

	// namespace and name can not change, so we check one of them is enough
	if p.filterConfigMap(cur) {
		return
	}

	if reflect.DeepEqual(old.Data, cur.Data) {
		// nothing changed
		return
	}

	p.queue.Enqueue(cache.ExplicitKey(p.cfg.LoadBalancerNamespace + "/" + p.cfg.LoadBalancerName))

}

func (p *GenericProvider) filterLoadBalancer(lb *lbapi.LoadBalancer) bool {
	if lb.Namespace == p.cfg.LoadBalancerNamespace && lb.Name == p.cfg.LoadBalancerName {
		return false
	}

	return true
}

func (p *GenericProvider) filterConfigMap(cm *v1.ConfigMap) bool {
	if cm.Namespace == p.cfg.LoadBalancerNamespace && (cm.Name == p.cfg.TCPConfigMap || cm.Name == p.cfg.UDPConfigMap) {
		return false
	}
	return true
}

func (p *GenericProvider) syncLoadBalancer(obj interface{}) error {
	key := obj.(string)
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	lb, err := p.lbLister.LoadBalancers(namespace).Get(name)
	if errors.IsNotFound(err) {
		log.Warn("LoadBalancer has been deleted", log.Fields{"lb": key})
		// deleted
		// TODO shutdown?
		return nil
	}
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to retrieve LoadBalancer %v from store: %v", key, err))
		return err
	}

	if err := lbapi.ValidateLoadBalancer(lb); err != nil {
		log.Debug("invalid loadbalancer scheme", log.Fields{"err": err})
		return err
	}

	return p.cfg.Backend.OnUpdate(lb)
}
