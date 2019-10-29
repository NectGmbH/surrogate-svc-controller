package main

import (
    "fmt"
    "regexp"
    "time"

    "github.com/sirupsen/logrus"

    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/api/errors"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/labels"
    utilruntime "k8s.io/apimachinery/pkg/util/runtime"
    "k8s.io/apimachinery/pkg/util/wait"
    informers "k8s.io/client-go/informers/core/v1"
    clientset "k8s.io/client-go/kubernetes"
    listers "k8s.io/client-go/listers/core/v1"
    "k8s.io/client-go/tools/cache"
    "k8s.io/client-go/util/workqueue"
)

// OriginLabel represents the label on a surrogate service which defines the original svc
const OriginLabel = "surrogatesvc.nect.com/origin"

// OriginResourceVersionLabel represents the label the note down the version of the origin for which a surrogate got created. This version will be used to found out, if the surrogate needs to be updated.
const OriginResourceVersionLabel = "surrogatesvc.nect.com/origin-resource-version"

// LabelLabel represents the name of the label for which the surrogatesvc got created
const LabelLabel = "surrogatesvc.nect.com/label"

// ValueLabel represents the value of the label for which the surrogatesvc got created
const ValueLabel = "surrogatesvc.nect.com/value"

// FallbackForLabel represents the label for the fallback services which fallback to the original if the surrogate has no endpoints anymore
const FallbackForLabel = "surrogatesvc.nect.com/fallback-for"

// FallbackSuffix is the suffix added to fallback surrogate services
const FallbackSuffix = "-fb"

// Controller represents a controller used for handling svc updates
type Controller struct {
    kubeClient clientset.Interface

    svcLister  listers.ServiceLister
    svcsSynced cache.InformerSynced

    epLister  listers.EndpointsLister
    epsSynced cache.InformerSynced

    queue workqueue.RateLimitingInterface

    knownValueSuffixes map[string]string
    label              string
    labelEscaped       string
    tag                string
}

// NewController creates a new controller for handling svc updates
func NewController(
    client clientset.Interface,
    svcInformer informers.ServiceInformer,
    epInformer informers.EndpointsInformer,
    suffixes map[string]string,
    label string,
    tag string,
) *Controller {

    alphanumerical := regexp.MustCompile("[^a-zA-Z0-9]+")

    cc := &Controller{
        kubeClient:         client,
        queue:              workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "svcs"),
        knownValueSuffixes: suffixes,
        label:              label,
        labelEscaped:       alphanumerical.ReplaceAllString(label, "_"),
        tag:                tag,
    }

    // Manage the addition/update of services
    svcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
        AddFunc: func(obj interface{}) {
            svc := obj.(*corev1.Service)
            logrus.Debugf("Adding service %s", svc.Name)
            cc.enqueue(obj)
        },
        UpdateFunc: func(old, new interface{}) {
            oldSvc := old.(*corev1.Service)
            logrus.Debugf("Updating service %s", oldSvc.Name)
            cc.enqueue(new)
        },
        DeleteFunc: func(obj interface{}) {
            svc, ok := obj.(*corev1.Service)
            if !ok {
                tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
                if !ok {
                    logrus.Debugf("Couldn't get object from tombstone %#v", obj)
                    return
                }
                svc, ok = tombstone.Obj.(*corev1.Service)
                if !ok {
                    logrus.Debugf("Tombstone contained object that is not a service: %#v", obj)
                    return
                }
            }
            logrus.Debugf("Deleting service %s", svc.Name)
            cc.enqueue(obj)
        },
    })

    cc.svcLister = svcInformer.Lister()
    cc.svcsSynced = svcInformer.Informer().HasSynced

    // Manage the addition/update of endpoints
    epInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
        AddFunc: func(obj interface{}) {
            ep := obj.(*corev1.Endpoints)
            logrus.Debugf("Adding Endpoints %s", ep.Name)
            cc.enqueue(obj)
        },
        UpdateFunc: func(old, new interface{}) {
            oldEp := old.(*corev1.Endpoints)
            logrus.Debugf("Updating Endpoints %s", oldEp.Name)
            cc.enqueue(new)
        },
        DeleteFunc: func(obj interface{}) {
            ep, ok := obj.(*corev1.Endpoints)
            if !ok {
                tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
                if !ok {
                    logrus.Debugf("Couldn't get object from tombstone %#v", obj)
                    return
                }
                ep, ok = tombstone.Obj.(*corev1.Endpoints)
                if !ok {
                    logrus.Debugf("Tombstone contained object that is not a Endpoints: %#v", obj)
                    return
                }
            }
            logrus.Debugf("Deleting Endpoints %s", ep.Name)
            cc.enqueue(obj)
        },
    })

    cc.epLister = epInformer.Lister()
    cc.epsSynced = epInformer.Informer().HasSynced

    return cc
}

