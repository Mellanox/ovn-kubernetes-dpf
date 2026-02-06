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
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNetworkInjector_Default(t *testing.T) {
	g := NewWithT(t)
	nodeWithoutDPUName := "node-without-dpu"
	nodeWithDPUName := "node-with-dpu"
	nodeWithNoLabelsName := "node-with-no-labels"
	resourceName := corev1.ResourceName("test-resource")

	objects := []client.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeWithoutDPUName,
				Labels: map[string]string{
					"node-type":   "no-dpu",
					"environment": "production",
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeWithDPUName,
				Labels: map[string]string{
					"k8s.ovn.org/dpu-host": "",
					"environment":          "production",
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeWithNoLabelsName,
			},
		},
		&unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "k8s.cni.cncf.io/v1",
				"kind":       "NetworkAttachmentDefinition",
				"metadata": map[string]interface{}{
					"name":      "dpf-ovn-kubernetes",
					"namespace": "ovn-kubernetes",
					"annotations": map[string]interface{}{
						"k8s.v1.cni.cncf.io/resourceName": resourceName.String(),
					},
				},
			},
		},
	}

	nodeWithoutDPUMatchExpressionsDoesNotExist := []corev1.NodeSelectorTerm{
		{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      "k8s.ovn.org/dpu-host",
					Operator: corev1.NodeSelectorOpDoesNotExist,
				},
			},
		},
	}

	nodeWithoutDPUMatchExpressionsNotIn := []corev1.NodeSelectorTerm{
		{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      "k8s.ovn.org/dpu-host",
					Operator: corev1.NodeSelectorOpNotIn,
					Values:   []string{""},
				},
			},
		},
	}

	nodeWithDPUMatchExpressionsExists := []corev1.NodeSelectorTerm{
		{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      "k8s.ovn.org/dpu-host",
					Operator: corev1.NodeSelectorOpExists,
				},
			},
		},
	}

	// Affinity with 2 terms: one matching nodes without DPU, another matching nodes with DPU
	twoTermsOneWithoutDPUOneWithDPU := []corev1.NodeSelectorTerm{
		{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      "k8s.ovn.org/dpu-host",
					Operator: corev1.NodeSelectorOpDoesNotExist,
				},
			},
		},
		{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      "k8s.ovn.org/dpu-host",
					Operator: corev1.NodeSelectorOpExists,
				},
			},
		},
	}

	// Affinity with 2 terms: one matching nodes without DPU, another matching arbitrary nodes
	twoTermsOneWithoutDPUOneArbitrary := []corev1.NodeSelectorTerm{
		{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      "k8s.ovn.org/dpu-host",
					Operator: corev1.NodeSelectorOpDoesNotExist,
				},
			},
		},
		{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      "node-type",
					Operator: corev1.NodeSelectorOpExists,
				},
			},
		},
	}

	// Single term matching nodes without DPU indirectly (using a label that only non-DPU nodes have)
	singleTermMatchingNodesWithoutDPUIndirectly := []corev1.NodeSelectorTerm{
		{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      "node-type",
					Operator: corev1.NodeSelectorOpExists,
				},
			},
		},
	}

	basePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{},
						Limits:   corev1.ResourceList{},
					},
				},
			},
		},
	}
	hostNetworkPod := basePod.DeepCopy()
	hostNetworkPod.Spec.HostNetwork = true

	podWithNodeWithoutDPUSelector := basePod.DeepCopy()
	podWithNodeWithoutDPUSelector.Spec.NodeSelector = map[string]string{"node-type": "no-dpu"}

	podWithNodeWithDPUSelector := basePod.DeepCopy()
	podWithNodeWithDPUSelector.Spec.NodeSelector = map[string]string{"k8s.ovn.org/dpu-host": ""}

	podWithNodeSelectorMatchingBothDPUAndNonDPU := basePod.DeepCopy()
	podWithNodeSelectorMatchingBothDPUAndNonDPU.Spec.NodeSelector = map[string]string{"environment": "production"}

	podWithNodeSelectorMatchingNoNodes := basePod.DeepCopy()
	podWithNodeSelectorMatchingNoNodes.Spec.NodeSelector = map[string]string{"nonexistent-label": "nonexistent-value"}

	podWithNodeWithoutDPUMatchExpressionsDoesNotExist := basePod.DeepCopy()
	setSelectorTerms(podWithNodeWithoutDPUMatchExpressionsDoesNotExist, nodeWithoutDPUMatchExpressionsDoesNotExist)

	podWithNodeWithoutDPUMatchExpressionsNotIn := basePod.DeepCopy()
	setSelectorTerms(podWithNodeWithoutDPUMatchExpressionsNotIn, nodeWithoutDPUMatchExpressionsNotIn)

	podWithNodeWithDPUMatchExpressionsExists := basePod.DeepCopy()
	setSelectorTerms(podWithNodeWithDPUMatchExpressionsExists, nodeWithDPUMatchExpressionsExists)

	podWithNodeWithoutDPUNameSelectorTerms := basePod.DeepCopy()
	setSelectorTermsToNodeName(podWithNodeWithoutDPUNameSelectorTerms, nodeWithoutDPUName)

	podWithNodeWithDPUNameSelectorTerms := basePod.DeepCopy()
	setSelectorTermsToNodeName(podWithNodeWithDPUNameSelectorTerms, nodeWithDPUName)

	podWithNodeWithNoLabelsNameSelectorTerms := basePod.DeepCopy()
	setSelectorTermsToNodeName(podWithNodeWithNoLabelsNameSelectorTerms, nodeWithNoLabelsName)

	podWithAffinityTwoTermsOneWithoutDPUOneWithDPU := basePod.DeepCopy()
	setSelectorTerms(podWithAffinityTwoTermsOneWithoutDPUOneWithDPU, twoTermsOneWithoutDPUOneWithDPU)

	podWithAffinityTwoTermsOneWithoutDPUOneArbitrary := basePod.DeepCopy()
	setSelectorTerms(podWithAffinityTwoTermsOneWithoutDPUOneArbitrary, twoTermsOneWithoutDPUOneArbitrary)

	podWithSingleTermMatchingNodesWithoutDPUIndirectly := basePod.DeepCopy()
	setSelectorTerms(podWithSingleTermMatchingNodesWithoutDPUIndirectly, singleTermMatchingNodesWithoutDPUIndirectly)

	podWithExistingVFResources := basePod.DeepCopy()
	podWithExistingVFResources.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
		resourceName: resource.MustParse("1"),
	}
	podWithExistingVFResources.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		resourceName: resource.MustParse("1"),
	}

	tests := []struct {
		name                  string
		pod                   *corev1.Pod
		expectedResourceCount string
		expectAnnotation      bool
	}{
		{
			name:                  "don't inject resource into pod that has hostNetwork == true",
			pod:                   hostNetworkPod,
			expectedResourceCount: "0",
		},
		{
			name:                  "inject VF to pod that has no nodeSelector or nodeAffinity",
			pod:                   basePod,
			expectedResourceCount: "1",
			expectAnnotation:      true,
		},
		{
			name:                  "don't inject VF to pod that has nodeSelector matching only hosts without DPU and no affinity",
			pod:                   podWithNodeWithoutDPUSelector,
			expectedResourceCount: "0",
		},
		{
			name:                  "inject VF to pod that has nodeSelector matching only hosts with DPU and no affinity",
			pod:                   podWithNodeWithDPUSelector,
			expectedResourceCount: "1",
			expectAnnotation:      true,
		},
		{
			name:                  "inject VF to pod that has nodeSelector matching both hosts with and without DPU and no affinity",
			pod:                   podWithNodeSelectorMatchingBothDPUAndNonDPU,
			expectedResourceCount: "1",
			expectAnnotation:      true,
		},
		{
			name:                  "inject VF to pod that has nodeSelector matching no existing nodes",
			pod:                   podWithNodeSelectorMatchingNoNodes,
			expectedResourceCount: "1",
			expectAnnotation:      true,
		},
		{
			name:                  "don't inject VF to pod that has no nodeSelector and affinity with a single term matching nodes without DPU",
			pod:                   podWithNodeWithoutDPUMatchExpressionsDoesNotExist,
			expectedResourceCount: "0",
		},
		{
			name:                  "don't inject VF to pod that has no nodeSelector and affinity with a single term using NotIn operator to exclude DPU hosts",
			pod:                   podWithNodeWithoutDPUMatchExpressionsNotIn,
			expectedResourceCount: "0",
		},
		{
			name:                  "inject VF to pod that has no nodeSelector and affinity with a single term matching nodes with DPU",
			pod:                   podWithNodeWithDPUMatchExpressionsExists,
			expectedResourceCount: "1",
			expectAnnotation:      true,
		},
		{
			name:                  "inject VF to pod that has no nodeSelector and affinity with 2 terms, one matching nodes without DPU and another matching nodes with DPU",
			pod:                   podWithAffinityTwoTermsOneWithoutDPUOneWithDPU,
			expectedResourceCount: "1",
			expectAnnotation:      true,
		},
		{
			name:                  "don't inject VF to pod that has no nodeSelector and affinity with 2 terms, one matching nodes without DPU directly and another matching nodes without DPU indirectly",
			pod:                   podWithAffinityTwoTermsOneWithoutDPUOneArbitrary,
			expectedResourceCount: "0",
		},
		{
			name:                  "don't inject VF to pod that has no nodeSelector and affinity with single term, matching nodes without DPU indirectly",
			pod:                   podWithSingleTermMatchingNodesWithoutDPUIndirectly,
			expectedResourceCount: "0",
		},
		{
			name:                  "inject VF to pod that has no nodeSelector and affinity with a single term matching specific node name, which node has DPU label",
			pod:                   podWithNodeWithDPUNameSelectorTerms,
			expectedResourceCount: "1",
			expectAnnotation:      true,
		},
		{
			name:                  "don't inject VF to pod that has no nodeSelector and affinity with a single term matching specific node name, which node doesn't have DPU label",
			pod:                   podWithNodeWithoutDPUNameSelectorTerms,
			expectedResourceCount: "0",
		},
		{
			name:                  "don't inject VF to pod that has no nodeSelector and affinity with a single term matching specific node name, which node has no labels",
			pod:                   podWithNodeWithNoLabelsNameSelectorTerms,
			expectedResourceCount: "0",
		},
		{
			name:                  "inject resources into pod with existing resource claims",
			pod:                   podWithExistingVFResources,
			expectedResourceCount: "2",
			expectAnnotation:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := scheme.Scheme
			fakeclient := fake.NewClientBuilder().WithObjects(objects...).WithScheme(s).Build()
			webhook := &NetworkInjector{
				Client: fakeclient,
				Settings: NetworkInjectorSettings{
					NADName:              "dpf-ovn-kubernetes",
					NADNamespace:         "ovn-kubernetes",
					DPUHostLabelKey:      "k8s.ovn.org/dpu-host",
					DPUHostLabelValue:    "",
					PrioritizeOffloading: true,
				},
			}
			err := webhook.Default(context.Background(), tt.pod)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(tt.pod.Spec.Containers[0].Resources.Limits[resourceName].Equal(resource.MustParse(tt.expectedResourceCount))).To(BeTrue())
			g.Expect(tt.pod.Spec.Containers[0].Resources.Requests[resourceName].Equal(resource.MustParse(tt.expectedResourceCount))).To(BeTrue())
			g.Expect(tt.pod.Annotations[annotationKeyToBeInjected] == "ovn-kubernetes/dpf-ovn-kubernetes").To(Equal(tt.expectAnnotation))
		})
	}
}

