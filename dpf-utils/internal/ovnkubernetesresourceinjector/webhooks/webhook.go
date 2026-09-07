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
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
	// RuntimeClassNADMappings maps a pod runtimeClassName to a NAD name. Pods whose runtimeClass matches a key use
	// the mapped NAD; all others fall through to NADName (the default).
	RuntimeClassNADMappings map[string]string
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

	// Resolve which NAD to use based on the pod's runtimeClassName.
	nadName := webhook.resolveNADName(pod)

	// Get VF resource name early to check if pod already has resources
	vfResourceName, err := getVFResourceName(ctx, webhook.Client, nadName, webhook.Settings.NADNamespace)
	if err != nil {
		return fmt.Errorf("error while getting VF resource name: %w", err)
	}

	// If pod already has VF resources, inject without checking affinity
	if podHasVFResources(pod, vfResourceName) {
		return injectNetworkResources(ctx, pod, nadName, webhook.Settings.NADNamespace, vfResourceName)
	}

	// If pod targets a specific node via spec.nodeName, decide based on that node alone.
	// The general mixed-node logic is wrong here — it would add an exclusion affinity
	// that contradicts the explicit node assignment, causing the kubelet to reject the pod.
	if pod.Spec.NodeName != "" {
		isDPU, err := webhook.isNodeDPUHost(ctx, pod.Spec.NodeName)
		if err != nil {
			return err
		}
		if isDPU {
			return injectNetworkResources(ctx, pod, nadName, webhook.Settings.NADNamespace, vfResourceName)
		}
		return nil
	}

	// If pod has per-node anti-affinity and a controller owner, distribute pods across
	// DPU and non-DPU nodes based on how many siblings have already been assigned to each.
	decided, shouldInject, err := webhook.tryDistributeWithAntiAffinity(ctx, pod, vfResourceName)
	if err != nil {
		return err
	}
	if decided {
		if shouldInject {
			return injectNetworkResources(ctx, pod, nadName, webhook.Settings.NADNamespace, vfResourceName)
		}
		return nil
	}

	// If pod has PVCs bound to PVs pinned to a specific node, decide based on that node.
	// Without this, the webhook adds an exclusion affinity that contradicts the PV's node
	// binding, making the pod unschedulable.
	pvNodeDecided, pvNodeIsDPU, err := webhook.checkPVNodeAffinity(ctx, pod)
	if err != nil {
		return err
	}
	if pvNodeDecided {
		if pvNodeIsDPU {
			return injectNetworkResources(ctx, pod, nadName, webhook.Settings.NADNamespace, vfResourceName)
		}
		return nil
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

	return injectNetworkResources(ctx, pod, nadName, webhook.Settings.NADNamespace, vfResourceName)
}

// resolveNADName returns the NAD name to use for the given pod. If the pod has a runtimeClassName that matches an
// entry in RuntimeClassNADMappings, the mapped NAD name is returned; otherwise NADName is used as the default.
func (webhook *NetworkInjector) resolveNADName(pod *corev1.Pod) string {
	if pod.Spec.RuntimeClassName != nil && *pod.Spec.RuntimeClassName != "" {
		if mapped, ok := webhook.Settings.RuntimeClassNADMappings[*pod.Spec.RuntimeClassName]; ok {
			return mapped
		}
	}
	return webhook.Settings.NADName
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

// nodeIsDPUHost checks whether a node has the DPU host label.
func (webhook *NetworkInjector) nodeIsDPUHost(node *corev1.Node) bool {
	if node.Labels == nil {
		return false
	}
	value, exists := node.Labels[webhook.Settings.DPUHostLabelKey]
	return exists && value == webhook.Settings.DPUHostLabelValue
}

// checkPVNodeAffinity checks if the pod has PVCs bound to PVs with node affinity.
// If all PV-bound nodes are DPU hosts, returns (true, true). If all are non-DPU,
// returns (true, false). If mixed, unbound, or no PVCs, returns (false, false, nil)
// to let the caller fall through to general logic.
func (webhook *NetworkInjector) checkPVNodeAffinity(ctx context.Context, pod *corev1.Pod) (decided bool, isDPU bool, err error) {
	var pvNodes []corev1.Node
	hasPVC := false

	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim == nil {
			continue
		}
		hasPVC = true

		pvc := &corev1.PersistentVolumeClaim{}
		if err := webhook.Client.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: vol.PersistentVolumeClaim.ClaimName}, pvc); err != nil {
			return false, false, nil
		}
		if pvc.Spec.VolumeName == "" {
			return false, false, nil
		}

		pv := &corev1.PersistentVolume{}
		if err := webhook.Client.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
			return false, false, nil
		}

		if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
			continue
		}

		nodes, err := webhook.nodesMatchingSelector(ctx, pv.Spec.NodeAffinity.Required)
		if err != nil {
			return false, false, err
		}
		pvNodes = append(pvNodes, nodes...)
	}

	if !hasPVC || len(pvNodes) == 0 {
		return false, false, nil
	}

	dpuCount := 0
	for i := range pvNodes {
		if webhook.nodeIsDPUHost(&pvNodes[i]) {
			dpuCount++
		}
	}

	if dpuCount == len(pvNodes) {
		return true, true, nil
	}
	if dpuCount == 0 {
		return true, false, nil
	}
	return false, false, nil
}

