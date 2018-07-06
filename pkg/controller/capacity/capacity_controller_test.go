package capacity

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubeinformers "k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	shipperv1 "github.com/bookingcom/shipper/pkg/apis/shipper/v1"
	shipperfake "github.com/bookingcom/shipper/pkg/client/clientset/versioned/fake"
	shipperinformers "github.com/bookingcom/shipper/pkg/client/informers/externalversions"
	"github.com/bookingcom/shipper/pkg/conditions"
	shippertesting "github.com/bookingcom/shipper/pkg/testing"
)

func init() {
	conditions.CapacityConditionsShouldDiscardTimestamps = true
}

func TestUpdatingCapacityTargetUpdatesDeployment(t *testing.T) {
	f := NewFixture(t)

	release := newRelease("0.0.1", "reviewsapi", 10)
	capacityTarget := newCapacityTargetForRelease(release, "capacity-v0.0.1", "reviewsapi", 50)
	f.managementObjects = append(f.managementObjects, release, capacityTarget)

	deployment := newDeploymentForRelease(release, "nginx", "reviewsapi", 0, 0)
	f.targetClusterObjects = append(f.targetClusterObjects, deployment)

	f.ExpectDeploymentPatchWithReplicas(deployment, 5)

	expectedClusterConditions := []shipperv1.ClusterCapacityCondition{
		{
			Type:   shipperv1.ClusterConditionTypeOperational,
			Status: corev1.ConditionTrue,
		},
		{
			Type:   shipperv1.ClusterConditionTypeReady,
			Status: corev1.ConditionTrue,
		},
	}

	f.expectCapacityTargetStatusUpdate(capacityTarget, 0, 0, expectedClusterConditions)

	f.runCapacityTargetSyncHandler()
}

func TestUpdatingDeploymentsUpdatesTheCapacityTargetStatus(t *testing.T) {
	f := NewFixture(t)

	release := newRelease("0.0.1", "reviewsapi", 10)
	capacityTarget := newCapacityTargetForRelease(release, "capacity-v0.0.1", "reviewsapi", 50)
	f.managementObjects = append(f.managementObjects, release, capacityTarget)

	deployment := newDeploymentForRelease(release, "nginx", "reviewsapi", 5, 5)
	f.targetClusterObjects = append(f.targetClusterObjects, deployment)

	clusterConditions := []shipperv1.ClusterCapacityCondition{
		{
			Type:    shipperv1.ClusterConditionTypeReady,
			Status:  corev1.ConditionFalse,
			Reason:  conditions.WrongPodCount,
			Message: "expected 5 replicas but have 0",
		},
	}
	f.expectCapacityTargetStatusUpdate(capacityTarget, 5, 50, clusterConditions)

	f.runCapacityTargetSyncHandler()
}

// TestSadPodsAreReflectedInCapacityTargetStatus tests a case where
// the deployment should have 5 available pods, but it has 4 happy
// pods and 1 sad pod.
func TestSadPodsAreReflectedInCapacityTargetStatus(t *testing.T) {
	f := NewFixture(t)

	release := newRelease("0.0.1", "reviewsapi", 2)
	capacityTarget := newCapacityTargetForRelease(release, "capacity-v0.0.1", "reviewsapi", 100)
	f.managementObjects = append(f.managementObjects, release, capacityTarget)

	deployment := newDeploymentForRelease(release, "nginx", "reviewsapi", 2, 1)
	happyPod := createHappyPodForDeployment(deployment)
	sadPod := createSadPodForDeployment(deployment)
	f.targetClusterObjects = append(f.targetClusterObjects, deployment, happyPod, sadPod)

	clusterConditions := []shipperv1.ClusterCapacityCondition{
		{
			Type:    shipperv1.ClusterConditionTypeReady,
			Status:  corev1.ConditionFalse,
			Reason:  conditions.PodsNotReady,
			Message: "there are 1 sad pods",
		},
	}
	f.expectCapacityTargetStatusUpdate(capacityTarget, 1, 50, clusterConditions, createSadPodConditionFromPod(sadPod))

	f.runCapacityTargetSyncHandler()
}

func NewFixture(t *testing.T) *fixture {
	return &fixture{
		t: t,
	}
}

type fixture struct {
	t *testing.T

	targetClusterClientset       *kubefake.Clientset
	targetClusterInformerFactory kubeinformers.SharedInformerFactory
	targetClusterObjects         []runtime.Object

	managementClientset       *shipperfake.Clientset
	managementInformerFactory shipperinformers.SharedInformerFactory
	managementObjects         []runtime.Object

	store *shippertesting.FakeClusterClientStore

	targetClusterActions     []kubetesting.Action
	managementClusterActions []kubetesting.Action
}

func (f *fixture) initializeFixture() {
	f.targetClusterClientset = kubefake.NewSimpleClientset(f.targetClusterObjects...)
	f.managementClientset = shipperfake.NewSimpleClientset(f.managementObjects...)

	const noResyncPeriod time.Duration = 0
	f.targetClusterInformerFactory = kubeinformers.NewSharedInformerFactory(f.targetClusterClientset, noResyncPeriod)
	f.managementInformerFactory = shipperinformers.NewSharedInformerFactory(f.managementClientset, noResyncPeriod)

	f.store = shippertesting.NewFakeClusterClientStore(f.targetClusterClientset, f.targetClusterInformerFactory, "minikube")
}

func (f *fixture) newController() *Controller {
	controller := NewController(
		f.managementClientset,
		f.managementInformerFactory,
		f.store,
		record.NewFakeRecorder(10),
	)

	return controller
}

