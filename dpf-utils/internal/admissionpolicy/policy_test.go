/*
Copyright 2025 NVIDIA

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

package admissionpolicy_test

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionregistrationv1alpha1 "k8s.io/api/admissionregistration/v1alpha1"
	"k8s.io/apimachinery/pkg/util/yaml"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// These must match the values used in generate-test-policy make target
	testResourceName = "nvidia.com/bf3-p0-vfs"
	testNADNamespace = "test-namespace"
	testNADName      = "dpf-ovn-kubernetes"
	testNamespace    = "default"
	timeout          = time.Second * 10
	interval         = time.Millisecond * 250
)

var _ = Describe("MutatingAdmissionPolicy", func() {

	BeforeEach(func() {
		// Create the policy and binding from loaded testdata
		Expect(k8sClient.Create(ctx, testPolicy.DeepCopy())).To(Succeed())
		Expect(k8sClient.Create(ctx, testPolicyBinding.DeepCopy())).To(Succeed())

		// Wait for policy to be ready
		time.Sleep(time.Second * 2)
	})

	AfterEach(func() {
		// Clean up policy and binding
		_ = k8sClient.Delete(ctx, testPolicy.DeepCopy())
		_ = k8sClient.Delete(ctx, testPolicyBinding.DeepCopy())
	})

	Context("when creating a pod without resources", func() {
		It("should inject the multus annotation and VF resources", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod-no-resources",
					Namespace: testNamespace,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "nginx:alpine",
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			// Fetch the created pod
			createdPod := &corev1.Pod{}
			Eventually(func() error {
				return k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), createdPod)
			}, timeout, interval).Should(Succeed())

			// Verify multus annotation was added
			Expect(createdPod.Annotations).To(HaveKey("v1.multus-cni.io/default-network"))
			Expect(createdPod.Annotations["v1.multus-cni.io/default-network"]).To(Equal(
				testNADNamespace + "/" + testNADName,
			))

			// Verify resource requests were added
			Expect(createdPod.Spec.Containers[0].Resources.Requests).To(HaveKey(corev1.ResourceName(testResourceName)))
			Expect(createdPod.Spec.Containers[0].Resources.Requests[corev1.ResourceName(testResourceName)]).To(Equal(resource.MustParse("1")))

			// Verify resource limits were added
			Expect(createdPod.Spec.Containers[0].Resources.Limits).To(HaveKey(corev1.ResourceName(testResourceName)))
			Expect(createdPod.Spec.Containers[0].Resources.Limits[corev1.ResourceName(testResourceName)]).To(Equal(resource.MustParse("1")))

			// Cleanup
			Expect(k8sClient.Delete(ctx, createdPod)).To(Succeed())
		})
	})

	Context("when creating a pod with existing VF resources", func() {
		It("should increment the VF resource count", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod-with-resources",
					Namespace: testNamespace,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "nginx:alpine",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceName(testResourceName): resource.MustParse("1"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceName(testResourceName): resource.MustParse("1"),
								},
							},
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			// Fetch the created pod
			createdPod := &corev1.Pod{}
			Eventually(func() error {
				return k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), createdPod)
			}, timeout, interval).Should(Succeed())

			// Verify resource requests were incremented (1 -> 2)
			Expect(createdPod.Spec.Containers[0].Resources.Requests[corev1.ResourceName(testResourceName)]).To(Equal(resource.MustParse("2")))
			Expect(createdPod.Spec.Containers[0].Resources.Limits[corev1.ResourceName(testResourceName)]).To(Equal(resource.MustParse("2")))

			// Cleanup
			Expect(k8sClient.Delete(ctx, createdPod)).To(Succeed())
		})
	})

	Context("when creating a pod with hostNetwork=true", func() {
		It("should NOT inject resources (excluded by matchCondition)", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod-hostnetwork",
					Namespace: testNamespace,
				},
				Spec: corev1.PodSpec{
					HostNetwork: true,
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "nginx:alpine",
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			// Fetch the created pod
			createdPod := &corev1.Pod{}
			Eventually(func() error {
				return k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), createdPod)
			}, timeout, interval).Should(Succeed())

			// Verify NO multus annotation was added
			Expect(createdPod.Annotations).NotTo(HaveKey("v1.multus-cni.io/default-network"))

			// Verify NO resource requests were added
			Expect(createdPod.Spec.Containers[0].Resources.Requests).NotTo(HaveKey(corev1.ResourceName(testResourceName)))

			// Cleanup
			Expect(k8sClient.Delete(ctx, createdPod)).To(Succeed())
		})
	})

	Context("when creating a pod with skip-injection annotation", func() {
		It("should NOT inject resources (excluded by matchCondition)", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod-skip-injection",
					Namespace: testNamespace,
					Annotations: map[string]string{
						"ovn.dpu.nvidia.com/skip-injection": "true",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "nginx:alpine",
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			// Fetch the created pod
			createdPod := &corev1.Pod{}
			Eventually(func() error {
				return k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), createdPod)
			}, timeout, interval).Should(Succeed())

			// Verify NO multus annotation was added (skip-injection should be preserved)
			Expect(createdPod.Annotations).NotTo(HaveKey("v1.multus-cni.io/default-network"))

			// Verify NO resource requests were added
			Expect(createdPod.Spec.Containers[0].Resources.Requests).NotTo(HaveKey(corev1.ResourceName(testResourceName)))

			// Cleanup
			Expect(k8sClient.Delete(ctx, createdPod)).To(Succeed())
		})
	})

	Context("when creating a pod with existing multus annotation", func() {
		It("should overwrite the annotation with the correct value and inject VF resources", func() {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod-existing-annotation",
					Namespace: testNamespace,
					Annotations: map[string]string{
						"v1.multus-cni.io/default-network": "some-other-namespace/some-other-nad",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "nginx:alpine",
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			// Fetch the created pod
			createdPod := &corev1.Pod{}
			Eventually(func() error {
				return k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), createdPod)
			}, timeout, interval).Should(Succeed())

			// Verify multus annotation was overwritten with the correct value
			Expect(createdPod.Annotations).To(HaveKey("v1.multus-cni.io/default-network"))
			Expect(createdPod.Annotations["v1.multus-cni.io/default-network"]).To(Equal(
				testNADNamespace + "/" + testNADName,
			))

			// Verify resource requests were added
			Expect(createdPod.Spec.Containers[0].Resources.Requests).To(HaveKey(corev1.ResourceName(testResourceName)))
			Expect(createdPod.Spec.Containers[0].Resources.Requests[corev1.ResourceName(testResourceName)]).To(Equal(resource.MustParse("1")))

			// Verify resource limits were added
			Expect(createdPod.Spec.Containers[0].Resources.Limits).To(HaveKey(corev1.ResourceName(testResourceName)))
			Expect(createdPod.Spec.Containers[0].Resources.Limits[corev1.ResourceName(testResourceName)]).To(Equal(resource.MustParse("1")))

			// Cleanup
			Expect(k8sClient.Delete(ctx, createdPod)).To(Succeed())
		})
	})
})

// loadPolicyFromTestdata reads the policy and binding from the helm-generated testdata file
func loadPolicyFromTestdata(path string) (*admissionregistrationv1alpha1.MutatingAdmissionPolicy, *admissionregistrationv1alpha1.MutatingAdmissionPolicyBinding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}

	var policy *admissionregistrationv1alpha1.MutatingAdmissionPolicy
	var binding *admissionregistrationv1alpha1.MutatingAdmissionPolicyBinding

	// Split YAML documents and decode each
	reader := yaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}

		// Skip empty documents
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}

		// Try to decode as MutatingAdmissionPolicy
		p := &admissionregistrationv1alpha1.MutatingAdmissionPolicy{}
		if err := yaml.Unmarshal(doc, p); err == nil && p.Kind == "MutatingAdmissionPolicy" {
			policy = p
			continue
		}

		// Try to decode as MutatingAdmissionPolicyBinding
		b := &admissionregistrationv1alpha1.MutatingAdmissionPolicyBinding{}
		if err := yaml.Unmarshal(doc, b); err == nil && b.Kind == "MutatingAdmissionPolicyBinding" {
			binding = b
			continue
		}
	}

	return policy, binding, nil
}