// nodesMatchingSelector returns nodes that match a NodeSelector.
func (webhook *NetworkInjector) nodesMatchingSelector(ctx context.Context, selector *corev1.NodeSelector) ([]corev1.Node, error) {
	nodeList := &corev1.NodeList{}
	if err := webhook.Client.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	var matching []corev1.Node
	for _, node := range nodeList.Items {
		if nodeMatchesSelector(&node, selector) {
			matching = append(matching, node)
		}
	}
	return matching, nil
}

// nodeMatchesSelector checks if a node matches any term in a NodeSelector.
func nodeMatchesSelector(node *corev1.Node, selector *corev1.NodeSelector) bool {
	for _, term := range selector.NodeSelectorTerms {
		if nodeMatchesTerm(node, &term) {
			return true
		}
	}
	return false
}

// nodeMatchesTerm checks if a node matches a single NodeSelectorTerm.
func nodeMatchesTerm(node *corev1.Node, term *corev1.NodeSelectorTerm) bool {
	for _, req := range term.MatchExpressions {
		if !nodeSatisfiesRequirement(node.Labels, req) {
			return false
		}
	}
	return true
}

// nodeSatisfiesRequirement checks if a node's labels satisfy a single NodeSelectorRequirement.
func nodeSatisfiesRequirement(nodeLabels map[string]string, req corev1.NodeSelectorRequirement) bool {
	value, exists := nodeLabels[req.Key]
	switch req.Operator {
	case corev1.NodeSelectorOpIn:
		if !exists {
			return false
		}
		for _, v := range req.Values {
			if v == value {
				return true
			}
		}
		return false
	case corev1.NodeSelectorOpNotIn:
		if !exists {
			return true
		}
		for _, v := range req.Values {
			if v == value {
				return false
			}
		}
		return true
	case corev1.NodeSelectorOpExists:
		return exists
	case corev1.NodeSelectorOpDoesNotExist:
		return !exists
	default:
		return false
	}
}

// isNodeDPUHost fetches a node by name and checks whether it is a DPU host.
func (webhook *NetworkInjector) isNodeDPUHost(ctx context.Context, nodeName string) (bool, error) {
	node := &corev1.Node{}
	if err := webhook.Client.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	return webhook.nodeIsDPUHost(node), nil
}

// ownerMutexes serializes webhook decisions for pods from the same controller,
// preventing race conditions when a ReplicaSet creates multiple pods concurrently.
var ownerMutexes sync.Map

func getOwnerMutex(uid types.UID) *sync.Mutex {
	val, _ := ownerMutexes.LoadOrStore(uid, &sync.Mutex{})
	return val.(*sync.Mutex)
}