func TestNetworkInjector_PrioritizeOffloadingDisabled(t *testing.T) {
	g := NewWithT(t)
	nodeWithoutDPUName := "node-without-dpu"
	nodeWithDPUName := "node-with-dpu"
	resourceName := corev1.ResourceName("test-resource")

	nodeWithNoLabelsName := "node-with-no-labels"

	objects := []client.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeWithoutDPUName,
				Labels: map[string]string{
					"node-type":   "no-dpu",
					"environment": "production",
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeWithDPUName,
				Labels: map[string]string{
					"k8s.ovn.org/dpu-host": "",
					"environment":          "production",
				},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeWithNoLabelsName,
			},
		},
		&unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "k8s.cni.cncf.io/v1",
				"kind":       "NetworkAttachmentDefinition",
				"metadata": map[string]interface{}{
					"name":      "dpf-ovn-kubernetes",
					"namespace": "ovn-kubernetes",
					"annotations": map[string]interface{}{
						"k8s.v1.cni.cncf.io/resourceName": resourceName.String(),
					},
				},
			},
		},
	}

	basePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{},
						Limits:   corev1.ResourceList{},
					},
				},
			},
		},
	}

	hostNetworkPod := basePod.DeepCopy()
	hostNetworkPod.Spec.HostNetwork = true

	podWithNodeWithoutDPUSelector := basePod.DeepCopy()
	podWithNodeWithoutDPUSelector.Spec.NodeSelector = map[string]string{"node-type": "no-dpu"}

	podWithNodeWithDPUSelector := basePod.DeepCopy()
	podWithNodeWithDPUSelector.Spec.NodeSelector = map[string]string{"k8s.ovn.org/dpu-host": ""}

	podWithSelectorMatchingBothDPUAndNonDPU := basePod.DeepCopy()
	podWithSelectorMatchingBothDPUAndNonDPU.Spec.NodeSelector = map[string]string{"environment": "production"}

	podWithSelectorMatchingNoNodes := basePod.DeepCopy()
	podWithSelectorMatchingNoNodes.Spec.NodeSelector = map[string]string{"nonexistent-label": "nonexistent-value"}

	nodeWithoutDPUMatchExpressionsDoesNotExist := []corev1.NodeSelectorTerm{
		{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      "k8s.ovn.org/dpu-host",
					Operator: corev1.NodeSelectorOpDoesNotExist,
				},
			},
		},
	}

	podWithAffinityMatchingOnlyNonDPU := basePod.DeepCopy()
	setSelectorTerms(podWithAffinityMatchingOnlyNonDPU, nodeWithoutDPUMatchExpressionsDoesNotExist)

	nodeWithoutDPUMatchExpressionsNotIn := []corev1.NodeSelectorTerm{
		{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      "k8s.ovn.org/dpu-host",
					Operator: corev1.NodeSelectorOpNotIn,
					Values:   []string{""},
				},
			},
		},
	}

	podWithAffinityMatchingOnlyNonDPUNotIn := basePod.DeepCopy()
	setSelectorTerms(podWithAffinityMatchingOnlyNonDPUNotIn, nodeWithoutDPUMatchExpressionsNotIn)

	nodeWithDPUMatchExpressionsExists := []corev1.NodeSelectorTerm{
		{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      "k8s.ovn.org/dpu-host",
					Operator: corev1.NodeSelectorOpExists,
				},
			},
		},
	}

	podWithAffinityMatchingOnlyDPU := basePod.DeepCopy()
	setSelectorTerms(podWithAffinityMatchingOnlyDPU, nodeWithDPUMatchExpressionsExists)

	twoTermsOneWithoutDPUOneWithDPU := []corev1.NodeSelectorTerm{
		{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      "k8s.ovn.org/dpu-host",
					Operator: corev1.NodeSelectorOpDoesNotExist,
				},
			},
		},
		{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      "k8s.ovn.org/dpu-host",
					Operator: corev1.NodeSelectorOpExists,
				},
			},
		},
	}

	podWithAffinityTwoTermsOneWithoutDPUOneWithDPU := basePod.DeepCopy()
	setSelectorTerms(podWithAffinityTwoTermsOneWithoutDPUOneWithDPU, twoTermsOneWithoutDPUOneWithDPU)

	twoTermsOneWithoutDPUOneArbitrary := []corev1.NodeSelectorTerm{
		{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      "k8s.ovn.org/dpu-host",
					Operator: corev1.NodeSelectorOpDoesNotExist,
				},
			},
		},
		{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      "node-type",
					Operator: corev1.NodeSelectorOpExists,
				},
			},
		},
	}

	podWithAffinityTwoTermsOneWithoutDPUOneArbitrary := basePod.DeepCopy()
	setSelectorTerms(podWithAffinityTwoTermsOneWithoutDPUOneArbitrary, twoTermsOneWithoutDPUOneArbitrary)

	singleTermMatchingNodesWithoutDPUIndirectly := []corev1.NodeSelectorTerm{
		{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{
					Key:      "node-type",
					Operator: corev1.NodeSelectorOpExists,
				},
			},
		},
	}

	podWithSingleTermMatchingNodesWithoutDPUIndirectly := basePod.DeepCopy()
	setSelectorTerms(podWithSingleTermMatchingNodesWithoutDPUIndirectly, singleTermMatchingNodesWithoutDPUIndirectly)

	podWithAffinityMatchingNodeByNameDPU := basePod.DeepCopy()
	setSelectorTermsToNodeName(podWithAffinityMatchingNodeByNameDPU, nodeWithDPUName)

	podWithAffinityMatchingNodeByNameNonDPU := basePod.DeepCopy()
	setSelectorTermsToNodeName(podWithAffinityMatchingNodeByNameNonDPU, nodeWithoutDPUName)

	podWithAffinityMatchingNodeByNameNoLabels := basePod.DeepCopy()
	setSelectorTermsToNodeName(podWithAffinityMatchingNodeByNameNoLabels, nodeWithNoLabelsName)

	podWithExistingVFResources := basePod.DeepCopy()
	podWithExistingVFResources.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
		resourceName: resource.MustParse("1"),
	}
	podWithExistingVFResources.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		resourceName: resource.MustParse("1"),
	}

	tests := []struct {
		name                       string
		pod                        *corev1.Pod
		expectedResourceCount      string
		expectAnnotation           bool
		expectAffinityPatch        bool
		expectedAffinityTermsCount int
	}{
		{
			name:                       "don't inject resource into pod that has hostNetwork == true (PrioritizeOffloading=false)",
			pod:                        hostNetworkPod,
			expectedResourceCount:      "0",
			expectAffinityPatch:        false,
			expectedAffinityTermsCount: 0,
		},
		{
			name:                       "don't inject VF and patch affinity for pod that has no nodeSelector or nodeAffinity (PrioritizeOffloading=false)",
			pod:                        basePod,
			expectedResourceCount:      "0",
			expectAnnotation:           false,
			expectAffinityPatch:        true,
			expectedAffinityTermsCount: 1,
		},
		{
			name:                       "don't inject VF when nodeSelector matches only hosts without DPU (PrioritizeOffloading=false)",
			pod:                        podWithNodeWithoutDPUSelector,
			expectedResourceCount:      "0",
			expectAffinityPatch:        false,
			expectedAffinityTermsCount: 0,
		},
		{
			name:                       "inject VF when nodeSelector matches only hosts with DPU (PrioritizeOffloading=false)",
			pod:                        podWithNodeWithDPUSelector,
			expectedResourceCount:      "1",
			expectAnnotation:           true,
			expectAffinityPatch:        false,
			expectedAffinityTermsCount: 0,
		},
		{
			name:                       "don't inject VF and patch affinity when nodeSelector matches both hosts with and without DPU (PrioritizeOffloading=false)",
			pod:                        podWithSelectorMatchingBothDPUAndNonDPU,
			expectedResourceCount:      "0",
			expectAffinityPatch:        true,
			expectedAffinityTermsCount: 1,
		},
		{
			name:                       "inject VF when nodeSelector matches no existing nodes (PrioritizeOffloading=false)",
			pod:                        podWithSelectorMatchingNoNodes,
			expectedResourceCount:      "1",
			expectAnnotation:           true,
			expectAffinityPatch:        false,
			expectedAffinityTermsCount: 0,
		},
		{
			name:                       "don't inject VF when affinity single term matches nodes without DPU using DoesNotExist (PrioritizeOffloading=false)",
			pod:                        podWithAffinityMatchingOnlyNonDPU,
			expectedResourceCount:      "0",
			expectAffinityPatch:        false,
			expectedAffinityTermsCount: 0,
		},
		{
			name:                       "don't inject VF when affinity single term matches nodes without DPU using NotIn operator (PrioritizeOffloading=false)",
			pod:                        podWithAffinityMatchingOnlyNonDPUNotIn,
			expectedResourceCount:      "0",
			expectAffinityPatch:        false,
			expectedAffinityTermsCount: 0,
		},
		{
			name:                       "inject VF when affinity single term matches nodes with DPU using Exists (PrioritizeOffloading=false)",
			pod:                        podWithAffinityMatchingOnlyDPU,
			expectedResourceCount:      "1",
			expectAnnotation:           true,
			expectAffinityPatch:        false,
			expectedAffinityTermsCount: 0,
		},
		{
			name:                       "don't inject VF and patch affinity when affinity has 2 terms, one matching nodes without DPU and another matching nodes with DPU (PrioritizeOffloading=false)",
			pod:                        podWithAffinityTwoTermsOneWithoutDPUOneWithDPU,
			expectedResourceCount:      "0",
			expectAffinityPatch:        true,
			expectedAffinityTermsCount: 2,
		},
		{
			name:                       "don't inject VF when affinity has 2 terms, both matching nodes without DPU (PrioritizeOffloading=false)",
			pod:                        podWithAffinityTwoTermsOneWithoutDPUOneArbitrary,
			expectedResourceCount:      "0",
			expectAffinityPatch:        false,
			expectedAffinityTermsCount: 0,
		},
		{
			name:                       "don't inject VF when affinity single term matches nodes without DPU indirectly (PrioritizeOffloading=false)",
			pod:                        podWithSingleTermMatchingNodesWithoutDPUIndirectly,
			expectedResourceCount:      "0",
			expectAffinityPatch:        false,
			expectedAffinityTermsCount: 0,
		},
		{
			name:                       "inject VF when affinity targets specific node by name which has DPU label (PrioritizeOffloading=false)",
			pod:                        podWithAffinityMatchingNodeByNameDPU,
			expectedResourceCount:      "1",
			expectAnnotation:           true,
			expectAffinityPatch:        false,
			expectedAffinityTermsCount: 0,
		},
		{
			name:                       "don't inject VF when affinity targets specific node by name which doesn't have DPU label (PrioritizeOffloading=false)",
			pod:                        podWithAffinityMatchingNodeByNameNonDPU,
			expectedResourceCount:      "0",
			expectAffinityPatch:        false,
			expectedAffinityTermsCount: 0,
		},
		{
			name:                       "don't inject VF when affinity targets specific node by name which has no labels (PrioritizeOffloading=false)",
			pod:                        podWithAffinityMatchingNodeByNameNoLabels,
			expectedResourceCount:      "0",
			expectAffinityPatch:        false,
			expectedAffinityTermsCount: 0,
		},
		{
			name:                       "inject additional resources for pod with existing resource claims that matches both DPU and non-DPU nodes (PrioritizeOffloading=false)",
			pod:                        podWithExistingVFResources,
			expectedResourceCount:      "2",
			expectAnnotation:           true,
			expectAffinityPatch:        false,
			expectedAffinityTermsCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := scheme.Scheme
			fakeclient := fake.NewClientBuilder().WithObjects(objects...).WithScheme(s).Build()
			webhook := &NetworkInjector{
				Client: fakeclient,
				Settings: NetworkInjectorSettings{
					NADName:              "dpf-ovn-kubernetes",
					NADNamespace:         "ovn-kubernetes",
					DPUHostLabelKey:      "k8s.ovn.org/dpu-host",
					DPUHostLabelValue:    "",
					PrioritizeOffloading: false,
				},
			}
			err := webhook.Default(context.Background(), tt.pod)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(tt.pod.Spec.Containers[0].Resources.Limits[resourceName].Equal(resource.MustParse(tt.expectedResourceCount))).To(BeTrue())
			g.Expect(tt.pod.Spec.Containers[0].Resources.Requests[resourceName].Equal(resource.MustParse(tt.expectedResourceCount))).To(BeTrue())
			g.Expect(tt.pod.Annotations[annotationKeyToBeInjected] == "ovn-kubernetes/dpf-ovn-kubernetes").To(Equal(tt.expectAnnotation))

			// Check if affinity was patched
			if tt.expectAffinityPatch {
				g.Expect(tt.pod.Spec.Affinity).NotTo(BeNil())
				g.Expect(tt.pod.Spec.Affinity.NodeAffinity).NotTo(BeNil())
				g.Expect(tt.pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution).NotTo(BeNil())
				terms := tt.pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
				g.Expect(len(terms)).To(Equal(tt.expectedAffinityTermsCount))

				// Verify each term has the DPU exclusion
				for _, term := range terms {
					foundExclusion := false
					for _, expr := range term.MatchExpressions {
						if expr.Key == "k8s.ovn.org/dpu-host" {
							if expr.Operator == corev1.NodeSelectorOpNotIn {
								foundExclusion = true
								g.Expect(expr.Values).To(ContainElement(""))
							} else if expr.Operator == corev1.NodeSelectorOpDoesNotExist {
								foundExclusion = true
							}
						}
					}
					g.Expect(foundExclusion).To(BeTrue(), "Expected DPU exclusion in affinity term")
				}
			}
		})
	}
}