// Run the controller workers.
func (cc *Controller) Run(workers int, stopCh <-chan struct{}) {
    defer utilruntime.HandleCrash()
    defer cc.queue.ShutDown()

    logrus.Infof("Starting svc controller")
    defer logrus.Infof("Shutting down svc controller")

    if !cache.WaitForCacheSync(stopCh, cc.svcsSynced) {
        return
    }

    if !cache.WaitForCacheSync(stopCh, cc.epsSynced) {
        return
    }

    for i := 0; i < workers; i++ {
        go wait.Until(cc.runWorker, time.Second, stopCh)
    }

    <-stopCh
}

func (cc *Controller) runWorker() {
    for cc.processNextWorkItem() {
    }
}

func (cc *Controller) processNextWorkItem() bool {
    cKey, quit := cc.queue.Get()
    if quit {
        return false
    }

    defer cc.queue.Done(cKey)

    if err := cc.sync(cKey.(string)); err != nil {
        cc.queue.AddRateLimited(cKey)
        utilruntime.HandleError(fmt.Errorf("sync %v failed with : %v", cKey, err))

        return true
    }

    cc.queue.Forget(cKey)
    return true

}

func (cc *Controller) enqueue(obj interface{}) {
    key, err := cache.MetaNamespaceKeyFunc(obj)
    if err != nil {
        utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
        return
    }
    cc.queue.Add(key)
}

func (cc *Controller) sync(key string) error {
    startTime := time.Now()

    namespace, name, err := cache.SplitMetaNamespaceKey(key)
    if err != nil {
        utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
        return nil
    }

    defer func() {
        logrus.Debugf("finished syncing svc %q (%v)", key, time.Since(startTime))
    }()

    svc, err := cc.svcLister.Services(namespace).Get(name)
    if errors.IsNotFound(err) {
        logrus.Debugf("service has been deleted: %v", key)
        return nil
    }
    if err != nil {
        return err
    }

    // need to operate on a copy so we don't mutate the pvc in the shared cache
    svc = svc.DeepCopy()

    err = cc.handleService(svc)
    if err != nil {
        return fmt.Errorf("couldn't sync service `%s` in namespace `%s`, see: %v", name, namespace, err)
    }

    return nil
}

