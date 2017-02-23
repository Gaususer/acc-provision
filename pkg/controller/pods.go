// Copyright 2016 Cisco Systems, Inc.
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

// Handlers for pod updates.  Pods map to opflex endpoints

package controller

import (
	"net/http"
	"reflect"

	"github.com/Sirupsen/logrus"

	kubeerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/pkg/api/v1"
	v1beta1 "k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/controller"

	"github.com/noironetworks/aci-containers/pkg/metadata"
)

func (cont *AciController) initPodInformerFromClient(
	kubeClient kubernetes.Interface) {

	cont.initPodInformerBase(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return kubeClient.CoreV1().Pods(metav1.NamespaceAll).List(options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return kubeClient.CoreV1().Pods(metav1.NamespaceAll).Watch(options)
			},
		})
}

func (cont *AciController) initPodInformerBase(listWatch *cache.ListWatch) {
	cont.podInformer = cache.NewSharedIndexInformer(
		listWatch,
		&v1.Pod{},
		controller.NoResyncPeriodFunc(),
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)
	cont.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			cont.podAdded(obj)
		},
		UpdateFunc: func(oldobj interface{}, newobj interface{}) {
			cont.podUpdated(oldobj, newobj)
		},
		DeleteFunc: func(obj interface{}) {
			cont.podDeleted(obj)
		},
	})

}

func podLogger(log *logrus.Logger, pod *v1.Pod) *logrus.Entry {
	return log.WithFields(logrus.Fields{
		"namespace": pod.ObjectMeta.Namespace,
		"name":      pod.ObjectMeta.Name,
		"node":      pod.Spec.NodeName,
	})
}

func podFilter(pod *v1.Pod) bool {
	if pod.Spec.HostNetwork {
		return false
	}
	return true
}

func (cont *AciController) processNextPodItem() bool {
	key, quit := cont.podQueue.Get()
	if quit {
		return false
	}

	podkey := key.(string)
	podobj, exists, err :=
		cont.podInformer.GetStore().GetByKey(podkey)
	if err == nil && exists {
		cont.handlePodUpdate(podobj.(*v1.Pod))
	}
	cont.podQueue.Forget(key)
	cont.podQueue.Done(key)

	return true
}

func (cont *AciController) queuePodUpdate(pod *v1.Pod) {
	podkey, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		podLogger(cont.log, pod).Error("Could not create pod key: ", err)
		return
	}
	cont.podQueue.Add(podkey)
}