func TestNetworkInjector_PreReqObjects(t *testing.T) {
	g := NewWithT(t)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "nginx",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{},
						Limits:   corev1.ResourceList{},
					},
				},
			},
		},
	}

	networkAttachDefWithoutAnnotation := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "k8s.cni.cncf.io/v1",
			"kind":       "NetworkAttachmentDefinition",
			"metadata": map[string]interface{}{
				"name":      "dpf-ovn-kubernetes",
				"namespace": "ovn-kubernetes",
			},
		},
	}

	networkAttachDefWithAnnotation := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "k8s.cni.cncf.io/v1",
			"kind":       "NetworkAttachmentDefinition",
			"metadata": map[string]interface{}{
				"name":      "dpf-ovn-kubernetes",
				"namespace": "ovn-kubernetes",
				"annotations": map[string]interface{}{
					"k8s.v1.cni.cncf.io/resourceName": "some-resource",
				},
			},
		},
	}

	tests := []struct {
		name            string
		existingObjects []client.Object
		expectError     bool
	}{
		{
			name:            "no NetworkAttachmentDefinition",
			existingObjects: nil,
			expectError:     true,
		},
		{
			name:            "no annotation on NetworkAttachmentDefinition",
			existingObjects: []client.Object{networkAttachDefWithoutAnnotation},
			expectError:     true,
		},
		{
			name:            "all prereq objects exist",
			existingObjects: []client.Object{networkAttachDefWithAnnotation},
			expectError:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := scheme.Scheme
			fakeclient := fake.NewClientBuilder().WithObjects(tt.existingObjects...).WithScheme(s).Build()
			webhook := &NetworkInjector{
				Client: fakeclient,
				Settings: NetworkInjectorSettings{
					NADName:              "dpf-ovn-kubernetes",
					NADNamespace:         "ovn-kubernetes",
					DPUHostLabelKey:      "k8s.ovn.org/dpu-host",
					DPUHostLabelValue:    "",
					PrioritizeOffloading: true,
				},
			}
			err := webhook.Default(context.Background(), pod)
			if tt.expectError {
				g.Expect(err).To(HaveOccurred())
			} else {
				g.Expect(err).ToNot(HaveOccurred())
			}
		})
	}
}

