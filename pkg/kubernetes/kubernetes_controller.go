/*
 *  Copyright (C) 2018 Nalej Group - All Rights Reserved
 *
 *
 */

package kubernetes

import (
    "fmt"
    "github.com/nalej/deployment-manager/internal/structures/monitor"
    "github.com/nalej/deployment-manager/pkg/utils"
    "time"
    "k8s.io/apimachinery/pkg/runtime"
    utilruntime "k8s.io/apimachinery/pkg/util/runtime"
    "k8s.io/apimachinery/pkg/fields"
    "k8s.io/apimachinery/pkg/util/wait"
    "k8s.io/client-go/tools/cache"
    "k8s.io/client-go/util/workqueue"
    "k8s.io/api/extensions/v1beta1"
    "github.com/rs/zerolog/log"
    "k8s.io/api/core/v1"
    "github.com/nalej/deployment-manager/pkg/executor"
    "github.com/nalej/deployment-manager/internal/entities"
)

// The kubernetes controllers has a set of queues monitoring k8s related operations.
type KubernetesController struct {
    // Deployments controller
    deployments *KubernetesObserver
    // Services controller
    services *KubernetesObserver
    // Namespaces controller
    //namespaces *KubernetesObserver
    // Ingress observer
    ingresses *KubernetesObserver
    // Pending checks to run
    monitoredInstances monitor.MonitoredInstances
}


// Create a new kubernetes controller for a given namespace.
func NewKubernetesController(kExecutor *KubernetesExecutor, monitoredInstances monitor.MonitoredInstances,
    namespace string) executor.DeploymentController {

    // Watch Deployments
    deploymentsListWatcher := cache.NewListWatchFromClient(
        kExecutor.Client.ExtensionsV1beta1().RESTClient(),
        "Deployments", namespace, fields.Everything())
    // Create the observer with the corresponding helping functions.
    depObserver := NewKubernetesObserver(deploymentsListWatcher,
        func() runtime.Object{return &v1beta1.Deployment{}}, checkDeployments,
        monitoredInstances)


    // Watch Services
    servicesListWatcher := cache.NewListWatchFromClient(
        kExecutor.Client.CoreV1().RESTClient(),
        "Services", namespace, fields.Everything())
    // Create the observer with the corresponding helping functions.
    servObserver := NewKubernetesObserver(servicesListWatcher,
        func() runtime.Object{return &v1.Service{}}, checkServicesDeployed,
        monitoredInstances)


    // Watch Ingresses
    ingressesListsWatcher := cache.NewListWatchFromClient(
        kExecutor.Client.ExtensionsV1beta1().RESTClient(),
        "Ingresses", namespace, fields.Everything())
    // Create the observer with the corresponding helping functions.
    ingrObserver := NewKubernetesObserver(ingressesListsWatcher,
        func() runtime.Object{return &v1beta1.Ingress{}}, checkIngressDeployed,
        monitoredInstances)


    // Watch namespaces
    // TODO decide how to proceed with namespaces control
    /*
    namespacesListWatcher := cache.NewListWatchFromClient(
        kExecutor.Client.CoreV1().RESTClient(),
        "namespaces", v1.NamespaceAll, fields.Everything())
    // Create the observer with the corresponding helping functions.
    namespaceObserver := NewKubernetesObserver(namespacesListWatcher,
        func()runtime.Object{return &v1.Namespace{}}, checkNamespacesDeployed,
        monitoredInstances)
    */

    return &KubernetesController{
        deployments:        depObserver,
        services:           servObserver,
        ingresses:          ingrObserver,
        //namespaces:         namespaceObserver,
        monitoredInstances: monitoredInstances,
    }
}



// Add a resource to be monitored indicating its id on the target platform (uid) and the stage identifier.
func (c *KubernetesController) AddMonitoredResource(resource *entities.MonitoredPlatformResource) {
    c.monitoredInstances.AddPendingResource(resource)
}

// Set the status of a native resource
func (c *KubernetesController) SetResourceStatus(appInstanceID string, serviceID string, uid string,
    status entities.NalejServiceStatus, info string, endpoint string) {
    c.monitoredInstances.SetResourceStatus(appInstanceID, serviceID, uid, status, info, endpoint)
}

// Run this controller with its corresponding observers
func (c *KubernetesController) Run() {
    log.Debug().Msg("time to run K8s controller")
    // Run Services controller
    go c.services.Run(1)
    // Run Deployments controller
    go c.deployments.Run(1)
    // Run ingresses controller
    go c.ingresses.Run(1)
    // Run namespaces controller
    //go c.namespaces.Run(1)
}