func (cont *AciController) handlePodUpdate(pod *v1.Pod) {
	if !podFilter(pod) {
		return
	}
	logger := podLogger(cont.log, pod)

	podkey, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		logger.Error("Could not create pod key: ", err)
		return
	}

	cont.indexMutex.Lock()
	defer cont.indexMutex.Unlock()

	if pod.Spec.NodeName != "" {
		// note here we're assuming pods do not change nodes
		cont.addPodToNode(pod.Spec.NodeName, podkey)
	}

	// top-level default annotation
	egval := &cont.defaultEg
	sgval := &cont.defaultSg

	// namespace annotation has next-highest priority
	namespaceobj, exists, err :=
		cont.namespaceInformer.GetStore().GetByKey(pod.ObjectMeta.Namespace)
	if err != nil {
		cont.log.Error("Could not lookup namespace " +
			pod.ObjectMeta.Namespace + ": " + err.Error())
		return
	}
	if exists && namespaceobj != nil {
		namespace := namespaceobj.(*v1.Namespace)

		if og, ok := namespace.ObjectMeta.Annotations[metadata.EgAnnotation]; ok && og != "" {
			egval = &og
		}
		if og, ok := namespace.ObjectMeta.Annotations[metadata.SgAnnotation]; ok && og != "" {
			sgval = &og
		}
	}

	// annotation on associated deployment is next-highest priority
	for _, depkey := range cont.depPods.GetObjForPod(podkey) {
		deploymentobj, exists, err :=
			cont.deploymentInformer.GetStore().GetByKey(depkey)
		if err != nil {
			cont.log.Error("Could not lookup deployment " +
				depkey + ": " + err.Error())
			continue
		}
		if exists && deploymentobj != nil {
			deployment := deploymentobj.(*v1beta1.Deployment)

			if og, ok := deployment.ObjectMeta.Annotations[metadata.EgAnnotation]; ok && og != "" {
				egval = &og
			}
			if og, ok := deployment.ObjectMeta.Annotations[metadata.SgAnnotation]; ok && og != "" {
				sgval = &og
			}

			// multiple deployments matching the same pod is a broken
			// configuration.  We'll just use the first one.
			break
		}
	}

	// direct pod annotation is highest priority
	if og, ok := pod.ObjectMeta.Annotations[metadata.EgAnnotation]; ok && og != "" {
		egval = &og
	}
	if og, ok := pod.ObjectMeta.Annotations[metadata.SgAnnotation]; ok && og != "" {
		sgval = &og
	}

	podUpdated := false
	oldegval, ok := pod.ObjectMeta.Annotations[metadata.CompEgAnnotation]
	if egval != nil && *egval != "" {
		if !ok || oldegval != *egval {
			pod.ObjectMeta.Annotations[metadata.CompEgAnnotation] = *egval
			podUpdated = true
		}
	} else {
		if ok || oldegval == "" {
			delete(pod.ObjectMeta.Annotations, metadata.CompEgAnnotation)
			podUpdated = true
		}
	}
	oldsgval, ok := pod.ObjectMeta.Annotations[metadata.CompSgAnnotation]
	if sgval != nil && *sgval != "" {
		if !ok || oldsgval != *sgval {
			pod.ObjectMeta.Annotations[metadata.CompSgAnnotation] = *sgval
			podUpdated = true
		}
	} else {
		if ok || oldsgval == "" {
			delete(pod.ObjectMeta.Annotations, metadata.CompSgAnnotation)
			podUpdated = true
		}
	}

	if podUpdated {
		_, err := cont.updatePod(pod)
		if err != nil {
			if serr, ok := err.(*kubeerr.StatusError); ok {
				if serr.ErrStatus.Code == http.StatusConflict {
					logger.Debug("Conflict updating pod; ",
						"will retry on next update")
					return
				}
			}
			logger.Error("Failed to update pod: ", err)
		} else {
			logger.WithFields(logrus.Fields{
				"Eg": pod.ObjectMeta.Annotations[metadata.CompEgAnnotation],
				"Sg": pod.ObjectMeta.Annotations[metadata.CompSgAnnotation],
			}).Info("Updated pod annotations")
		}
	}
}

func (cont *AciController) podAdded(obj interface{}) {
	pod := obj.(*v1.Pod)
	if !cont.depPods.UpdatePod(pod) {
		cont.queuePodUpdate(pod)
	}
}

func (cont *AciController) podUpdated(oldobj interface{}, newobj interface{}) {
	oldpod := oldobj.(*v1.Pod)
	newpod := newobj.(*v1.Pod)

	if !reflect.DeepEqual(oldpod.ObjectMeta.Labels, newpod.ObjectMeta.Labels) {
		cont.depPods.UpdatePod(newpod)
	}
	if !reflect.DeepEqual(oldpod.ObjectMeta.Annotations,
		newpod.ObjectMeta.Annotations) {
		cont.queuePodUpdate(newpod)
	}
}

func (cont *AciController) podDeleted(obj interface{}) {
	pod := obj.(*v1.Pod)
	logger := podLogger(cont.log, pod)
	podkey, err := cache.MetaNamespaceKeyFunc(pod)
	if err != nil {
		logger.Error("Could not create pod key:" + err.Error())
		return
	}

	cont.depPods.DeletePod(pod)

	cont.indexMutex.Lock()
	cont.removePodFromNode(pod.Spec.NodeName, podkey)
	cont.indexMutex.Unlock()

	logger.Debug("Pod deleted")
}