func TestAddAffinityForNonDPUNodes(t *testing.T) {
	g := NewWithT(t)
	dpuLabelKey := "k8s.ovn.org/dpu-host"
	dpuLabelValue := ""

	tests := []struct {
		name                   string
		pod                    *corev1.Pod
		expectedTermsCount     int
		expectedTermsWithNotIn int // Number of terms that should have NotIn operator (the one we add)
	}{
		{
			name: "patch pod with no affinity",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
				Spec:       corev1.PodSpec{},
			},
			expectedTermsCount:     1,
			expectedTermsWithNotIn: 1, // New term created with NotIn
		},
		{
			name: "patch pod with existing affinity matching production nodes",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      "environment",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"production"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expectedTermsCount:     1,
			expectedTermsWithNotIn: 1, // NotIn added to existing term
		},
		{
			name: "don't patch pod that already has DoesNotExist for DPU label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      dpuLabelKey,
												Operator: corev1.NodeSelectorOpDoesNotExist,
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expectedTermsCount:     1,
			expectedTermsWithNotIn: 0, // Already has DoesNotExist, no NotIn added
		},
		{
			name: "don't patch pod that already has NotIn with DPU value",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      dpuLabelKey,
												Operator: corev1.NodeSelectorOpNotIn,
												Values:   []string{dpuLabelValue},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expectedTermsCount:     1,
			expectedTermsWithNotIn: 1, // Already has NotIn, nothing added but we verify it's there
		},
		{
			name: "patch pod with multiple terms (OR logic)",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      "zone",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"us-east"},
											},
										},
									},
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      "zone",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"us-west"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expectedTermsCount:     2,
			expectedTermsWithNotIn: 2, // NotIn added to both terms
		},
		{
			name: "patch pod with multiple terms where one already has exclusion",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
								NodeSelectorTerms: []corev1.NodeSelectorTerm{
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      "zone",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"us-east"},
											},
											{
												Key:      dpuLabelKey,
												Operator: corev1.NodeSelectorOpDoesNotExist,
											},
										},
									},
									{
										MatchExpressions: []corev1.NodeSelectorRequirement{
											{
												Key:      "zone",
												Operator: corev1.NodeSelectorOpIn,
												Values:   []string{"us-west"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expectedTermsCount:     2,
			expectedTermsWithNotIn: 1, // Only second term gets NotIn added, first already has DoesNotExist
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			addAffinityForNonDPUNodes(ctx, tt.pod, dpuLabelKey, dpuLabelValue)

			// Verify affinity was initialized
			g.Expect(tt.pod.Spec.Affinity).NotTo(BeNil())
			g.Expect(tt.pod.Spec.Affinity.NodeAffinity).NotTo(BeNil())
			g.Expect(tt.pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution).NotTo(BeNil())

			terms := tt.pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
			g.Expect(len(terms)).To(Equal(tt.expectedTermsCount))

			// Count terms with NotIn operator for DPU label
			termsWithNotIn := 0
			for _, term := range terms {
				for _, expr := range term.MatchExpressions {
					if expr.Key == dpuLabelKey && expr.Operator == corev1.NodeSelectorOpNotIn {
						// Verify it has the correct value
						g.Expect(expr.Values).To(ContainElement(dpuLabelValue))
						termsWithNotIn++
						break
					}
				}
			}
			g.Expect(termsWithNotIn).To(Equal(tt.expectedTermsWithNotIn), "Expected specific number of terms with NotIn operator")
		})
	}
}

func setSelectorTermsToNodeName(pod *corev1.Pod, nodeName string) {
	setSelectorTerms(pod, []corev1.NodeSelectorTerm{
		{
			MatchFields: []corev1.NodeSelectorRequirement{
				{
					Key:      "metadata.name",
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{nodeName},
				},
			},
		},
	})
}

func setSelectorTerms(pod *corev1.Pod, terms []corev1.NodeSelectorTerm) {
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	if pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
	}
	pod.Spec.Affinity.NodeAffinity.
		RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = terms
}