func (c *KubernetesController) Stop() {
    log.Debug().Msg("time to stop K8s controller")
    /*
    defer close(c.deployments.stopCh)
    defer close(c.services.stopCh)
    //defer close(c.namespaces.stopCh)
    defer close(c.ingresses.stopCh)
    */
}


type KubernetesObserver struct {
    indexer  cache.Indexer
    queue    workqueue.RateLimitingInterface
    informer cache.Controller
    // function to determine how entities have to be checked to be deployed
    checkingFunc       func(interface{},monitor.MonitoredInstances)
    monitoredInstances monitor.MonitoredInstances
    // channel to control pod stop
    stopCh chan struct{}
}

// Build a new kubernetes observer for an available API resource.
//  params:
//   watcher containing the api resource name to be queried
//   targetFunc function to transform elements from the cache into a processable entity
//   checkingFunc function to indicate how the elements extracted from the queue have to be checked
//   checks list of pending stages
//  return:
//   a pointer to a kubernetes observer
func NewKubernetesObserver (watcher *cache.ListWatch, targetFunc func()runtime.Object,
    checkingFunc func(interface{},monitor.MonitoredInstances), checks monitor.MonitoredInstances) *KubernetesObserver {

    // create the workqueue
    queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
    // Bind the workqueue to a cache with the help of an informer. This way we make sure that
    // whenever the cache is updated, the pod key is added to the workqueue.
    // Note that when we finally process the item from the workqueue, we might see a newer version
    // of the Pod than the version which was responsible for triggering the update.

    indexer, informer := cache.NewIndexerInformer(watcher, targetFunc(), 0, cache.ResourceEventHandlerFuncs{
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

    return &KubernetesObserver{
        informer:           informer,
        indexer:            indexer,
        queue:              queue,
        checkingFunc:       checkingFunc,
        monitoredInstances: checks,
        stopCh:             make(chan struct{}),
    }
}


func (c *KubernetesObserver) processNextItem() bool {
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
func (c *KubernetesObserver) updatePendingChecks(key string) error {
    obj, exists, err := c.indexer.GetByKey(key)
    if err != nil {
        log.Error().Msgf("fetching object with key %s from store failed with %v", key, err)
        return err
    }

    if !exists {
        // Below we will warm up our cache with a Pod, so that we will see a delete for one pod
        log.Debug().Msgf("deployment %s does not exist anymore", key)
    } else {
        // Note that you also have to check the uid if you have a local controlled resource, which
        // is dependent on the actual instance, to detect that a Pod was recreated with the same name
        c.checkingFunc(obj,c.monitoredInstances)
    }
    return nil
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *KubernetesObserver) handleErr(err error, key interface{}) {
    if err == nil {
        // Forget about the #AddRateLimited history of the key on every successful synchronization.
        // This ensures that future processing of updates for this key is not delayed because of
        // an outdated error history.
        c.queue.Forget(key)
        return
    }

    // This controller retries 5 times if something goes wrong. After that, it stops trying.
    if c.queue.NumRequeues(key) < 5 {
        log.Error().Msgf("Error syncing pod %v: %v", key, err)

        // Re-enqueue the key rate limited. Based on the rate limiter on the
        // queue and the re-enqueue history, the key will be processed later again.
        c.queue.AddRateLimited(key)
        return
    }

    c.queue.Forget(key)
    // Report to an external entity that, even after several retries, we could not successfully process this key
    utilruntime.HandleError(err)
    log.Debug().Msgf("Dropping pod %q out of the queue: %v", key, err)
}

func (c *KubernetesObserver) Run(threadiness int) {
    defer utilruntime.HandleCrash()

    // Let the workers stop when we are done
    defer c.queue.ShutDown()
    //log.Debug().Msg("starting Pod controller")

    go c.informer.Run(c.stopCh)

    // Wait for all involved caches to be synced, before processing items from the queue is started
    if !cache.WaitForCacheSync(c.stopCh, c.informer.HasSynced) {
        utilruntime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
        return
    }

    for i := 0; i < threadiness; i++ {
        go wait.Until(c.runWorker, time.Second, c.stopCh)
    }

    <-c.stopCh
    //log.Debug().Msg("stopping Pod controller")
}

func (c *KubernetesObserver) runWorker() {
    for c.processNextItem() {
    }
}


// Helping function to check if a deployment is deployed or not. If so, it should
// update the pending checks by removing it from the list of tasks.
//  params:
//   stored object stored in the pipeline.
//   pending list of pending checks.
func checkDeployments(stored interface{}, pending monitor.MonitoredInstances){
    dep := stored.(*v1beta1.Deployment)
    log.Debug().Msgf("deployment %s status %v", dep.GetName(), dep.Status.String())
    // This deployment is monitored, and all its replicas are available
    // if pending.IsMonitoredResource(dep.Labels[utils.NALEJ_ANNOTATION_INSTANCE_ID],
    //    dep.Labels[utils.NALEJ_ANNOTATION_SERVICE_ID], string(dep.GetUID())){
        // if there are enough replicas, we assume this is working
        if (dep.Status.UnavailableReplicas == 0 && dep.Status.AvailableReplicas > 0){
            pending.SetResourceStatus(dep.Labels[utils.NALEJ_ANNOTATION_INSTANCE_ID],
                dep.Labels[utils.NALEJ_ANNOTATION_SERVICE_ID],string(dep.GetUID()),
                entities.NALEJ_SERVICE_RUNNING,"", "")
        } else {
            foundStatus := entities.KubernetesDeploymentStatusTranslation(dep.Status)
            // Generate an information string if possible
            info := ""
            if len(dep.Status.Conditions) != 0 {
                for _, condition := range dep.Status.Conditions {
                    info = fmt.Sprintf("%s %s",info,condition)
                }
            }
            log.Debug().Str(utils.NALEJ_ANNOTATION_INSTANCE_ID,dep.Labels[utils.NALEJ_ANNOTATION_INSTANCE_ID]).
                Str(utils.NALEJ_ANNOTATION_SERVICE_ID, dep.Labels[utils.NALEJ_ANNOTATION_SERVICE_ID]).
                Str("uid",string(dep.GetUID())).Interface("status", foundStatus).
                Msg("set deployment new status to ready")
            pending.SetResourceStatus(dep.Labels[utils.NALEJ_ANNOTATION_INSTANCE_ID],
                dep.Labels[utils.NALEJ_ANNOTATION_SERVICE_ID],string(dep.GetUID()), foundStatus, info, "")
        }
    /*
    } else {
        log.Warn().Str(utils.NALEJ_ANNOTATION_INSTANCE_ID,dep.Labels[utils.NALEJ_ANNOTATION_INSTANCE_ID]).
            Str(utils.NALEJ_ANNOTATION_SERVICE_ID, dep.Labels[utils.NALEJ_ANNOTATION_SERVICE_ID]).
            Str("uid",string(dep.GetUID())).Interface("status", entities.NALEJ_SERVICE_RUNNING).
            Msg("deployment is not monitored")
    }
    */
    return
}

// Helping function to check if a service is deployed or not. If so, it should
// update the pending checks by removing it from the list of tasks.
//  params:
//   stored object stored in the pipeline.
//   pending list of pending checks.
func checkServicesDeployed(stored interface{}, pending monitor.MonitoredInstances){
    // TODO determine what do we expect from a service to be deployed
    dep := stored.(*v1.Service)
    // This deployment is monitored.
    //if pending.IsMonitoredResource(dep.Labels[utils.NALEJ_ANNOTATION_INSTANCE_ID],
    //    dep.Labels[utils.NALEJ_ANNOTATION_SERVICE_ID],string(dep.GetUID())) {
        // K8S API does not offer any direct method to check if a service is already up an running
        // The ServiceStatus is almost empty and only contains pointer the load ingress values.
        // TODO check if there is a way to get more information for service status
        log.Debug().Str(utils.NALEJ_ANNOTATION_INSTANCE_ID,dep.Labels[utils.NALEJ_ANNOTATION_INSTANCE_ID]).
            Str(utils.NALEJ_ANNOTATION_SERVICE_ID, dep.Labels[utils.NALEJ_ANNOTATION_SERVICE_ID]).
            Str("uid",string(dep.GetUID())).Interface("status", entities.NALEJ_SERVICE_RUNNING).
            Msg("set service new status to ready")
        pending.SetResourceStatus(dep.Labels[utils.NALEJ_ANNOTATION_INSTANCE_ID],
            dep.Labels[utils.NALEJ_ANNOTATION_SERVICE_ID],string(dep.GetUID()), entities.NALEJ_SERVICE_RUNNING,"",
            "")
        /*
    } else {
        log.Warn().Str(utils.NALEJ_ANNOTATION_INSTANCE_ID,dep.Labels[utils.NALEJ_ANNOTATION_INSTANCE_ID]).
            Str(utils.NALEJ_ANNOTATION_SERVICE_ID, dep.Labels[utils.NALEJ_ANNOTATION_SERVICE_ID]).
            Str("uid",string(dep.GetUID())).Interface("status", entities.NALEJ_SERVICE_RUNNING).
            Msg("service is not monitored")
    }
        */
}

// Helping function to check if a namespace is deployed or not. If so, it should
// update the pending checks by removing it from the list of tasks.
//  params:
//   stored object stored in the pipeline.
//   pending list of pending checks.
func checkNamespacesDeployed(stored interface{}, pending monitor.MonitoredInstances){
    // TODO determine what do we expect from a namespace to be deployed
    dep := stored.(*v1.Namespace)

    // This namespace will only be correct if it is active
    //if pending.IsMonitoredResource(dep.Labels[utils.NALEJ_ANNOTATION_INSTANCE_ID],
    //    dep.Labels[utils.NALEJ_ANNOTATION_SERVICE_ID],string(dep.GetUID())){
        if dep.Status.Phase == v1.NamespaceActive {
            log.Debug().Str(utils.NALEJ_ANNOTATION_INSTANCE_ID,dep.Labels[utils.NALEJ_ANNOTATION_INSTANCE_ID]).
                Str(utils.NALEJ_ANNOTATION_SERVICE_ID, dep.Labels[utils.NALEJ_ANNOTATION_SERVICE_ID]).
                Str("uid",string(dep.GetUID())).Interface("status", entities.NALEJ_SERVICE_RUNNING).
                Msg("set namespace new status to ready")
            pending.SetResourceStatus(dep.Labels[utils.NALEJ_ANNOTATION_INSTANCE_ID],
                dep.Labels[utils.NALEJ_ANNOTATION_SERVICE_ID],string(dep.GetUID()), entities.NALEJ_SERVICE_RUNNING,
                "", "")
        }
        /*
    } else {
        log.Warn().Str(utils.NALEJ_ANNOTATION_INSTANCE_ID,dep.Labels[utils.NALEJ_ANNOTATION_INSTANCE_ID]).
            Str(utils.NALEJ_ANNOTATION_SERVICE_ID, dep.Labels[utils.NALEJ_ANNOTATION_SERVICE_ID]).
            Str("uid",string(dep.GetUID())).Interface("status", entities.NALEJ_SERVICE_RUNNING).
            Msg("namespace is not monitored")
    }
        */
}

// Helping function to check if an ingress is deployed or not. If so, it should
// update the pending checks by removing it from the list of tasks.
//  params:
//   stored object stored in the pipeline.
//   pending list of pending checks.
// TODO link this checker with the corresponding objects.
func checkIngressDeployed(stored interface{}, pending monitor.MonitoredInstances){
    dep := stored.(*v1beta1.Ingress)
    // This namespace will only be correct if it is active
    // if pending.IsMonitoredResource(dep.Annotations[utils.NALEJ_ANNOTATION_INSTANCE_ID],
    //    dep.Annotations[utils.NALEJ_ANNOTATION_SERVICE_ID],string(dep.GetUID())){
        // It considers the ingress to be ready when all the entries have ip and hostname
        ready := true
        for _, ing := range dep.Status.LoadBalancer.Ingress {
            if ing.Hostname != "" && ing.IP != "" {
                ready = true
                break
            }
        }
        if ready {
            log.Debug().Str(utils.NALEJ_ANNOTATION_INSTANCE_ID,dep.Labels[utils.NALEJ_ANNOTATION_INSTANCE_ID]).
                Str(utils.NALEJ_ANNOTATION_SERVICE_ID, dep.Labels[utils.NALEJ_ANNOTATION_SERVICE_ID]).
                Str("uid",string(dep.GetUID())).Interface("status", entities.NALEJ_SERVICE_RUNNING).
                Msg("set ingress new status to ready")
            pending.SetResourceStatus(dep.Labels[utils.NALEJ_ANNOTATION_INSTANCE_ID],
                dep.Labels[utils.NALEJ_ANNOTATION_SERVICE_ID],string(dep.GetUID()), entities.NALEJ_SERVICE_RUNNING,
                "", dep.Labels[utils.NALEJ_ANNOTATION_INGRESS_ENDPOINT])
        }
        /*
    } else {
        log.Warn().Str(utils.NALEJ_ANNOTATION_INSTANCE_ID,dep.Labels[utils.NALEJ_ANNOTATION_INSTANCE_ID]).
            Str(utils.NALEJ_ANNOTATION_SERVICE_ID, dep.Labels[utils.NALEJ_ANNOTATION_SERVICE_ID]).
            Str("uid",string(dep.GetUID())).Interface("status", entities.NALEJ_SERVICE_RUNNING).
            Msg("ingress is not monitored")
    }
        */
}
