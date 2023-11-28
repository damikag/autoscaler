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
	"strconv"
	"strings"

	"k8s.io/klog/v2"
	kubelet_config "k8s.io/kubernetes/pkg/kubelet/apis/config"
)

// ParseShutdownGracePeriodsAndPriorities parse priorityGracePeriodStr and returns an array of ShutdownGracePeriodByPodPriority if succeeded.
// Otherwise, returns an empty list
func ParseShutdownGracePeriodsAndPriorities(priorityGracePeriodStr string) []kubelet_config.ShutdownGracePeriodByPodPriority {
	var priorityGracePeriodMap, emptyMap []kubelet_config.ShutdownGracePeriodByPodPriority

	if priorityGracePeriodStr == "" {
		return emptyMap
	}
	priorityGracePeriodStrArr := strings.Split(priorityGracePeriodStr, ",")
	for _, item := range priorityGracePeriodStrArr {
		priorityAndPeriod := strings.Split(item, ":")
		if len(priorityAndPeriod) != 2 {
			klog.Errorf("Parsing shutdown grace periods failed because '%s' is not a priority and grace period couple seperated by ':'", item)
			return emptyMap
		}
		priority, err := strconv.Atoi(priorityAndPeriod[0])
		if err != nil {
			klog.Errorf("Parsing shutdown grace periods and priorities failed: %v", err)
			return emptyMap
		}
		shutDownGracePeriod, err := strconv.Atoi(priorityAndPeriod[1])
		if err != nil {
			klog.Errorf("Parsing shutdown grace periods and priorities failed: %v", err)
			return emptyMap
		}
		priorityGracePeriodMap = append(priorityGracePeriodMap, kubelet_config.ShutdownGracePeriodByPodPriority{
			Priority:                   int32(priority),
			ShutdownGracePeriodSeconds: int64(shutDownGracePeriod),
		})
	}
	return priorityGracePeriodMap
}
