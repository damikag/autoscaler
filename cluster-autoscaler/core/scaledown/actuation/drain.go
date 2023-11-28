/*
Copyright 2022 The Kubernetes Authors.

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

package actuation

import (
	"context"
	"fmt"
	"sort"
	"time"

	apiv1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	kube_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/autoscaler/cluster-autoscaler/metrics"
	"k8s.io/klog/v2"
	kubelet_config "k8s.io/kubernetes/pkg/kubelet/apis/config"

	acontext "k8s.io/autoscaler/cluster-autoscaler/context"
	"k8s.io/autoscaler/cluster-autoscaler/core/scaledown/status"
	"k8s.io/autoscaler/cluster-autoscaler/utils/daemonset"
	"k8s.io/autoscaler/cluster-autoscaler/utils/errors"
	pod_util "k8s.io/autoscaler/cluster-autoscaler/utils/pod"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	// DefaultEvictionRetryTime is the time after CA retries failed pod eviction.
	DefaultEvictionRetryTime = 10 * time.Second
	// DefaultPodEvictionHeadroom is the extra time we wait to catch situations when the pod is ignoring SIGTERM and
	// is killed with SIGKILL after MaxGracefulTerminationTime/ShutdownGracePeriodSeconds
	DefaultPodEvictionHeadroom = 30 * time.Second
)

type evictionRegister interface {
	RegisterEviction(*apiv1.Pod)
}

type Evictor struct {
	EvictionRetryTime                time.Duration
	PodEvictionHeadroom              time.Duration
	evictionRegister                 evictionRegister
	shutdownGracePeriodByPodPriority []kubelet_config.ShutdownGracePeriodByPodPriority
	priorityEvictorEnabled           bool
}

// NewEvictor returns an instance of Evictor.
func NewEvictor(evictionRegister evictionRegister, shutdownGracePeriodByPodPriority []kubelet_config.ShutdownGracePeriodByPodPriority, priorityEvictorEnabled bool) Evictor {
	sort.Slice(shutdownGracePeriodByPodPriority, func(i, j int) bool {
		return shutdownGracePeriodByPodPriority[i].Priority < shutdownGracePeriodByPodPriority[j].Priority
	})

	return Evictor{
		EvictionRetryTime:                DefaultEvictionRetryTime,
		PodEvictionHeadroom:              DefaultPodEvictionHeadroom,
		evictionRegister:                 evictionRegister,
		shutdownGracePeriodByPodPriority: shutdownGracePeriodByPodPriority,
		priorityEvictorEnabled:           priorityEvictorEnabled,
	}
}

// DrainNode groups pods in the node in to priority groups and, evicts pods in the ascending order of priorities.
// If priority evictor is not enable, eviction of daemonSet pods is the best effort.
func (e Evictor) DrainNode(ctx *acontext.AutoscalingContext, node *apiv1.Node, pods, dsPods []*apiv1.Pod) (map[string]status.PodEvictionResult, error) {
	if e.priorityEvictorEnabled {
		return e.drainNodeWithPodsBasedOnPodPriority(ctx, node, append(pods, dsPods...), nil)
	}
	return e.drainNodeWithPodsBasedOnPodPriority(ctx, node, pods, dsPods)
}

// drainNodeWithPodsBasedOnPodPriority performs drain logic on the node based on pod priorities. Marks the node as unschedulable and
// later removes all pods, giving each pod group up to ShutdownGracePeriodSeconds to finish. The list of pods to evict has to be provided.
func (e Evictor) drainNodeWithPodsBasedOnPodPriority(ctx *acontext.AutoscalingContext, node *apiv1.Node, fullEvictionPods, bestEffortEvictionPods []*apiv1.Pod) (map[string]status.PodEvictionResult, error) {
	evictionResults := make(map[string]status.PodEvictionResult)

	groups := groupByPriority(e.shutdownGracePeriodByPodPriority, fullEvictionPods, bestEffortEvictionPods)
	for _, group := range groups {
		for _, pod := range group.FullEvictionPods {
			evictionResults[pod.Name] = status.PodEvictionResult{Pod: pod, TimedOut: true,
				Err: errors.NewAutoscalerError(errors.UnexpectedScaleDownStateError, "Eviction did not attempted for the pod %s because some of the previous evictions failed", pod.Name)}
		}
	}

	for _, group := range groups {
		// If there are no pods in a particular range,
		// then do not wait for pods in that priority range.
		if len(group.FullEvictionPods) == 0 && len(group.BestEffortEvictionPods) == 0 {
			continue
		}

		var err error
		evictionResults, err = e.initiateEviction(ctx, node, group.FullEvictionPods, group.BestEffortEvictionPods, evictionResults, group.ShutdownGracePeriodSeconds)
		if err != nil {
			return evictionResults, err
		}

		// Evictions created successfully, wait ShutdownGracePeriodSeconds + podEvictionHeadroom to see if fullEviction pods really disappeared.
		evictionResults, err = e.waitPodsToDisappear(ctx, node, group.FullEvictionPods, evictionResults, group.ShutdownGracePeriodSeconds)
		if err != nil {
			return evictionResults, err
		}
	}
	return evictionResults, nil
}

func (e Evictor) waitPodsToDisappear(ctx *acontext.AutoscalingContext, node *apiv1.Node, pods []*apiv1.Pod, evictionResults map[string]status.PodEvictionResult,
	maxTermination int64) (map[string]status.PodEvictionResult, error) {
	var allGone bool
	for start := time.Now(); time.Now().Sub(start) < time.Duration(maxTermination)*time.Second+e.PodEvictionHeadroom; time.Sleep(5 * time.Second) {
		allGone = true
		for _, pod := range pods {
			podReturned, err := ctx.ClientSet.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
			if err == nil && (podReturned == nil || podReturned.Spec.NodeName == node.Name) {
				klog.V(1).Infof("Not deleted yet %s/%s", pod.Namespace, pod.Name)
				allGone = false
				break
			}
			if err != nil && !kube_errors.IsNotFound(err) {
				klog.Errorf("Failed to check pod %s/%s: %v", pod.Namespace, pod.Name, err)
				allGone = false
				break
			}
		}
		if allGone {
			klog.V(1).Infof("All pods removed from %s", node.Name)
			// Let the deferred function know there is no need for cleanup
			return evictionResults, nil
		}
	}

	for _, pod := range pods {
		podReturned, err := ctx.ClientSet.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
		if err == nil && (podReturned == nil || podReturned.Spec.NodeName == node.Name) {
			evictionResults[pod.Name] = status.PodEvictionResult{Pod: pod, TimedOut: true, Err: nil}
		} else if err != nil && !kube_errors.IsNotFound(err) {
			evictionResults[pod.Name] = status.PodEvictionResult{Pod: pod, TimedOut: true, Err: err}
		} else {
			evictionResults[pod.Name] = status.PodEvictionResult{Pod: pod, TimedOut: false, Err: nil}
		}
	}

	return evictionResults, errors.NewAutoscalerError(errors.TransientError, "Failed to drain node %s/%s: pods remaining after timeout", node.Namespace, node.Name)
}

func (e Evictor) initiateEviction(ctx *acontext.AutoscalingContext, node *apiv1.Node, fullEvictionPods, bestEffortEvictionPods []*apiv1.Pod, evictionResults map[string]status.PodEvictionResult,
	maxTermination int64) (map[string]status.PodEvictionResult, error) {

	retryUntil := time.Now().Add(ctx.MaxPodEvictionTime)
	fullEvictionConfirmations := make(chan status.PodEvictionResult, len(fullEvictionPods))
	bestEffortEvictionConfirmations := make(chan status.PodEvictionResult, len(bestEffortEvictionPods))

	for _, pod := range fullEvictionPods {
		evictionResults[pod.Name] = status.PodEvictionResult{Pod: pod, TimedOut: true, Err: nil}
		go func(pod *apiv1.Pod) {
			fullEvictionConfirmations <- e.evictPod(ctx, pod, retryUntil, maxTermination, true)
		}(pod)
	}

	for _, pod := range bestEffortEvictionPods {
		go func(pod *apiv1.Pod) {
			bestEffortEvictionConfirmations <- e.evictPod(ctx, pod, retryUntil, maxTermination, false)
		}(pod)
	}

	for i := 0; i < len(fullEvictionPods)+len(bestEffortEvictionPods); i++ {
		select {
		case evictionResult := <-fullEvictionConfirmations:
			evictionResults[evictionResult.Pod.Name] = evictionResult
			if evictionResult.WasEvictionSuccessful() {
				metrics.RegisterEvictions(1, metrics.PodEvictionSucceed)
			} else {
				metrics.RegisterEvictions(1, metrics.PodEvictionFailed)
			}
		case <-bestEffortEvictionConfirmations:
		}
	}

	evictionErrs := make([]error, 0)
	for _, pod := range fullEvictionPods {
		result := evictionResults[pod.Name]
		if !result.WasEvictionSuccessful() {
			evictionErrs = append(evictionErrs, result.Err)
		}
	}
	if len(evictionErrs) != 0 {
		return evictionResults, errors.NewAutoscalerError(errors.ApiCallError, "Failed to drain node %s/%s, due to following errors: %v", node.Namespace, node.Name, evictionErrs)
	}
	return evictionResults, nil
}

func (e Evictor) evictPod(ctx *acontext.AutoscalingContext, podToEvict *apiv1.Pod, retryUntil time.Time, maxTermination int64, fullEvictionPod bool) status.PodEvictionResult {
	ctx.Recorder.Eventf(podToEvict, apiv1.EventTypeNormal, "ScaleDown", "deleting pod for node scale down")

	termination := int64(apiv1.DefaultTerminationGracePeriodSeconds)
	if podToEvict.Spec.TerminationGracePeriodSeconds != nil {
		termination = *podToEvict.Spec.TerminationGracePeriodSeconds
	}
	if maxTermination > 0 && termination > maxTermination {
		termination = maxTermination
	}

	var lastError error
	for first := true; first || time.Now().Before(retryUntil); time.Sleep(e.EvictionRetryTime) {
		first = false
		eviction := &policyv1beta1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: podToEvict.Namespace,
				Name:      podToEvict.Name,
			},
			DeleteOptions: &metav1.DeleteOptions{
				GracePeriodSeconds: &termination,
			},
		}
		lastError = ctx.ClientSet.CoreV1().Pods(podToEvict.Namespace).Evict(context.TODO(), eviction)
		if lastError == nil || kube_errors.IsNotFound(lastError) {
			if e.evictionRegister != nil {
				e.evictionRegister.RegisterEviction(podToEvict)
			}
			return status.PodEvictionResult{Pod: podToEvict, TimedOut: false, Err: nil}
		}
	}
	if fullEvictionPod {
		klog.Errorf("Failed to evict pod %s, error: %v", podToEvict.Name, lastError)
		ctx.Recorder.Eventf(podToEvict, apiv1.EventTypeWarning, "ScaleDownFailed", "failed to delete pod for ScaleDown")
	}
	return status.PodEvictionResult{Pod: podToEvict, TimedOut: true, Err: fmt.Errorf("failed to evict pod %s/%s within allowed timeout (last error: %v)", podToEvict.Namespace, podToEvict.Name, lastError)}
}

func podsToEvict(ctx *acontext.AutoscalingContext, nodeInfo *framework.NodeInfo) (dsPods, nonDsPods []*apiv1.Pod) {
	for _, podInfo := range nodeInfo.Pods {
		if pod_util.IsMirrorPod(podInfo.Pod) {
			continue
		} else if pod_util.IsDaemonSetPod(podInfo.Pod) {
			dsPods = append(dsPods, podInfo.Pod)
		} else {
			nonDsPods = append(nonDsPods, podInfo.Pod)
		}
	}
	dsPodsToEvict := daemonset.PodsToEvict(dsPods, ctx.DaemonSetEvictionForOccupiedNodes)
	return dsPodsToEvict, nonDsPods
}

func groupByPriority(shutdownGracePeriodByPodPriority []kubelet_config.ShutdownGracePeriodByPodPriority, fullEvictionPods, bestEffortEvictionPods []*apiv1.Pod) []podShutdownGroup {
	groups := make([]podShutdownGroup, 0, len(shutdownGracePeriodByPodPriority))
	for _, period := range shutdownGracePeriodByPodPriority {
		groups = append(groups, podShutdownGroup{
			ShutdownGracePeriodByPodPriority: period,
		})
	}

	for _, pod := range fullEvictionPods {
		index := groupIndex(pod, &groups)
		groups[index].FullEvictionPods = append(groups[index].FullEvictionPods, pod)
	}
	for _, pod := range bestEffortEvictionPods {
		index := groupIndex(pod, &groups)
		groups[index].BestEffortEvictionPods = append(groups[index].BestEffortEvictionPods, pod)
	}
	return groups
}

func groupIndex(pod *apiv1.Pod, groups *[]podShutdownGroup) int {
	var priority int32
	if pod.Spec.Priority != nil {
		priority = *pod.Spec.Priority
	}

	// Find the group index according to the priority.
	index := sort.Search(len(*groups), func(i int) bool {
		return (*groups)[i].Priority >= priority
	})

	// 1. Those higher than the highest priority default to the highest priority
	// 2. Those lower than the lowest priority default to the lowest priority
	// 3. Those boundary priority default to the lower priority
	// if priority of pod is:
	//   groups[index-1].Priority <= pod priority < groups[index].Priority
	// in which case we want to pick lower one (i.e. index-1)
	if index == len(*groups) {
		index = len(*groups) - 1
	} else if index < 0 {
		index = 0
	} else if index > 0 && (*groups)[index].Priority > priority {
		index--
	}
	return index
}

type podShutdownGroup struct {
	kubelet_config.ShutdownGracePeriodByPodPriority
	FullEvictionPods       []*apiv1.Pod
	BestEffortEvictionPods []*apiv1.Pod
}