// tryDistributeWithAntiAffinity handles pods with per-node anti-affinity from a
// controller (ReplicaSet, etc.) targeting a mix of DPU and non-DPU nodes. Instead
// of applying the same policy to every pod, it distributes pods across the two
// partitions: the first N pods (where N = DPU node count) get routed to DPU nodes
// with VF injection, the rest get routed to non-DPU nodes.
//
// Returns (decided, shouldInject, error). If decided is false, the caller should
// fall through to the general shouldSkipInjection logic.
func (webhook *NetworkInjector) tryDistributeWithAntiAffinity(ctx context.Context, pod *corev1.Pod, vfResourceName corev1.ResourceName) (decided bool, shouldInject bool, err error) {
	if !hasPodPerNodeAntiAffinity(pod) || len(pod.OwnerReferences) == 0 {
		return false, false, nil
	}

	log := ctrl.LoggerFrom(ctx)

	matchingNodes, err := webhook.getMatchingNodes(ctx, pod)
	if err != nil {
		return false, false, err
	}

	dpuNodeCount := 0
	for i := range matchingNodes {
		if webhook.nodeIsDPUHost(&matchingNodes[i]) {
			dpuNodeCount++
		}
	}
	nonDPUNodeCount := len(matchingNodes) - dpuNodeCount

	if dpuNodeCount == 0 || nonDPUNodeCount == 0 {
		return false, false, nil
	}

	owner := pod.OwnerReferences[0]
	mu := getOwnerMutex(owner.UID)
	mu.Lock()
	defer mu.Unlock()

	siblingPods := &corev1.PodList{}
	if err := webhook.Client.List(ctx, siblingPods, client.InNamespace(pod.Namespace)); err != nil {
		return false, false, fmt.Errorf("failed to list pods in namespace: %w", err)
	}

	dpuAssigned := 0
	for i := range siblingPods.Items {
		sibling := &siblingPods.Items[i]
		if !hasOwnerWithUID(sibling.OwnerReferences, owner.UID) {
			continue
		}
		if podHasVFResources(sibling, vfResourceName) {
			dpuAssigned++
		}
	}

	if dpuAssigned < dpuNodeCount {
		log.Info("distributing pod to DPU node", "dpuAssigned", dpuAssigned, "dpuNodeCount", dpuNodeCount)
		addAffinityForDPUNodes(ctx, pod, webhook.Settings.DPUHostLabelKey, webhook.Settings.DPUHostLabelValue)
		return true, true, nil
	}

	log.Info("distributing pod to non-DPU node", "dpuAssigned", dpuAssigned, "dpuNodeCount", dpuNodeCount)
	addAffinityForNonDPUNodes(ctx, pod, webhook.Settings.DPUHostLabelKey, webhook.Settings.DPUHostLabelValue)
	return true, false, nil
}

// hasPodPerNodeAntiAffinity checks if the pod has required pod anti-affinity with
// topologyKey kubernetes.io/hostname, meaning at most one pod per node.
func hasPodPerNodeAntiAffinity(pod *corev1.Pod) bool {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.PodAntiAffinity == nil {
		return false
	}
	for _, term := range pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if term.TopologyKey == "kubernetes.io/hostname" {
			return true
		}
	}
	return false
}

// hasOwnerWithUID checks if the given owner references include one with the specified UID.
func hasOwnerWithUID(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range refs {
		if ref.UID == uid {
			return true
		}
	}
	return false
}

// getMatchingNodes returns nodes that match the pod's scheduling requirements.
func (webhook *NetworkInjector) getMatchingNodes(ctx context.Context, pod *corev1.Pod) ([]corev1.Node, error) {
	requiredNodeAffinity := nodeaffinity.GetRequiredNodeAffinity(pod)

	listOpts := []client.ListOption{}
	if len(pod.Spec.NodeSelector) > 0 {
		labelSelector := labels.SelectorFromSet(pod.Spec.NodeSelector)
		listOpts = append(listOpts, client.MatchingLabelsSelector{Selector: labelSelector})
	}

	nodeList := &corev1.NodeList{}
	if err := webhook.Client.List(ctx, nodeList, listOpts...); err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	var matchingNodes []corev1.Node
	for _, node := range nodeList.Items {
		matches, err := requiredNodeAffinity.Match(&node)
		if err != nil {
			return nil, fmt.Errorf("failed to match node affinity: %w", err)
		}
		if matches {
			matchingNodes = append(matchingNodes, node)
		}
	}
	return matchingNodes, nil
}

// addAffinityForDPUNodes patches the pod's node affinity to require nodes with the DPU label.
func addAffinityForDPUNodes(ctx context.Context, pod *corev1.Pod, dpuHostLabelKey string, dpuHostLabelValue string) {
	log := ctrl.LoggerFrom(ctx)

	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	if pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
	}

	requireDPUExpr := corev1.NodeSelectorRequirement{
		Key:      dpuHostLabelKey,
		Operator: corev1.NodeSelectorOpIn,
		Values:   []string{dpuHostLabelValue},
	}

	terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) == 0 {
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = []corev1.NodeSelectorTerm{
			{MatchExpressions: []corev1.NodeSelectorRequirement{requireDPUExpr}},
		}
	} else {
		for i := range terms {
			terms[i].MatchExpressions = append(terms[i].MatchExpressions, requireDPUExpr)
		}
	}

	log.Info("patched pod with node affinity to require DPU nodes")
}

// shouldSkipInjection determines if VF injection should be skipped based on the pod's scheduling requirements and matching nodes.
func (webhook *NetworkInjector) shouldSkipInjection(ctx context.Context, pod *corev1.Pod) (skipInjection bool, shouldAddAffinityForNonDPUNodes bool, error error) {
	matchingNodes, err := webhook.getMatchingNodes(ctx, pod)
	if err != nil {
		return false, false, err
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
	for i := range matchingNodes {
		if webhook.nodeIsDPUHost(&matchingNodes[i]) {
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
