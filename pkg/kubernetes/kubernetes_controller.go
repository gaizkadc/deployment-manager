/*
 * Copyright 2018 Nalej
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package kubernetes

import (
    "fmt"
    "time"

    "github.com/golang/glog"

    "k8s.io/api/core/v1"
    //meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/fields"
    "k8s.io/apimachinery/pkg/util/runtime"
    "k8s.io/apimachinery/pkg/util/wait"
    "k8s.io/client-go/tools/cache"
    "k8s.io/client-go/util/workqueue"

    "k8s.io/api/extensions/v1beta1"
    "github.com/rs/zerolog/log"
)

type KubernetesController struct {
    indexer  cache.Indexer
    queue    workqueue.RateLimitingInterface
    informer cache.Controller
    pendingStages *PendingStages
}

func NewKubernetesController(executor *KubernetesExecutor) *KubernetesController {

    // create the pod watcher
    //podListWatcher := cache.NewListWatchFromClient(executor.client.CoreV1().RESTClient(), "pods", v1.NamespaceDefault, fields.Everything())


    deploymentsListWatcher := cache.NewListWatchFromClient(
        executor.client.ExtensionsV1beta1().RESTClient(),
        "deployments", v1.NamespaceAll, fields.Everything())


    // create the workqueue
    queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
    // Bind the workqueue to a cache with the help of an informer. This way we make sure that
    // whenever the cache is updated, the pod key is added to the workqueue.
    // Note that when we finally process the item from the workqueue, we might see a newer version
    // of the Pod than the version which was responsible for triggering the update.
    //indexer, informer := cache.NewIndexerInformer(podListWatcher, &v1.Pod{}, 0, cache.ResourceEventHandlerFuncs{
    //indexer, informer := cache.NewIndexerInformer(podListWatcher, &v1.Pod{}, 0, cache.ResourceEventHandlerFuncs{
    indexer, informer := cache.NewIndexerInformer(deploymentsListWatcher, &v1beta1.Deployment{}, 0, cache.ResourceEventHandlerFuncs{

        AddFunc: func(obj interface{}) {
            key, err := cache.MetaNamespaceKeyFunc(obj)
            if err == nil {
                queue.Add(key)
            }
        },
        UpdateFunc: func(old interface{}, new interface{}) {
            key, err := cache.MetaNamespaceKeyFunc(new)
            if err == nil {
                queue.Add(key)
            }
        },
        DeleteFunc: func(obj interface{}) {
            // IndexerInformer uses a delta queue, therefore for deletes we have to use this
            // key function.
            key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
            if err == nil {
                queue.Add(key)
            }
        },
    }, cache.Indexers{})

    return &KubernetesController{
        informer: informer,
        indexer:  indexer,
        queue:    queue,
        pendingStages: executor.pendingStages,
    }
}


func (c *KubernetesController) processNextItem() bool {
    // Wait until there is a new item in the working queue
    key, quit := c.queue.Get()
    if quit {
        return false
    }
    // Tell the queue that we are done with processing this key. This unblocks the key for other workers
    // This allows safe parallel processing because two pods with the same key are never processed in
    // parallel.
    defer c.queue.Done(key)

    // Invoke the method containing the business logic
    err := c.updatePendingChecks(key.(string))
    // Handle the error if something went wrong during the execution of the business logic
    c.handleErr(err, key)
    return true
}

// updatePendingChecks is the business logic of the controller. In this controller it simply prints
// information about the pod to stdout. In case an error happened, it has to simply return the error.
// The retry logic should not be part of the business logic.
func (c *KubernetesController) updatePendingChecks(key string) error {
    obj, exists, err := c.indexer.GetByKey(key)
    if err != nil {
        glog.Errorf("Fetching object with key %s from store failed with %v", key, err)
        return err
    }

    if !exists {
        // Below we will warm up our cache with a Pod, so that we will see a delete for one pod
        log.Debug().Msgf("deployment %s does not exist anymore", key)
    } else {
        // Note that you also have to check the uid if you have a local controlled resource, which
        // is dependent on the actual instance, to detect that a Pod was recreated with the same name

        // check if this is one of the pending monitored deployments
        dep := obj.(*v1beta1.Deployment)
        log.Debug().Msgf("deployment %s status %v", dep.GetName(), dep.Status.String() )

        // This deployment is monitored, and all its replicas are available
        if c.pendingStages.IsMonitoredResource(string(dep.GetUID())) && dep.Status.UnavailableReplicas == 0 {
            c.pendingStages.RemoveResource(string(dep.GetUID()))
        }
    }
    return nil
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *KubernetesController) handleErr(err error, key interface{}) {
    if err == nil {
        // Forget about the #AddRateLimited history of the key on every successful synchronization.
        // This ensures that future processing of updates for this key is not delayed because of
        // an outdated error history.
        c.queue.Forget(key)
        return
    }

    // This controller retries 5 times if something goes wrong. After that, it stops trying.
    if c.queue.NumRequeues(key) < 5 {
        glog.Infof("Error syncing pod %v: %v", key, err)

        // Re-enqueue the key rate limited. Based on the rate limiter on the
        // queue and the re-enqueue history, the key will be processed later again.
        c.queue.AddRateLimited(key)
        return
    }

    c.queue.Forget(key)
    // Report to an external entity that, even after several retries, we could not successfully process this key
    runtime.HandleError(err)
    glog.Infof("Dropping pod %q out of the queue: %v", key, err)
}

func (c *KubernetesController) Run(threadiness int, stopCh chan struct{}) {
    defer runtime.HandleCrash()

    // Let the workers stop when we are done
    defer c.queue.ShutDown()
    glog.Info("Starting Pod controller")

    go c.informer.Run(stopCh)

    // Wait for all involved caches to be synced, before processing items from the queue is started
    if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
        runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
        return
    }

    for i := 0; i < threadiness; i++ {
        go wait.Until(c.runWorker, time.Second, stopCh)
    }

    <-stopCh
    glog.Info("Stopping Pod controller")
}

func (c *KubernetesController) runWorker() {
    for c.processNextItem() {
    }
}


