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
	// PrioritizeOffloading when enabled, injects VFs when pod selectors match both nodes with and without the DPU label
	PrioritizeOffloading bool
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

	// Get VF resource name early to check if pod already has resources
	vfResourceName, err := getVFResourceName(ctx, webhook.Client, webhook.Settings.NADName, webhook.Settings.NADNamespace)
	if err != nil {
		return fmt.Errorf("error while getting VF resource name: %w", err)
	}

	// If pod already has VF resources, inject without checking affinity
	if podHasVFResources(pod, vfResourceName) {
		return injectNetworkResources(ctx, pod, webhook.Settings.NADName, webhook.Settings.NADNamespace, vfResourceName)
	}

	// Determine if injection should be skipped and if node affinity should be added for non-DPU workers
	skipInjection, shouldAddAffinityForNonDPUNodes, err := webhook.shouldSkipInjection(ctx, pod)
	if err != nil {
		return err
	}

	// Add node affinity for non-DPU nodes if needed
	if shouldAddAffinityForNonDPUNodes {
		addAffinityForNonDPUNodes(ctx, pod, webhook.Settings.DPUHostLabelKey, webhook.Settings.DPUHostLabelValue)
	}

	if skipInjection {
		return nil
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

// shouldSkipInjection determines if VF injection should be skipped based on the pod's scheduling requirements and matching nodes.
func (webhook *NetworkInjector) shouldSkipInjection(ctx context.Context, pod *corev1.Pod) (skipInjection bool, shouldAddAffinityForNonDPUNodes bool, error error) {
	// Get the required node affinity from the pod (combines nodeSelector and affinity)
	requiredNodeAffinity := nodeaffinity.GetRequiredNodeAffinity(pod)

	// List all nodes
	nodeList := &corev1.NodeList{}
	if err := webhook.Client.List(ctx, nodeList); err != nil {
		return false, false, fmt.Errorf("failed to list nodes: %w", err)
	}

	// Filter nodes that match the pod's scheduling requirements
	var matchingNodes []corev1.Node
	for _, node := range nodeList.Items {
		matches, err := requiredNodeAffinity.Match(&node)
		if err != nil {
			return false, false, fmt.Errorf("failed to match node affinity: %w", err)
		}
		if matches {
			matchingNodes = append(matchingNodes, node)
		}
	}

	// If no nodes match, return false (inject by default - pod might not be schedulable or node might join later)
	// Notes in case nodeSelector is correct and nodes might join later:
	// * We expect cases where Pods targeting directly or indirectly only nodes without DPU to be stuck in Pending. User
	//   will need to recreate the Pods.
	// * We don't take into account the PrioritizeOffloading setting here because in case we would, when set to false we
	//   might have ended up with Pods indirectly targeting upcoming DPU Nodes without a VF injected but with nodeAffinity
	//   to ignore such nodes set. This would be hard to debug.
	if len(matchingNodes) == 0 {
		return false, false, nil
	}

	// Count nodes with and without the DPU label
	nodesWithDPU := 0
	nodesWithoutDPU := 0
	for _, node := range matchingNodes {
		hasDPULabel := false
		if node.Labels != nil {
			if value, exists := node.Labels[webhook.Settings.DPUHostLabelKey]; exists && value == webhook.Settings.DPUHostLabelValue {
				hasDPULabel = true
			}
		}
		if hasDPULabel {
			nodesWithDPU++
		} else {
			nodesWithoutDPU++
		}
	}

	// This is the default mode where we prioritize scheduling on nodes with DPU in case there is ambiguity.
	if webhook.Settings.PrioritizeOffloading {
		// If at least one matching node has the DPU label, inject VFs
		if nodesWithDPU > 0 {
			return false, false, nil
		}
		// All matching nodes lack the DPU label, don't inject VFs
		return true, false, nil
	}

	// This is the mode where we prioritize scheduling on nodes without DPU in case there is ambiguity.
	// If some (but not all) matching nodes have the DPU label
	if nodesWithDPU > 0 && nodesWithoutDPU > 0 {
		// Request adding node affinity for non-DPU nodes to exclude DPU nodes, don't inject VFs
		return true, true, nil
	}

	// If all matching nodes have the DPU label, inject VFs
	if nodesWithDPU > 0 && nodesWithoutDPU == 0 {
		return false, false, nil
	}

	// All matching nodes lack the DPU label, don't inject VFs
	return true, false, nil
}

// podHasVFResources checks if the pod already has VF resources in either requests or limits.
func podHasVFResources(pod *corev1.Pod, vfResourceName corev1.ResourceName) bool {
	if len(pod.Spec.Containers) == 0 {
		return false
	}

	if pod.Spec.Containers[0].Resources.Requests != nil {
		if _, ok := pod.Spec.Containers[0].Resources.Requests[vfResourceName]; ok {
			return true
		}
	}

	if pod.Spec.Containers[0].Resources.Limits != nil {
		if _, ok := pod.Spec.Containers[0].Resources.Limits[vfResourceName]; ok {
			return true
		}
	}

	return false
}

// addAffinityForNonDPUNodes patches the pod's node affinity to explicitly exclude nodes with the DPU label.
func addAffinityForNonDPUNodes(ctx context.Context, pod *corev1.Pod, dpuHostLabelKey string, dpuHostLabelValue string) {
	log := ctrl.LoggerFrom(ctx)

	// Initialize pod affinity if needed
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	if pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
	}

	// Create a node selector term that excludes DPU nodes
	excludeDPUTerm := corev1.NodeSelectorTerm{
		MatchExpressions: []corev1.NodeSelectorRequirement{
			{
				Key:      dpuHostLabelKey,
				Operator: corev1.NodeSelectorOpNotIn,
				Values:   []string{dpuHostLabelValue},
			},
		},
	}

	// If there are existing terms, we need to add the DPU exclusion to each term (AND logic)
	// If no existing terms, add the exclusion as a new term
	terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) == 0 {
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = []corev1.NodeSelectorTerm{excludeDPUTerm}
		log.Info("patched pod with node affinity to exclude DPU nodes")
	} else {
		// Add the DPU exclusion to all existing terms to maintain OR semantics across terms
		// while adding AND logic within each term
		patchedCount := 0
		for i := range terms {
			// Check if this specific term already has the exclusion to avoid duplicates
			hasExclusion := false
			for _, expr := range terms[i].MatchExpressions {
				// Skip if the expression is not for the DPU label
				if expr.Key != dpuHostLabelKey {
					continue
				}
				// DoesNotExist is stricter than NotIn - it excludes any node with the label
				if expr.Operator == corev1.NodeSelectorOpDoesNotExist {
					hasExclusion = true
					break
				}
				// Check if NotIn already includes the value
				if expr.Operator == corev1.NodeSelectorOpNotIn {
					for _, val := range expr.Values {
						if val == dpuHostLabelValue {
							hasExclusion = true
							break
						}
					}
					if hasExclusion {
						break
					}
				}
			}
			if !hasExclusion {
				terms[i].MatchExpressions = append(terms[i].MatchExpressions, corev1.NodeSelectorRequirement{
					Key:      dpuHostLabelKey,
					Operator: corev1.NodeSelectorOpNotIn,
					Values:   []string{dpuHostLabelValue},
				})
				patchedCount++
			}
		}
		if patchedCount > 0 {
			log.Info("patched pod with node affinity to exclude DPU nodes", "termsCount", len(terms), "patchedTerms", patchedCount)
		} else {
			log.Info("all pod node affinity terms already exclude DPU nodes", "termsCount", len(terms))
		}
	}
}

func injectNetworkResources(ctx context.Context, pod *corev1.Pod, netAttachDefName string, netAttachDefNamespace string, vfResourceName corev1.ResourceName) error {
	log := ctrl.LoggerFrom(ctx)

	// Initialize resources if not present
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