func (cc *Controller) handleService(svc *corev1.Service) error {
    if cc.tag != "" && !cc.isTaggedService(svc) {
        logrus.Debugf(
            "skipping service `%s` in namespace `%s` since it's not tagged with the `%s` label/annotation",
            svc.GetName(),
            svc.GetNamespace(),
            cc.tag)

        return nil
    }

    if cc.isSurrogateService(svc) {
        if !cc.responsibleForSurrogateService(svc) {
            logrus.Debugf(
                "skipping surrogate service `%s` in namespace `%s` since we aint responsible (e.g. wrong label)",
                svc.GetName(),
                svc.GetNamespace())

            return nil
        }

        origin, originExists, err := cc.originOfSurrogateExists(svc)
        if err != nil {
            return fmt.Errorf(
                "couldn't get origin of surrogate service `%s` in namespace `%s`, see: %v",
                svc.GetName(),
                svc.GetNamespace(),
                err)
        }

        if !originExists {
            err = cc.kubeClient.CoreV1().Services(svc.GetNamespace()).Delete(svc.GetName(), &metav1.DeleteOptions{})
            if err != nil {
                return fmt.Errorf(
                    "couldn't delete surrogate svc `%s` in namespace `%s` with non existing origin `%s`, see: %v",
                    svc.GetName(),
                    svc.GetNamespace(),
                    origin,
                    err)
            }

            logrus.Infof(
                "deleted surrogate service `%s` in namespace `%s` since origin `%s` does not exist anymore",
                svc.GetName(),
                svc.GetNamespace(),
                origin)

            return nil
        }

        originSvc, err := cc.svcLister.Services(svc.GetNamespace()).Get(origin)
        if err != nil {
            return fmt.Errorf(
                "couldn't retrieve origin service `%s` for surrogate `%s` in namespace `%s`, see: %v",
                origin,
                svc.GetName(),
                svc.GetNamespace(),
                err)
        }

        if originSvc.Spec.Type != corev1.ServiceTypeClusterIP {
            err = cc.kubeClient.CoreV1().Services(svc.GetNamespace()).Delete(svc.GetName(), &metav1.DeleteOptions{})
            if err != nil {
                return fmt.Errorf(
                    "couldn't delete surrogate svc `%s` in namespace `%s` with non existing origin `%s`, see: %v",
                    svc.GetName(),
                    svc.GetNamespace(),
                    origin,
                    err)
            }

            logrus.Warnf(
                "deleted surrogate service `%s` in namespace `%s` since origin `%s` switched to non-ClusterIP type",
                svc.GetName(),
                svc.GetNamespace(),
                origin)

            return nil
        }

        changedVersion, err := cc.syncChangesFromServiceToSurrogate(originSvc, svc)
        if err != nil {
            return fmt.Errorf(
                "couldn't sync changes from origin `%s` to surrogate `%s` in namespace `%s`, see: %v",
                origin,
                svc.GetName(),
                svc.GetNamespace(),
                err)
        }

        changedSelector, err := cc.ensureSurrogateServiceSelector(svc)
        if err != nil {
            return fmt.Errorf(
                "couldn't ensure surrogate service selector for service `%s` in namespace `%s`, see: %v",
                svc.GetName(),
                svc.GetNamespace(),
                err)
        }

        changed := changedVersion || changedSelector

        if changed {
            _, err = cc.kubeClient.CoreV1().Services(svc.GetNamespace()).Update(svc)
            if err != nil {
                return fmt.Errorf(
                    "couldn't create/update surrogate service `%s` in namespace `%s`, see: %v",
                    svc.GetName(),
                    svc.GetNamespace(),
                    err)
            }

            logrus.Infof(
                "Updated selector of surrogate service `%s` in namespace `%s`",
                svc.GetName(),
                svc.GetNamespace())
        }
    } else {
        if svc.DeletionTimestamp != nil || svc.Spec.Type != corev1.ServiceTypeClusterIP {
            err := cc.enqueueSurrogateServicesOfSvc(svc)
            if err != nil {
                return fmt.Errorf(
                    "couldn't enqueue surrogate services for deletion for service `%s` in namespace `%s` , see: %v",
                    svc.GetName(),
                    svc.GetNamespace(),
                    err)
            }

            return nil
        }

        err := cc.ensureSurrogateServicesForService(svc, false)
        if err != nil {
            return fmt.Errorf(
                "couldn't ensure surrogate services (non fallback) for service `%s` in namespace `%s`, see: %v",
                svc.GetName(),
                svc.GetNamespace(),
                err)
        }

        err = cc.ensureSurrogateServicesForService(svc, true)
        if err != nil {
            return fmt.Errorf(
                "couldn't ensure surrogate services (fallback) for service `%s` in namespace `%s`, see: %v",
                svc.GetName(),
                svc.GetNamespace(),
                err)
        }
    }

    return nil
}

func (cc *Controller) isTaggedService(svc *corev1.Service) bool {
    if cc.tag == "" {
        return true
    }

    labels := svc.GetLabels()
    if labels != nil {
        _, found := labels[cc.tag]
        if found {
            return true
        }
    }

    annotations := svc.GetAnnotations()
    if annotations != nil {
        _, found := annotations[cc.tag]
        if found {
            return true
        }
    }

    return false
}

