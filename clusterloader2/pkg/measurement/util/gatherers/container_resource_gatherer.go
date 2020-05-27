/*
Copyright 2018 The Kubernetes Authors.

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

package gatherers

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/util/system"
	"k8s.io/perf-tests/clusterloader2/pkg/measurement/util"
)

// NodesSet is a flag defining the node set range.
type NodesSet int

const (
	// AllNodes - all containers on all nodes
	AllNodes NodesSet = 0
	// MasterNodes - all containers on Master nodes only
	MasterNodes NodesSet = 1
	// MasterAndDNSNodes - all containers on Master nodes and DNS containers on other nodes
	MasterAndDNSNodes NodesSet = 2
	// MasterAndNonDaemons - all containers on Master nodes and non-daemons on other nodes.
	MasterAndNonDaemons NodesSet = 3
)

// ResourceUsageSummary represents summary of resource usage per container.
type ResourceUsageSummary map[string][]util.SingleContainerSummary

// Get returns collection of SingleContainerSummaries for given percentile.
func (r *ResourceUsageSummary) Get(perc string) []util.SingleContainerSummary {
	return (*r)[perc]
}

// ContainerResourceGatherer gathers resource metrics from containers.
type ContainerResourceGatherer struct {
	client       clientset.Interface
	isRunning    bool
	stopCh       chan struct{}
	workers      []resourceGatherWorker
	workerWg     sync.WaitGroup
	containerIDs []string
	options      ResourceGathererOptions
}

// ResourceGathererOptions specifies options for ContainerResourceGatherer.
type ResourceGathererOptions struct {
	InKubemark                  bool
	Nodes                       NodesSet
	ResourceDataGatheringPeriod time.Duration
	ProbeDuration               time.Duration
	PrintVerboseLogs            bool
}

func isDaemonPod(pod *corev1.Pod) bool {
	controller := metav1.GetControllerOf(pod)
	if controller == nil {
		// If controller is unset, assume it's not a daemon pod.
		return false
	}
	return controller.Kind == "DaemonSet" || controller.Kind == "Node"
}

// NewResourceUsageGatherer creates new instance of ContainerResourceGatherer
func NewResourceUsageGatherer(c clientset.Interface, host, provider string, options ResourceGathererOptions, pods *corev1.PodList) (*ContainerResourceGatherer, error) {
	g := ContainerResourceGatherer{
		client:       c,
		isRunning:    true,
		stopCh:       make(chan struct{}),
		containerIDs: make([]string, 0),
		options:      options,
	}

	if options.InKubemark {
		g.workerWg.Add(1)
		g.workers = append(g.workers, resourceGatherWorker{
			inKubemark:                  true,
			stopCh:                      g.stopCh,
			wg:                          &g.workerWg,
			finished:                    false,
			resourceDataGatheringPeriod: options.ResourceDataGatheringPeriod,
			probeDuration:               options.ProbeDuration,
			printVerboseLogs:            options.PrintVerboseLogs,
			host:                        host,
			provider:                    provider,
		})
	} else {
		// Tracks kube-system pods if no valid PodList is passed in.
		var err error
		if pods == nil {
			pods, err = c.CoreV1().Pods("kube-system").List(metav1.ListOptions{})
			if err != nil {
				return nil, fmt.Errorf("listing pods error: %v", err)
			}
		}
		dnsNodes := make(map[string]bool)
		for _, pod := range pods.Items {
			if (options.Nodes == MasterNodes) && !system.IsMasterNode(pod.Spec.NodeName) {
				continue
			}
			if (options.Nodes == MasterAndDNSNodes) && !system.IsMasterNode(pod.Spec.NodeName) && pod.Labels["k8s-app"] != "kube-dns" {
				continue
			}
			if (options.Nodes == MasterAndNonDaemons) && !system.IsMasterNode(pod.Spec.NodeName) && isDaemonPod(&pod) {
				continue
			}
			for _, container := range pod.Status.InitContainerStatuses {
				g.containerIDs = append(g.containerIDs, container.Name)
			}
			for _, container := range pod.Status.ContainerStatuses {
				g.containerIDs = append(g.containerIDs, container.Name)
			}
			if options.Nodes == MasterAndDNSNodes {
				dnsNodes[pod.Spec.NodeName] = true
			}
		}
		nodeList, err := c.CoreV1().Nodes().List(metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing nodes error: %v", err)
		}

		for _, node := range nodeList.Items {
			if options.Nodes == AllNodes || system.IsMasterNode(node.Name) || dnsNodes[node.Name] {
				g.workerWg.Add(1)
				g.workers = append(g.workers, resourceGatherWorker{
					c:                           c,
					nodeName:                    node.Name,
					wg:                          &g.workerWg,
					containerIDs:                g.containerIDs,
					stopCh:                      g.stopCh,
					finished:                    false,
					inKubemark:                  false,
					resourceDataGatheringPeriod: options.ResourceDataGatheringPeriod,
					probeDuration:               options.ProbeDuration,
					printVerboseLogs:            options.PrintVerboseLogs,
				})
				if options.Nodes == MasterNodes {
					break
				}
			}
		}
	}
	return &g, nil
}

// StartGatheringData starts a stat gathering worker blocks for each node to track,
// and blocks until StopAndSummarize is called.
func (g *ContainerResourceGatherer) StartGatheringData() {
	if len(g.workers) == 0 {
		return
	}
	delayPeriod := g.options.ResourceDataGatheringPeriod / time.Duration(len(g.workers))
	delay := time.Duration(0)
	for i := range g.workers {
		go g.workers[i].gather(delay)
		delay += delayPeriod
	}
	g.workerWg.Wait()
}

// StopAndSummarize stops stat gathering workers, processes the collected stats,
// generates resource summary for the passed-in percentiles, and returns the summary.
func (g *ContainerResourceGatherer) StopAndSummarize(percentiles []int) (*ResourceUsageSummary, error) {
	g.stop()
	klog.Infof("Closed stop channel. Waiting for %v workers", len(g.workers))
	finished := make(chan struct{})
	go func() {
		g.workerWg.Wait()
		finished <- struct{}{}
	}()
	select {
	case <-finished:
		klog.Infof("Waitgroup finished.")
	case <-time.After(2 * time.Minute):
		unfinished := make([]string, 0)
		for i := range g.workers {
			if !g.workers[i].finished {
				unfinished = append(unfinished, g.workers[i].nodeName)
			}
		}
		klog.Infof("Timed out while waiting for waitgroup, some workers failed to finish: %v", unfinished)
	}

	if len(percentiles) == 0 {
		klog.Infof("Warning! Empty percentile list for stopAndPrintData.")
		return &ResourceUsageSummary{}, fmt.Errorf("failed to get any resource usage data")
	}
	data := make(map[int]util.ResourceUsagePerContainer)
	for i := range g.workers {
		if g.workers[i].finished {
			stats := util.ComputePercentiles(g.workers[i].dataSeries, percentiles)
			data = util.LeftMergeData(stats, data)
		}
	}

	// Workers has been stopped. We need to gather data stored in them.
	sortedKeys := []string{}
	for name := range data[percentiles[0]] {
		sortedKeys = append(sortedKeys, name)
	}
	sort.Strings(sortedKeys)
	summary := make(ResourceUsageSummary)
	for _, perc := range percentiles {
		for _, name := range sortedKeys {
			usage := data[perc][name]
			summary[strconv.Itoa(perc)] = append(summary[strconv.Itoa(perc)], util.SingleContainerSummary{
				Name: name,
				Cpu:  usage.CPUUsageInCores,
				Mem:  usage.MemoryWorkingSetInBytes,
			})
		}
	}
	return &summary, nil
}

// Dispose disposes container resource gatherer.
func (g *ContainerResourceGatherer) Dispose() {
	g.stop()
}

func (g *ContainerResourceGatherer) stop() {
	if g.isRunning {
		g.isRunning = false
		close(g.stopCh)
	}
}
