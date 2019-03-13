/*
 * Copyright 2019 Open Networking Foundation
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

package chaos

import (
	"bytes"
	"context"
	"docker.io/go-docker"
	dockertypes "docker.io/go-docker/api/types"
	"docker.io/go-docker/api/types/filters"
	"fmt"
	"github.com/atomix/chaos-controller/pkg/apis/chaos/v1alpha1"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"math/rand"
	"os/exec"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

type PartitionMonkey struct {
	context Context
	monkey  *v1alpha1.ChaosMonkey
}

func (m *PartitionMonkey) getPartitionName(local v1.Pod, remote v1.Pod) string {
	return fmt.Sprintf("%s-%s-%s", m.monkey.Name, local.Name, remote.Name)
}

func (m *PartitionMonkey) getPartitionNamespacedName(local v1.Pod, remote v1.Pod) types.NamespacedName {
	return types.NamespacedName{
		Namespace: m.monkey.Namespace,
		Name:      m.getPartitionName(local, remote),
	}
}

func (m *PartitionMonkey) create(pods []v1.Pod) error {
	switch m.monkey.Spec.Partition.PartitionStrategy.Type {
	case v1alpha1.PartitionIsolate:
		return m.createIsolate(pods)
	case v1alpha1.PartitionBridge:
		return m.createBridge(pods)
	case v1alpha1.PartitionHalves:
		return m.createHalves(pods)
	default:
		return nil
	}
}

func (m *PartitionMonkey) createIsolate(pods []v1.Pod) error {
	// Select a random pod to isolate.
	local := pods[rand.Intn(len(pods))]

	// Iterate through all pods and isolate the local pod from all remote pods.
	for _, remote := range pods {
		if remote.Name != local.Name {
			err := m.createPartition(local, remote)
			if err != nil {
				return err
			}

			err = m.createPartition(remote, local)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *PartitionMonkey) createBridge(pods []v1.Pod) error {
	// Choose a random pod to isolate.
	bridgeIdx := rand.Intn(len(pods))
	bridge := pods[bridgeIdx]

	log.Info("Bridging pod", "pod", bridge.Name, "namespace", bridge.Namespace)

	// Split the rest of the nodes into two halves.
	leftPods, rightPods := []v1.Pod{}, []v1.Pod{}
	for i, pod := range pods {
		if i != bridgeIdx {
			if i%2 == 0 {
				leftPods = append(leftPods, pod)
			} else {
				rightPods = append(rightPods, pod)
			}
		}
	}

	// Iterate through both the left and right partitions and partition the nodes from each other.
	for _, leftPod := range leftPods {
		for _, rightPod := range rightPods {
			// Create a partition on the left pod from the right pod.
			err := m.createPartition(leftPod, rightPod)
			if err != nil {
				return err
			}

			// Create a partition on the right pod from the left pod.
			err = m.createPartition(rightPod, leftPod)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *PartitionMonkey) createHalves(pods []v1.Pod) error {
	// Split the set of pods into two halves.
	leftPods, rightPods := []v1.Pod{}, []v1.Pod{}
	for i, pod := range pods {
		if i%2 == 0 {
			leftPods = append(leftPods, pod)
		} else {
			rightPods = append(rightPods, pod)
		}
	}

	log.Info("Partitioning cluster into halves")

	// Iterate through both the left and right partitions and partition the nodes from each other.
	for _, leftPod := range leftPods {
		for _, rightPod := range rightPods {
			// Create a partition on the left pod from the right pod.
			err := m.createPartition(leftPod, rightPod)
			if err != nil {
				return err
			}

			// Create a partition on the right pod from the left pod.
			err = m.createPartition(rightPod, leftPod)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *PartitionMonkey) createPartition(local v1.Pod, remote v1.Pod) error {
	// TODO: Set the parent of the network partition
	partition := &v1alpha1.NetworkPartition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.getPartitionName(local, remote),
			Namespace: m.monkey.Namespace,
			Labels:    getLabels(m.monkey),
		},
		Spec: v1alpha1.NetworkPartitionSpec{
			PodName:    local.Name,
			SourceName: remote.Name,
		},
		Status: v1alpha1.NetworkPartitionStatus{
			Phase: v1alpha1.PhaseStarted,
		},
	}
	if err := controllerutil.SetControllerReference(m.monkey, partition, m.context.scheme); err != nil {
		return err
	}
	return m.context.client.Create(context.TODO(), partition)
}

func (m *PartitionMonkey) delete(pods []v1.Pod) error {
	// Iterate through all pods and ensure partitions are deleted.
	for _, local := range pods {
		for _, remote := range pods {
			partition := &v1alpha1.NetworkPartition{}
			err := m.context.client.Get(context.TODO(), m.getPartitionNamespacedName(local, remote), partition)
			if err != nil {
				if !errors.IsNotFound(err) {
					return err
				}
				return nil
			}

			partition.Status.Phase = v1alpha1.PhaseStopped
			err = m.context.client.Status().Update(context.TODO(), partition)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// addPartitionController adds a Crash resource controller to the given controller
func addPartitionController(mgr manager.Manager) error {
	r := &ReconcileNetworkPartition{
		client: mgr.GetClient(),
		scheme: mgr.GetScheme(),
		config: mgr.GetConfig(),
	}

	c, err := controller.New("partition", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to Crash resource
	err = c.Watch(&source.Kind{Type: &v1alpha1.NetworkPartition{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}
	return err
}

var _ reconcile.Reconciler = &ReconcileNetworkPartition{}

// ReconcileNetworkPartition reconciles a Crash object
type ReconcileNetworkPartition struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
	config *rest.Config
}

// Reconcile reads that state of the cluster for a ChaosMonkey object and makes changes based on the state read
// and what is in the ChaosMonkey.Spec
func (r *ReconcileNetworkPartition) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling ChaosMonkey")

	// Fetch the NetworkPartition instance
	instance := &v1alpha1.NetworkPartition{}
	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// If the partition is still running, attempt to partition the pod.
	if instance.Status.Phase == v1alpha1.PhaseStarted {
		err = r.partition(instance)
	} else if instance.Status.Phase == v1alpha1.PhaseStopped {
		err = r.heal(instance)
	}
	return reconcile.Result{}, err
}

func (r *ReconcileNetworkPartition) setRunning(partition *v1alpha1.NetworkPartition) error {
	partition.Status.Phase = v1alpha1.PhaseRunning
	return r.client.Status().Update(context.TODO(), partition)
}

func (r *ReconcileNetworkPartition) setComplete(partition *v1alpha1.NetworkPartition) error {
	partition.Status.Phase = v1alpha1.PhaseComplete
	return r.client.Status().Update(context.TODO(), partition)
}

func (r *ReconcileNetworkPartition) partition(partition *v1alpha1.NetworkPartition) error {
	sourcePod := &v1.Pod{}
	if err := r.client.Get(context.TODO(), types.NamespacedName{partition.Namespace, partition.Spec.SourceName}, sourcePod); err != nil {
		return err
	}

	if sourcePod.Status.PodIP == "" {
		return nil
	}

	sourceIp := sourcePod.Status.PodIP

	ifaces, err := r.getInterfaces(partition)
	if err != nil {
		return err
	}

	for _, iface := range ifaces {
		err = exec.Command("bash", "-c", "iptables -A INPUT -i "+iface+" -s "+sourceIp+" -j DROP -w -m comment --comment \""+partition.Name+"\"").Run()
		if err != nil {
			return err
		}
	}
	return r.setRunning(partition)
}

func (r *ReconcileNetworkPartition) getInterfaces(stress *v1alpha1.NetworkPartition) ([]string, error) {
	cli, err := docker.NewEnvClient()
	if err != nil {
		return nil, err
	}

	containers, err := cli.ContainerList(context.Background(), dockertypes.ContainerListOptions{
		Filters: filters.NewArgs(filters.Arg("label=io.kubernetes.pod.name", stress.Spec.PodName)),
	})
	if err != nil {
		return nil, err
	}

	var ifaces []string
	for _, c := range containers {
		ifindex, err := r.exec("bash", "-c", "grep ^ /sys/class/net/vet*/ifindex | grep \":$(docker exec "+c.ID+" cat /sys/class/net/eth0/iflink)\" | cut -d \":\" -f 2")
		if err != nil {
			return nil, err
		}

		iface, err := r.exec("bash", "-c", "ip addr | grep \""+ifindex+":\" -f 2 | cut -d \"@\" -f 1 | tr -d '[:space:]'")
		if err != nil {
			return nil, err
		}
		ifaces = append(ifaces, iface)
	}
	return ifaces, nil
}

func (r *ReconcileNetworkPartition) heal(partition *v1alpha1.NetworkPartition) error {
	_, err := r.exec("bash", "-c", "iptables -D INPUT $(iptables -L INPUT --line-number | grep "+partition.Name+" | awk '{print $1}')")
	if err != nil {
		return err
	}
	return r.setComplete(partition)
}

func (r *ReconcileNetworkPartition) exec(command string, args ...string) (string, error) {
	stdout := bytes.Buffer{}
	cmd := exec.Command(command, args...)
	cmd.Stdout = &stdout
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return stdout.String(), nil
}