func (cc *Controller) enqueueSurrogateServicesOfSvc(svc *corev1.Service) error {
    labelMap := make(map[string]string)
    labelMap[OriginLabel] = svc.GetName()

    selector := labels.SelectorFromSet(labels.Set(labelMap))
    surrogates, err := cc.svcLister.Services(svc.GetNamespace()).List(selector)
    if err != nil {
        return fmt.Errorf(
            "couldn't retrieve list of surrogate services for svc `%s` in namespace `%s`, see: %v",
            svc.GetName(),
            svc.GetNamespace(),
            err)
    }

    for _, surrogateSvc := range surrogates {
        if !cc.responsibleForSurrogateService(surrogateSvc) {
            continue
        }

        cc.enqueue(surrogateSvc)
    }

    return nil
}

func (cc *Controller) labelValueOfSurrogateService(svc *corev1.Service) string {
    labels := svc.GetLabels()
    if labels == nil {
        return ""
    }

    value, hasLabel := labels[ValueLabel]

    if !hasLabel {
        return ""
    }

    return value
}

func (cc *Controller) originOfSurrogateExists(svc *corev1.Service) (string, bool, error) {
    labels := svc.GetLabels()
    if labels == nil {
        labels = make(map[string]string)
    }

    origin, hasOriginLabel := labels[OriginLabel]
    if !hasOriginLabel {
        return "", false, fmt.Errorf(
            "surrogate service `%s` in namespace `%s` is missing `%s` label",
            svc.GetName(),
            svc.GetNamespace(),
            OriginLabel)
    }

    originSvc, err := cc.svcLister.Services(svc.GetNamespace()).Get(origin)
    if errors.IsNotFound(err) || (err == nil && originSvc.DeletionTimestamp != nil) {
        return origin, false, nil
    } else if err != nil {
        return origin, false, err
    }

    return origin, true, nil
}

func (cc *Controller) isSurrogateService(svc *corev1.Service) bool {
    labels := svc.GetLabels()
    if labels == nil {
        return false
    }

    _, hasOriginLabel := labels[OriginLabel]

    return hasOriginLabel
}

func (cc *Controller) isFallbackSurrogateService(svc *corev1.Service) bool {
    labels := svc.GetLabels()
    if labels == nil {
        return false
    }

    _, hasFallbackLabel := labels[FallbackForLabel]

    return hasFallbackLabel
}

func (cc *Controller) responsibleForSurrogateService(svc *corev1.Service) bool {
    labels := svc.GetLabels()
    if labels == nil {
        labels = make(map[string]string)
    }

    val, ok := labels[LabelLabel]
    if !ok {
        logrus.Warnf(
            "surrogateService `%s` in namespace `%s` does not contain the `%s` label, skipping it.",
            svc.GetName(),
            svc.GetNamespace(),
            LabelLabel)

        return false
    }

    return val == cc.labelEscaped
}

