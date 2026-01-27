/*
Copyright 2024 NVIDIA

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

package webhooks

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// NetworkInjector is a component that can inject Multus annotations and resources on Pods
type NetworkInjector struct {
	// Client is the client to the Kubernetes API server
	Client client.Reader
	// Settings are the settings for this component
	Settings NetworkInjectorSettings
}

// NetworkInjectorSettings are the settings for the Network Injector
type NetworkInjectorSettings struct {
	// NADName is the name of the network attachment definition that the injector should use to configure VFs for the
	// default network
	NADName string
	// NADNamespace is the namespace of the network attachment definition that the injector should use to configure VFs
	// for the default network
	NADNamespace string
	// DPUHostLabelKey is the label key that indicates a node has a DPU, runs OVNK in dpu-host mode and needs VF injection
	DPUHostLabelKey string
	// DPUHostLabelValue is the label value of DPUHostLabelKey
	DPUHostLabelValue string
}

const (
	// netAttachDefResourceNameAnnotation is the key of the network attachment definition annotation that indicates the
	// resource name.
	netAttachDefResourceNameAnnotation = "k8s.v1.cni.cncf.io/resourceName"
	// annotationKeyToBeInjected is the multus annotation we inject to the pods so that multus can inject the VFs
	annotationKeyToBeInjected = "v1.multus-cni.io/default-network"
)

var _ webhook.CustomDefaulter = &NetworkInjector{}

// +kubebuilder:webhook:path=/mutate--v1-pod,mutating=true,failurePolicy=fail,sideEffects=None,groups="",resources=pods,verbs=create,versions=v1,name=network-injector.dpu.nvidia.com,admissionReviewVersions=v1
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;list;watch;
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

func (webhook *NetworkInjector) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&corev1.Pod{}).
		WithDefaulter(webhook).
		Complete()
}

// Default implements webhook.Defaulter so a webhook will be registered for the type.
func (webhook *NetworkInjector) Default(ctx context.Context, obj runtime.Object) error {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return apierrors.NewBadRequest(fmt.Sprintf("expected a Pod but got a %T", obj))
	}

	// Use GenerateName if Name is not set yet (pod is being created by a controller)
	podName := pod.Name
	if podName == "" {
		podName = pod.GenerateName
	}

	// Update the logger in the context with pod information
	log := ctrl.LoggerFrom(ctx).WithValues("podName", podName, "podNamespace", pod.Namespace)
	ctx = ctrl.LoggerInto(ctx, log)

	// If the pod is on the host network no-op.
	if pod.Spec.HostNetwork {
		return nil
	}

	// Skip injection if the pod is explicitly scheduled to a node without the OVNK dpu-host label.
	isScheduledToNodesWithoutDPU, err := webhook.isScheduledToNodesWithoutDPU(ctx, pod)
	if err != nil {
		return err
	}

	if isScheduledToNodesWithoutDPU {
		return nil
	}

	vfResourceName, err := getVFResourceName(ctx, webhook.Client, webhook.Settings.NADName, webhook.Settings.NADNamespace)
	if err != nil {
		return fmt.Errorf("error while getting VF resource name: %w", err)
	}

	return injectNetworkResources(ctx, pod, webhook.Settings.NADName, webhook.Settings.NADNamespace, vfResourceName)
}

// getVFResourceName gets the resource name that relates to the VFs that should be injected.
func getVFResourceName(ctx context.Context, c client.Reader, netAttachDefName string, netAttachDefNamespace string) (corev1.ResourceName, error) {
	netAttachDef := &unstructured.Unstructured{}
	netAttachDef.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "k8s.cni.cncf.io",
		Version: "v1",
		Kind:    "NetworkAttachmentDefinition",
	})
	key := client.ObjectKey{Namespace: netAttachDefNamespace, Name: netAttachDefName}
	if err := c.Get(ctx, key, netAttachDef); err != nil {
		return "", fmt.Errorf("error while getting %s %s: %w", netAttachDef.GetObjectKind().GroupVersionKind().String(), key.String(), err)
	}

	if v, ok := netAttachDef.GetAnnotations()[netAttachDefResourceNameAnnotation]; ok {
		return corev1.ResourceName(v), nil
	}

	return "", fmt.Errorf("resource can't be found in network attachment definition because annotation %s doesn't exist", netAttachDefResourceNameAnnotation)
}

// isScheduledToNodesWithoutDPU checks if all nodes matching the pod's scheduling requirements lack the OVNK dpu-host label.
// Returns true if all matching nodes lack the label (skip VF injection), false otherwise (inject by default).
func (webhook *NetworkInjector) isScheduledToNodesWithoutDPU(ctx context.Context, pod *corev1.Pod) (bool, error) {
	// Get the required node affinity from the pod (combines nodeSelector and affinity)
	requiredNodeAffinity := nodeaffinity.GetRequiredNodeAffinity(pod)

	// List all nodes
	nodeList := &corev1.NodeList{}
	if err := webhook.Client.List(ctx, nodeList); err != nil {
		return false, fmt.Errorf("failed to list nodes: %w", err)
	}

	// Filter nodes that match the pod's scheduling requirements
	// TODO: Optimize for speed at scale with listing nodes that match selector instead of listing all nodes and filtering
	// them.
	var matchingNodes []corev1.Node
	for _, node := range nodeList.Items {
		matches, err := requiredNodeAffinity.Match(&node)
		if err != nil {
			return false, fmt.Errorf("failed to match node affinity: %w", err)
		}
		if matches {
			matchingNodes = append(matchingNodes, node)
		}
	}

	// If no nodes match, return false (inject by default - pod might not be schedulable or node might join later)
	// Notes in case nodeSelector is correct and nodes might join later:
	// * We expect cases where Pods targeting directly or indirectly only nodes without DPU to be stuck in Pending. User
	//   will need to recreate the Pods.
	if len(matchingNodes) == 0 {
		return false, nil
	}

	// Check if any matching node has the OVNK dpu-host label
	for _, node := range matchingNodes {
		if node.Labels != nil {
			if value, hasDPULabel := node.Labels[webhook.Settings.DPUHostLabelKey]; hasDPULabel && value == webhook.Settings.DPUHostLabelValue {
				// At least one matching node has the DPU label, return false (inject VFs)
				return false, nil
			}
		}
	}

	// All matching nodes lack the OVNK dpu-host label, return true (don't inject VFs)
	return true, nil
}

func injectNetworkResources(ctx context.Context, pod *corev1.Pod, netAttachDefName string, netAttachDefNamespace string, vfResourceName corev1.ResourceName) error {
	log := ctrl.LoggerFrom(ctx)
	// Inject device requests. One additional VF.
	if pod.Spec.Containers[0].Resources.Requests == nil {
		pod.Spec.Containers[0].Resources.Requests = corev1.ResourceList{}
	}
	if pod.Spec.Containers[0].Resources.Limits == nil {
		pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{}

	}
	if _, ok := pod.Spec.Containers[0].Resources.Requests[vfResourceName]; ok {
		res := pod.Spec.Containers[0].Resources.Requests[vfResourceName]
		res.Add(resource.MustParse("1"))
		pod.Spec.Containers[0].Resources.Requests[vfResourceName] = res
	} else {
		pod.Spec.Containers[0].Resources.Requests[vfResourceName] = resource.MustParse("1")
	}

	if _, ok := pod.Spec.Containers[0].Resources.Limits[vfResourceName]; ok {
		res := pod.Spec.Containers[0].Resources.Limits[vfResourceName]
		res.Add(resource.MustParse("1"))
		pod.Spec.Containers[0].Resources.Limits[vfResourceName] = res
	} else {
		pod.Spec.Containers[0].Resources.Limits[vfResourceName] = resource.MustParse("1")
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[annotationKeyToBeInjected] = fmt.Sprintf("%s/%s", netAttachDefNamespace, netAttachDefName)
	log.Info(fmt.Sprintf("injected resource %v into pod", vfResourceName))
	return nil
}
