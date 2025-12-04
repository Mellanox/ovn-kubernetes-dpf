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
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	admissionregistrationv1alpha1 "k8s.io/api/admissionregistration/v1alpha1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const testdataPath = "testdata/policy.yaml"

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc

	// Loaded from testdata
	testPolicy        *admissionregistrationv1alpha1.MutatingAdmissionPolicy
	testPolicyBinding *admissionregistrationv1alpha1.MutatingAdmissionPolicyBinding
)

func TestAdmissionPolicy(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Admission Policy Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.Background())

	By("loading policy from testdata")
	var err error
	testPolicy, testPolicyBinding, err = loadPolicyFromTestdata(testdataPath)
	Expect(err).NotTo(HaveOccurred())
	Expect(testPolicy).NotTo(BeNil())
	Expect(testPolicyBinding).NotTo(BeNil())

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		// Enable the MutatingAdmissionPolicy feature gate
		ControlPlane: envtest.ControlPlane{
			APIServer: &envtest.APIServer{
				Args: []string{
					"--feature-gates=MutatingAdmissionPolicy=true",
					"--runtime-config=admissionregistration.k8s.io/v1alpha1=true",
				},
			},
		},
		ErrorIfCRDPathMissing: false,
	}

	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	// Add admissionregistration v1alpha1 to scheme
	err = admissionregistrationv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())
})

var _ = AfterSuite(func() {
	cancel()
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})