func (cc *Controller) ensureSurrogateServicesForService(svc *corev1.Service, isFallback bool) error {
    for labelValue := range cc.knownValueSuffixes {
        wantedSvcName := cc.surrogateSvcNameForSvcAndLabelValue(svc.GetName(), labelValue, isFallback)
        if wantedSvcName == "" {
            logrus.Debugf(
                "skipping creation of surrogate for svc `%s` with label value `%s` in namespace `%s` since suffix is empty",
                svc.GetName(),
                labelValue,
                svc.GetNamespace())

            continue
        }

        isNew := false
        isChanged := false
        wantedSvc, err := cc.svcLister.Services(svc.GetNamespace()).Get(wantedSvcName)
        if errors.IsNotFound(err) {
            wantedSvc, err = cc.surrogateServiceForLabelValue(labelValue, svc, isFallback)
            if err != nil {
                return fmt.Errorf(
                    "couldn't create surrogate service for service `%s` in namespace `%s`, see: %v",
                    svc.GetName(),
                    svc.GetNamespace(),
                    err)
            }

            isChanged = true
            isNew = true
        } else if err != nil {
            return fmt.Errorf(
                "couldn't retrieve surrogate svc `%s` in namespace `%s`, see: %v",
                wantedSvcName,
                svc.GetNamespace(),
                err)
        } else {
            changedVersion, err := cc.syncChangesFromServiceToSurrogate(svc, wantedSvc)
            if err != nil {
                return fmt.Errorf(
                    "couldn't sync changes from origin `%s` to surrogate `%s` in namespace `%s`, see: %v",
                    svc.GetName(),
                    wantedSvc.GetName(),
                    wantedSvc.GetNamespace(),
                    err)
            }

            changedSelector, err := cc.ensureSurrogateServiceSelector(wantedSvc)
            if err != nil {
                return fmt.Errorf(
                    "couldn't ensure surrogate service selector for service `%s` in namespace `%s`, see: %v",
                    wantedSvc.GetName(),
                    wantedSvc.GetNamespace(),
                    err)
            }

            isChanged = changedVersion || changedSelector
        }

        if !isChanged {
            logrus.Debugf(
                "skipping service `%s` in namespace `%s` for label value `%s` since its already up to date.",
                svc.GetName(),
                svc.GetNamespace(),
                labelValue)

            continue
        }

        if isNew {
            _, err = cc.kubeClient.CoreV1().Services(svc.GetNamespace()).Create(wantedSvc)
        } else {
            _, err = cc.kubeClient.CoreV1().Services(svc.GetNamespace()).Update(wantedSvc)
        }

        if err != nil {
            return fmt.Errorf(
                "couldn't create/update surrogate service `%s` in namespace `%s`, see: %v",
                wantedSvcName,
                svc.GetNamespace(),
                err)
        }
    }

    return nil
}

func (cc *Controller) surrogateSvcNameForSvcAndLabelValue(svcName string, labelValue string, isFallback bool) string {
    suffix, ok := cc.knownValueSuffixes[labelValue]
    if !ok {
        return ""
    }

    str := fmt.Sprintf("%s-%s", svcName, suffix)

    if isFallback {
        str = str + FallbackSuffix
    }

    return str
}

func (cc *Controller) syncChangesFromServiceToSurrogate(origin *corev1.Service, surrogate *corev1.Service) (bool, error) {
    surrogateLabels := surrogate.GetLabels()
    if surrogateLabels == nil {
        return false, fmt.Errorf("couldn't retrieve labels from surrogate service")
    }

    surrogateOriginResourceVersion, hasSurrogateOriginResourceVersion := surrogateLabels[OriginResourceVersionLabel]
    if !hasSurrogateOriginResourceVersion {
        return false, fmt.Errorf("surrogate service is missing the `%s` label", OriginResourceVersionLabel)
    }

    surrogateValueLabel, hasSurrogateValueLabel := surrogateLabels[ValueLabel]
    if !hasSurrogateValueLabel {
        return false, fmt.Errorf("surrogate service is missing the `%s` label", ValueLabel)
    }

    if origin.ObjectMeta.ResourceVersion == surrogateOriginResourceVersion {
        // Same resource version -> no need to sync
        return false, nil
    }

    originCopy := origin.DeepCopy()

    // Sync metadata
    originLabels := origin.GetLabels()
    if originLabels == nil {
        originLabels = make(map[string]string)
    }

    originLabels[OriginLabel] = origin.GetName()
    originLabels[OriginResourceVersionLabel] = origin.ObjectMeta.ResourceVersion
    originLabels[LabelLabel] = cc.labelEscaped
    originLabels[ValueLabel] = surrogateValueLabel

    fallbackLabel, hasFallbackLabel := surrogateLabels[FallbackForLabel]
    if hasFallbackLabel {
        originLabels[FallbackForLabel] = fallbackLabel
    }

    surrogate.SetLabels(originLabels)

    originAnnotations := origin.GetAnnotations()
    if originAnnotations == nil {
        originAnnotations = make(map[string]string)
    }

    surrogate.SetAnnotations(originAnnotations)

    // Sync spec
    surrogateClusterIP := surrogate.Spec.ClusterIP
    surrogate.Spec = originCopy.Spec
    surrogate.Spec.ClusterIP = surrogateClusterIP

    _, err := cc.ensureSurrogateServiceSelector(surrogate)
    if err != nil {
        return false, fmt.Errorf(
            "couldn't ensure surrogate service selector for service `%s` in namespace `%s`, see: %v",
            surrogate.GetName(),
            surrogate.GetNamespace(),
            err)
    }

    return true, nil
}

