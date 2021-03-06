/*
Copyright 2016 The Kubernetes Authors.

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

package ingress

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	federationapi "k8s.io/kubernetes/federation/apis/federation/v1beta1"
	fakefedclientset "k8s.io/kubernetes/federation/client/clientset_generated/federation_clientset/fake"
	"k8s.io/kubernetes/federation/pkg/federation-controller/util"
	"k8s.io/kubernetes/federation/pkg/federation-controller/util/deletionhelper"
	. "k8s.io/kubernetes/federation/pkg/federation-controller/util/test"
	"k8s.io/kubernetes/pkg/api/errors"
	apiv1 "k8s.io/kubernetes/pkg/api/v1"
	extensionsv1beta1 "k8s.io/kubernetes/pkg/apis/extensions/v1beta1"
	"k8s.io/kubernetes/pkg/client/cache"
	kubeclientset "k8s.io/kubernetes/pkg/client/clientset_generated/clientset"
	fakekubeclientset "k8s.io/kubernetes/pkg/client/clientset_generated/clientset/fake"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/types"
	"k8s.io/kubernetes/pkg/util/wait"

	"github.com/golang/glog"
	"github.com/stretchr/testify/assert"
)

const (
	maxTrials = 20
)

func TestIngressController(t *testing.T) {
	fakeClusterList := federationapi.ClusterList{Items: []federationapi.Cluster{}}
	fakeConfigMapList1 := apiv1.ConfigMapList{Items: []apiv1.ConfigMap{}}
	fakeConfigMapList2 := apiv1.ConfigMapList{Items: []apiv1.ConfigMap{}}
	cluster1 := NewCluster("cluster1", apiv1.ConditionTrue)
	cluster2 := NewCluster("cluster2", apiv1.ConditionTrue)
	cfg1 := NewConfigMap("foo")
	cfg2 := NewConfigMap("bar") // Different UID from cfg1, so that we can check that they get reconciled.

	t.Log("Creating fake infrastructure")
	fedClient := &fakefedclientset.Clientset{}
	RegisterFakeList("clusters", &fedClient.Fake, &fakeClusterList)
	RegisterFakeList("ingresses", &fedClient.Fake, &extensionsv1beta1.IngressList{Items: []extensionsv1beta1.Ingress{}})
	fedIngressWatch := RegisterFakeWatch("ingresses", &fedClient.Fake)
	clusterWatch := RegisterFakeWatch("clusters", &fedClient.Fake)
	fedClusterUpdateChan := RegisterFakeCopyOnUpdate("clusters", &fedClient.Fake, clusterWatch)
	fedIngressUpdateChan := RegisterFakeCopyOnUpdate("ingresses", &fedClient.Fake, fedIngressWatch)

	cluster1Client := &fakekubeclientset.Clientset{}
	RegisterFakeList("ingresses", &cluster1Client.Fake, &extensionsv1beta1.IngressList{Items: []extensionsv1beta1.Ingress{}})
	RegisterFakeList("configmaps", &cluster1Client.Fake, &fakeConfigMapList1)
	cluster1IngressWatch := RegisterFakeWatch("ingresses", &cluster1Client.Fake)
	cluster1ConfigMapWatch := RegisterFakeWatch("configmaps", &cluster1Client.Fake)
	cluster1IngressCreateChan := RegisterFakeCopyOnCreate("ingresses", &cluster1Client.Fake, cluster1IngressWatch)
	cluster1IngressUpdateChan := RegisterFakeCopyOnUpdate("ingresses", &cluster1Client.Fake, cluster1IngressWatch)

	cluster2Client := &fakekubeclientset.Clientset{}
	RegisterFakeList("ingresses", &cluster2Client.Fake, &extensionsv1beta1.IngressList{Items: []extensionsv1beta1.Ingress{}})
	RegisterFakeList("configmaps", &cluster2Client.Fake, &fakeConfigMapList2)
	cluster2IngressWatch := RegisterFakeWatch("ingresses", &cluster2Client.Fake)
	cluster2ConfigMapWatch := RegisterFakeWatch("configmaps", &cluster2Client.Fake)
	cluster2IngressCreateChan := RegisterFakeCopyOnCreate("ingresses", &cluster2Client.Fake, cluster2IngressWatch)
	cluster2ConfigMapUpdateChan := RegisterFakeCopyOnUpdate("configmaps", &cluster2Client.Fake, cluster2ConfigMapWatch)

	clientFactoryFunc := func(cluster *federationapi.Cluster) (kubeclientset.Interface, error) {
		switch cluster.Name {
		case cluster1.Name:
			return cluster1Client, nil
		case cluster2.Name:
			return cluster2Client, nil
		default:
			return nil, fmt.Errorf("Unknown cluster")
		}
	}
	ingressController := NewIngressController(fedClient)
	ingressInformer := ToFederatedInformerForTestOnly(ingressController.ingressFederatedInformer)
	ingressInformer.SetClientFactory(clientFactoryFunc)
	configMapInformer := ToFederatedInformerForTestOnly(ingressController.configMapFederatedInformer)
	configMapInformer.SetClientFactory(clientFactoryFunc)
	ingressController.clusterAvailableDelay = time.Second
	ingressController.ingressReviewDelay = 100 * time.Millisecond
	ingressController.configMapReviewDelay = 100 * time.Millisecond
	ingressController.smallDelay = 100 * time.Millisecond
	ingressController.updateTimeout = 5 * time.Second

	stop := make(chan struct{})
	t.Log("Running Ingress Controller")
	ingressController.Run(stop)

	// TODO: Here we are creating the ingress with first cluster annotation.
	// Add another test without that annotation when
	// https://github.com/kubernetes/kubernetes/issues/36540 is fixed.
	fedIngress := extensionsv1beta1.Ingress{
		ObjectMeta: apiv1.ObjectMeta{
			Name:      "test-ingress",
			Namespace: "mynamespace",
			SelfLink:  "/api/v1/namespaces/mynamespace/ingress/test-ingress",
			Annotations: map[string]string{
				firstClusterAnnotation: cluster1.Name,
			},
		},
		Status: extensionsv1beta1.IngressStatus{
			LoadBalancer: apiv1.LoadBalancerStatus{
				Ingress: make([]apiv1.LoadBalancerIngress, 0, 0),
			},
		},
	}

	t.Log("Adding cluster 1")
	clusterWatch.Add(cluster1)

	t.Log("Adding Ingress UID ConfigMap to cluster 1")
	cluster1ConfigMapWatch.Add(cfg1)

	// Test add federated ingress.
	t.Log("Adding Federated Ingress")
	fedIngressWatch.Add(&fedIngress)

	t.Log("Checking that UID annotation on Cluster 1 annotation was correctly updated after adding Federated Ingress")
	cluster := GetClusterFromChan(fedClusterUpdateChan)
	assert.NotNil(t, cluster)
	assert.Equal(t, cluster.ObjectMeta.Annotations[uidAnnotationKey], cfg1.Data[uidKey])

	t.Logf("Checking that approproate finalizers are added")
	// There should be 2 updates to add both the finalizers.
	updatedIngress := GetIngressFromChan(t, fedIngressUpdateChan)
	assert.True(t, ingressController.hasFinalizerFunc(updatedIngress, deletionhelper.FinalizerDeleteFromUnderlyingClusters))
	updatedIngress = GetIngressFromChan(t, fedIngressUpdateChan)
	assert.True(t, ingressController.hasFinalizerFunc(updatedIngress, apiv1.FinalizerOrphan), fmt.Sprintf("ingress does not have the orphan finalizer: %v", updatedIngress))
	fedIngress = *updatedIngress

	t.Log("Checking that Ingress was correctly created in cluster 1")
	createdIngress := GetIngressFromChan(t, cluster1IngressCreateChan)
	assert.NotNil(t, createdIngress)
	cluster1Ingress := *createdIngress
	assert.True(t, reflect.DeepEqual(fedIngress.Spec, cluster1Ingress.Spec), "Spec of created ingress is not equal")
	assert.True(t, util.ObjectMetaEquivalent(fedIngress.ObjectMeta, cluster1Ingress.ObjectMeta),
		"Metadata of created object is not equivalent")

	// Wait for finalizers to appear in federation store.
	assert.NoError(t, WaitForFinalizersInFederationStore(ingressController, ingressController.ingressInformerStore,
		types.NamespacedName{Namespace: fedIngress.Namespace, Name: fedIngress.Name}.String()), "finalizers not found in federated ingress")

	// Wait for the cluster ingress to appear in cluster store.
	assert.NoError(t, WaitForIngressInClusterStore(ingressController.ingressFederatedInformer.GetTargetStore(), cluster1.Name,
		types.NamespacedName{Namespace: createdIngress.Namespace, Name: createdIngress.Name}.String()),
		"Created ingress not found in underlying cluster store")

	// Test that IP address gets transferred from cluster ingress to federated ingress.
	t.Log("Checking that IP address gets transferred from cluster ingress to federated ingress")
	cluster1Ingress.Status.LoadBalancer.Ingress = append(cluster1Ingress.Status.LoadBalancer.Ingress,
		apiv1.LoadBalancerIngress{IP: "1.2.3.4"})
	glog.Infof("Setting artificial IP address for cluster1 ingress")

	for trial := 0; trial < maxTrials; trial++ {
		cluster1IngressWatch.Modify(&cluster1Ingress)
		// Wait for store to see the updated cluster ingress.
		key := types.NamespacedName{Namespace: createdIngress.Namespace, Name: createdIngress.Name}.String()
		if err := WaitForStatusUpdate(t, ingressController.ingressFederatedInformer.GetTargetStore(),
			cluster1.Name, key, cluster1Ingress.Status.LoadBalancer, time.Second); err != nil {
			continue
		}
		if err := WaitForFedStatusUpdate(t, ingressController.ingressInformerStore,
			key, cluster1Ingress.Status.LoadBalancer, time.Second); err != nil {
			continue
		}
	}

	for trial := 0; trial < maxTrials; trial++ {
		updatedIngress = GetIngressFromChan(t, fedIngressUpdateChan)
		assert.NotNil(t, updatedIngress, "Cluster's ingress load balancer status was not correctly transferred to the federated ingress")
		if updatedIngress == nil {
			return
		}
		if reflect.DeepEqual(cluster1Ingress.Status.LoadBalancer.Ingress, updatedIngress.Status.LoadBalancer.Ingress) {
			fedIngress.Status.LoadBalancer = updatedIngress.Status.LoadBalancer
			break
		} else {
			glog.Infof("Status check failed: expected: %v actual: %v", cluster1Ingress.Status, updatedIngress.Status)
		}
	}
	glog.Infof("Status check: expected: %v actual: %v", cluster1Ingress.Status, updatedIngress.Status)
	assert.True(t, reflect.DeepEqual(cluster1Ingress.Status.LoadBalancer.Ingress, updatedIngress.Status.LoadBalancer.Ingress),
		fmt.Sprintf("Ingress IP was not transferred from cluster ingress to federated ingress.  %v is not equal to %v",
			cluster1Ingress.Status.LoadBalancer.Ingress, updatedIngress.Status.LoadBalancer.Ingress))

	assert.NoError(t, WaitForStatusUpdate(t, ingressController.ingressFederatedInformer.GetTargetStore(),
		cluster1.Name, types.NamespacedName{Namespace: createdIngress.Namespace, Name: createdIngress.Name}.String(),
		cluster1Ingress.Status.LoadBalancer, time.Second))
	assert.NoError(t, WaitForFedStatusUpdate(t, ingressController.ingressInformerStore,
		types.NamespacedName{Namespace: createdIngress.Namespace, Name: createdIngress.Name}.String(),
		cluster1Ingress.Status.LoadBalancer, time.Second))
	t.Logf("expected: %v, actual: %v", createdIngress, updatedIngress)

	// Test update federated ingress.
	if fedIngress.ObjectMeta.Annotations == nil {
		fedIngress.ObjectMeta.Annotations = make(map[string]string)
	}
	fedIngress.ObjectMeta.Annotations["A"] = "B"
	t.Log("Modifying Federated Ingress")
	fedIngressWatch.Modify(&fedIngress)
	t.Log("Checking that Ingress was correctly updated in cluster 1")
	var updatedIngress2 *extensionsv1beta1.Ingress

	for trial := 0; trial < maxTrials; trial++ {
		updatedIngress2 = GetIngressFromChan(t, cluster1IngressUpdateChan)
		assert.NotNil(t, updatedIngress2)
		if updatedIngress2 == nil {
			return
		}
		if reflect.DeepEqual(fedIngress.Spec, updatedIngress.Spec) &&
			updatedIngress2.ObjectMeta.Annotations["A"] == fedIngress.ObjectMeta.Annotations["A"] {
			break
		}
	}

	assert.True(t, reflect.DeepEqual(updatedIngress2.Spec, fedIngress.Spec), "Spec of updated ingress is not equal")
	assert.Equal(t, updatedIngress2.ObjectMeta.Annotations["A"], fedIngress.ObjectMeta.Annotations["A"], "Updated annotation not transferred from federated to cluster ingress.")

	// Test add cluster
	t.Log("Adding a second cluster")

	fedIngress.Annotations[staticIPNameKeyWritable] = "foo" // Make sure that the base object has a static IP name first.
	fedIngressWatch.Modify(&fedIngress)
	clusterWatch.Add(cluster2)
	// First check that the original values are not equal - see above comment
	assert.NotEqual(t, cfg1.Data[uidKey], cfg2.Data[uidKey], fmt.Sprintf("ConfigMap in cluster 2 must initially not equal that in cluster 1 for this test - please fix test"))
	cluster2ConfigMapWatch.Add(cfg2)
	t.Log("Checking that the ingress got created in cluster 2")
	createdIngress2 := GetIngressFromChan(t, cluster2IngressCreateChan)
	assert.NotNil(t, createdIngress2)
	assert.True(t, reflect.DeepEqual(fedIngress.Spec, createdIngress2.Spec), "Spec of created ingress is not equal")
	t.Logf("created meta: %v fed meta: %v", createdIngress2.ObjectMeta, fedIngress.ObjectMeta)
	assert.True(t, util.ObjectMetaEquivalent(fedIngress.ObjectMeta, createdIngress2.ObjectMeta), "Metadata of created object is not equivalent")

	t.Log("Checking that the configmap in cluster 2 got updated.")
	updatedConfigMap2 := GetConfigMapFromChan(cluster2ConfigMapUpdateChan)
	assert.NotNil(t, updatedConfigMap2, fmt.Sprintf("ConfigMap in cluster 2 was not updated (or more likely the test is broken and the API type written is wrong)"))
	if updatedConfigMap2 != nil {
		assert.Equal(t, cfg1.Data[uidKey], updatedConfigMap2.Data[uidKey],
			fmt.Sprintf("UID's in configmaps in cluster's 1 and 2 are not equal (%q != %q)", cfg1.Data["uid"], updatedConfigMap2.Data["uid"]))
	}

	close(stop)
}

func GetIngressFromChan(t *testing.T, c chan runtime.Object) *extensionsv1beta1.Ingress {
	obj := GetObjectFromChan(c)
	ingress, ok := obj.(*extensionsv1beta1.Ingress)
	if !ok {
		t.Logf("Object on channel was not of type *extensionsv1beta1.Ingress: %v", obj)
	}
	return ingress
}

func GetConfigMapFromChan(c chan runtime.Object) *apiv1.ConfigMap {
	configMap, _ := GetObjectFromChan(c).(*apiv1.ConfigMap)
	return configMap
}

func GetClusterFromChan(c chan runtime.Object) *federationapi.Cluster {
	cluster, _ := GetObjectFromChan(c).(*federationapi.Cluster)
	return cluster
}

func NewConfigMap(uid string) *apiv1.ConfigMap {
	return &apiv1.ConfigMap{
		ObjectMeta: apiv1.ObjectMeta{
			Name:      uidConfigMapName,
			Namespace: uidConfigMapNamespace,
			SelfLink:  "/api/v1/namespaces/" + uidConfigMapNamespace + "/configmap/" + uidConfigMapName,
			// TODO: Remove: Annotations: map[string]string{},
		},
		Data: map[string]string{
			uidKey: uid,
		},
	}
}

// Wait for finalizers to appear in federation store.
func WaitForFinalizersInFederationStore(ingressController *IngressController, store cache.Store, key string) error {
	retryInterval := 100 * time.Millisecond
	timeout := wait.ForeverTestTimeout
	err := wait.PollImmediate(retryInterval, timeout, func() (bool, error) {
		obj, found, err := store.GetByKey(key)
		if !found || err != nil {
			return false, err
		}
		ingress := obj.(*extensionsv1beta1.Ingress)
		if ingressController.hasFinalizerFunc(ingress, apiv1.FinalizerOrphan) &&
			ingressController.hasFinalizerFunc(ingress, deletionhelper.FinalizerDeleteFromUnderlyingClusters) {
			return true, nil
		}
		return false, nil
	})
	return err
}

// Wait for the cluster ingress to appear in cluster store.
func WaitForIngressInClusterStore(store util.FederatedReadOnlyStore, clusterName, key string) error {
	retryInterval := 100 * time.Millisecond
	timeout := wait.ForeverTestTimeout
	err := wait.PollImmediate(retryInterval, timeout, func() (bool, error) {
		_, found, err := store.GetByKey(clusterName, key)
		if found && err == nil {
			return true, nil
		}
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	})
	return err
}

// Wait for ingress status to be updated to match the desiredStatus.
func WaitForStatusUpdate(t *testing.T, store util.FederatedReadOnlyStore, clusterName, key string, desiredStatus apiv1.LoadBalancerStatus, timeout time.Duration) error {
	retryInterval := 100 * time.Millisecond
	err := wait.PollImmediate(retryInterval, timeout, func() (bool, error) {
		obj, found, err := store.GetByKey(clusterName, key)
		if !found || err != nil {
			return false, err
		}
		ingress := obj.(*extensionsv1beta1.Ingress)
		return reflect.DeepEqual(ingress.Status.LoadBalancer, desiredStatus), nil
	})
	return err
}

// Wait for ingress status to be updated to match the desiredStatus.
func WaitForFedStatusUpdate(t *testing.T, store cache.Store, key string, desiredStatus apiv1.LoadBalancerStatus, timeout time.Duration) error {
	retryInterval := 100 * time.Millisecond
	err := wait.PollImmediate(retryInterval, timeout, func() (bool, error) {
		obj, found, err := store.GetByKey(key)
		if !found || err != nil {
			return false, err
		}
		ingress := obj.(*extensionsv1beta1.Ingress)
		return reflect.DeepEqual(ingress.Status.LoadBalancer, desiredStatus), nil
	})
	return err
}