func (f *fixture) runInternal() *Controller {
	f.initializeFixture()

	controller := f.newController()

	stopCh := make(chan struct{})
	defer close(stopCh)

	f.store.Run(stopCh)

	f.managementInformerFactory.Start(stopCh)
	f.targetClusterInformerFactory.Start(stopCh)

	f.managementInformerFactory.WaitForCacheSync(stopCh)
	f.targetClusterInformerFactory.WaitForCacheSync(stopCh)

	return controller
}

func (f *fixture) runCapacityTargetSyncHandler() {
	controller := f.runInternal()
	if controller.capacityTargetSyncHandler("reviewsapi/capacity-v0.0.1") {
		f.t.Error("sync handler unexpectedly returned 'retry'")
	}

	targetClusterActual := shippertesting.FilterActions(f.targetClusterClientset.Actions())
	managementClusterActual := shippertesting.FilterActions(f.managementClientset.Actions())

	shippertesting.CheckActions(f.targetClusterActions, targetClusterActual, f.t)
	shippertesting.CheckActions(f.managementClusterActions, managementClusterActual, f.t)
}

func (f *fixture) ExpectDeploymentPatchWithReplicas(deployment *appsv1.Deployment, replicas int32) {
	patchAction := kubetesting.NewPatchSubresourceAction(
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		deployment.GetNamespace(),
		deployment.GetName(),
		[]byte(fmt.Sprintf(`{"spec": {"replicas": %d}}`, replicas)),
	)
	f.targetClusterActions = append(f.targetClusterActions, patchAction)
}

func (f *fixture) expectCapacityTargetStatusUpdate(capacityTarget *shipperv1.CapacityTarget, availableReplicas, achievedPercent int32, clusterConditions []shipperv1.ClusterCapacityCondition, sadPods ...shipperv1.PodStatus) {
	clusterStatus := shipperv1.ClusterCapacityStatus{
		Name:              capacityTarget.Spec.Clusters[0].Name,
		AvailableReplicas: availableReplicas,
		AchievedPercent:   achievedPercent,
		Conditions:        clusterConditions,
		SadPods:           sadPods,
	}

	capacityTarget.Status.Clusters = append(capacityTarget.Status.Clusters, clusterStatus)

	updateAction := kubetesting.NewUpdateAction(
		schema.GroupVersionResource{Group: "shipper.booking.com", Version: "v1", Resource: "capacitytargets"},
		capacityTarget.GetNamespace(),
		capacityTarget,
	)

	f.managementClusterActions = append(f.managementClusterActions, updateAction)
}

func newCapacityTargetForRelease(release *shipperv1.Release, name, namespace string, percent int32) *shipperv1.CapacityTarget {
	minikube := shipperv1.ClusterCapacityTarget{
		Name:    "minikube",
		Percent: percent,
	}

	clusters := []shipperv1.ClusterCapacityTarget{minikube}

	metaLabels := map[string]string{
		shipperv1.ReleaseLabel: release.Name,
	}

	return &shipperv1.CapacityTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    metaLabels,
			OwnerReferences: []metav1.OwnerReference{
				metav1.OwnerReference{
					APIVersion: "shipper.booking.com/v1",
					Kind:       "Release",
					Name:       release.GetName(),
					UID:        release.GetUID(),
				},
			},
		},
		Spec: shipperv1.CapacityTargetSpec{
			Clusters: clusters,
		},
	}
}

func newRelease(name, namespace string, replicas int32) *shipperv1.Release {
	releaseMeta := shipperv1.ReleaseMeta{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				shipperv1.ReleaseReplicasAnnotation: strconv.Itoa(int(replicas)),
			},
		},
		Environment: shipperv1.ReleaseEnvironment{},
	}

	return &shipperv1.Release{
		ReleaseMeta: releaseMeta,
	}
}

func newDeploymentForRelease(release *shipperv1.Release, name, namespace string, replicas int32, availableReplicas int32) *appsv1.Deployment {
	status := appsv1.DeploymentStatus{
		AvailableReplicas: availableReplicas,
	}

	metaLabels := map[string]string{
		shipperv1.ReleaseLabel: release.Name,
	}

	specSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{
			shipperv1.ReleaseLabel: release.Name,
		},
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    metaLabels,
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: specSelector,
		},
		Status: status,
	}
}

func createSadPodForDeployment(deployment *appsv1.Deployment) *corev1.Pod {
	sadCondition := corev1.PodCondition{
		Type:    corev1.PodReady,
		Status:  corev1.ConditionFalse,
		Reason:  "ExpectedFail",
		Message: "This failure is meant to happen!",
	}

	status := corev1.PodStatus{
		Phase:      corev1.PodFailed,
		Conditions: []corev1.PodCondition{sadCondition},
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nginx-1a93Y2-sad",
			Namespace: "reviewsapi",
			Labels: map[string]string{
				shipperv1.ReleaseLabel: deployment.Labels[shipperv1.ReleaseLabel],
			},
		},
		Status: status,
	}
}

func createHappyPodForDeployment(deployment *appsv1.Deployment) *corev1.Pod {
	sadCondition := corev1.PodCondition{
		Type:   corev1.PodReady,
		Status: corev1.ConditionTrue,
	}

	status := corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{sadCondition},
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nginx-1a93Y2-happy",
			Namespace: "reviewsapi",
			Labels: map[string]string{
				shipperv1.ReleaseLabel: deployment.Labels[shipperv1.ReleaseLabel],
			},
		},
		Status: status,
	}
}

func createSadPodConditionFromPod(sadPod *corev1.Pod) shipperv1.PodStatus {
	return shipperv1.PodStatus{
		Name:      sadPod.Name,
		Condition: sadPod.Status.Conditions[0],
	}
}