func (cc *Controller) surrogateServiceForLabelValue(labelValue string, svc *corev1.Service, isFallback bool) (*corev1.Service, error) {
    svcName := cc.surrogateSvcNameForSvcAndLabelValue(svc.GetName(), labelValue, isFallback)
    if svcName == "" {
        logrus.Debugf(
            "skipping creation of surrogate for svc `%s` with label value `%s` in namespace `%s` since suffix is empty",
            svc.GetName(),
            labelValue,
            svc.GetNamespace())

        return nil, nil
    }

    newSvc := svc.DeepCopy()
    newSvc.ObjectMeta.Name = svcName
    newSvc.ObjectMeta.CreationTimestamp = metav1.Time{}
    newSvc.ObjectMeta.ResourceVersion = ""
    newSvc.ObjectMeta.SelfLink = ""
    newSvc.ObjectMeta.UID = ""
    newSvc.ObjectMeta.Generation = 0

    labels := newSvc.GetLabels()
    if labels == nil {
        labels = make(map[string]string)
    }

    labels[OriginLabel] = svc.GetName()
    labels[OriginResourceVersionLabel] = svc.ObjectMeta.ResourceVersion
    labels[LabelLabel] = cc.labelEscaped
    labels[ValueLabel] = labelValue

    if isFallback {
        labels[FallbackForLabel] = cc.surrogateSvcNameForSvcAndLabelValue(svc.GetName(), labelValue, false)
    }

    newSvc.SetLabels(labels)

    newSvc.Status = corev1.ServiceStatus{}
    newSvc.Spec.ClusterIP = ""

    _, err := cc.ensureSurrogateServiceSelector(newSvc)
    if err != nil {
        return nil, fmt.Errorf(
            "couldn't ensure surrogate service selector for service `%s` in namespace `%s`, see: %v",
            newSvc.GetName(),
            newSvc.GetNamespace(),
            err)
    }

    return newSvc, nil
}

func (cc *Controller) shouldFallbackService(svc *corev1.Service) bool {
    if !cc.isFallbackSurrogateService(svc) {
        return false
    }

    labels := svc.GetLabels()
    if labels == nil {
        labels = make(map[string]string)
    }

    nonFallbackSvcName, _ := labels[FallbackForLabel]

    endpoints, err := cc.kubeClient.CoreV1().Endpoints(svc.GetNamespace()).Get(nonFallbackSvcName, metav1.GetOptions{})
    if err != nil {
        logrus.Warnf(
            "could not get endpoints for service `%s` in namespace `%s` therefore will fallback the service, see: %v",
            nonFallbackSvcName,
            svc.GetNamespace(),
            err)

        return true
    }

    for _, subset := range endpoints.Subsets {
        for range subset.Addresses {
            // As long as there are services in the surrogate we dont need to fallback
            return false
        }
    }

    return true
}

func (cc *Controller) ensureSurrogateServiceSelector(svc *corev1.Service) (bool, error) {
    labelValue := cc.labelValueOfSurrogateService(svc)
    if labelValue == "" {
        return false, fmt.Errorf(
            "surrogate service `%s` in namespace `%s` is missing the `%s` label",
            svc.GetName(),
            svc.GetNamespace(),
            ValueLabel)
    }

    if svc.Spec.Selector == nil {
        svc.Spec.Selector = make(map[string]string)
    }

    changed := false
    value, exists := svc.Spec.Selector[cc.label]
    if !exists || value != labelValue {
        changed = true
    }

    shouldFallback := cc.shouldFallbackService(svc)

    if shouldFallback {
        if exists {
            delete(svc.Spec.Selector, cc.label)
            return true, nil
        }

        return false, nil
    }

    svc.Spec.Selector[cc.label] = labelValue

    return changed, nil
}
