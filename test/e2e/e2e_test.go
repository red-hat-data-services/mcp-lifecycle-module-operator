/*
Copyright 2026.

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

package e2e

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformcommon "github.com/opendatahub-io/odh-platform-utilities/api/common"

	v1alpha1 "github.com/opendatahub-io/mcp-lifecycle-module-operator/api/v1alpha1"
)

var (
	_ client.Object = &v1alpha1.MCPLifecycleOperator{}
)

const (
	operandDeployment        = "mcp-lifecycle-operator-controller-manager"
	operandCRD               = "mcpservers.mcp.x-k8s.io"
	moduleOperatorDeployment = "mcp-lifecycle-module-operator-controller-manager"

	timeout            = 5 * time.Minute
	interval           = 5 * time.Second
	consistentDuration = 30 * time.Second
	consistentInterval = 5 * time.Second
)

var operandNamespace = cmp.Or(os.Getenv("SYSTEM_NAMESPACE"), "mcp-lifecycle-module-operator-system")

var _ = Describe("MCPLifecycleOperator", func() {
	ctx := context.Background()

	AfterEach(func() {
		cr := &v1alpha1.MCPLifecycleOperator{
			ObjectMeta: metav1.ObjectMeta{
				Name: v1alpha1.MCPLifecycleOperatorInstanceName,
			},
		}
		err := k8sClient.Delete(ctx, cr)
		if err != nil && !k8serr.IsNotFound(err) {
			Fail("failed to delete MCPLifecycleOperator CR: " + err.Error())
		}
	})

	It("deploys the MCP Lifecycle Operator with a reachable webhook", func() {
		createManagedCR(ctx)
		waitForOperandReady(ctx)

		const serviceName = "mcp-lifecycle-operator-webhook-service"
		By("Checking the webhook Service and fail-closed admission registration")
		Eventually(func(g Gomega) {
			service := &corev1.Service{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: serviceName}, service)).To(Succeed())
			g.Expect(service.Spec.Selector).To(HaveKeyWithValue("app.kubernetes.io/name", "mcp-lifecycle-operator"))

			module := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: moduleOperatorDeployment}, module)).To(Succeed())
			g.Expect(labels.SelectorFromSet(service.Spec.Selector).Matches(labels.Set(module.Spec.Template.Labels))).To(BeFalse(),
				"webhook Service selector %v matches module Pod labels %v", service.Spec.Selector, module.Spec.Template.Labels)

			configuration := &admissionv1.ValidatingWebhookConfiguration{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mcp-lifecycle-operator-validating-webhook-configuration"}, configuration)).To(Succeed())
			index := slices.IndexFunc(configuration.Webhooks, func(webhook admissionv1.ValidatingWebhook) bool {
				return webhook.Name == "vmcpserver.mcp.x-k8s.io"
			})
			g.Expect(index).To(BeNumerically(">=", 0))
			webhook := configuration.Webhooks[index]
			g.Expect(webhook.ClientConfig.Service).To(HaveValue(SatisfyAll(
				HaveField("Name", serviceName),
				HaveField("Namespace", operandNamespace),
			)))
			g.Expect(webhook.FailurePolicy).To(HaveValue(Equal(admissionv1.Fail)))
			g.Expect(webhook.NamespaceSelector).To(Or(BeNil(), Equal(&metav1.LabelSelector{})))
			g.Expect(webhook.ObjectSelector).To(Or(BeNil(), Equal(&metav1.LabelSelector{})))
			g.Expect(webhook.MatchConditions).To(BeEmpty())
			g.Expect(webhook.Rules).To(ContainElement(SatisfyAll(
				HaveField("Operations", ContainElement(admissionv1.Create)),
				HaveField("APIGroups", ContainElement("mcp.x-k8s.io")),
				HaveField("APIVersions", ContainElement("v1alpha1")),
				HaveField("Resources", ContainElement("mcpservers")),
				HaveField("Scope", Or(BeNil(), HaveValue(BeElementOf(admissionv1.AllScopes, admissionv1.NamespacedScope)))),
			)))
		}, timeout, interval).Should(Succeed())

		By("Sending a dry-run MCPServer create request through admission")
		server := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "mcp.x-k8s.io/v1alpha1",
			"kind":       "MCPServer",
			"metadata": map[string]interface{}{
				"name":      "mcplmo-e2e-webhook",
				"namespace": operandNamespace,
			},
			"spec": map[string]interface{}{
				"source": map[string]interface{}{
					"type":           "ContainerImage",
					"containerImage": map[string]interface{}{"ref": "quay.io/containers/kubernetes_mcp_server:latest"},
				},
				"config": map[string]interface{}{"port": int64(8080)},
			},
		}}
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Create(ctx, server.DeepCopy(), client.DryRunAll)).To(Succeed())
		}, timeout, interval).Should(Succeed())
	})

	It("deploys the MCP Lifecycle Operator with a reachable metrics endpoint", func() {
		createManagedCR(ctx)
		waitForOperandReady(ctx)

		const metricsService = "mcp-lifecycle-operator-controller-manager-metrics-service"
		const metricsPort = int32(8443)

		By("Checking the metrics Service selects the operand manager Pod and not the module operator Pod")
		Eventually(func(g Gomega) {
			service := &corev1.Service{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: metricsService}, service)).To(Succeed())
			g.Expect(service.Spec.Selector).To(HaveKeyWithValue("app.kubernetes.io/name", "mcp-lifecycle-operator"))
			g.Expect(service.Spec.Selector).To(HaveKeyWithValue("control-plane", "controller-manager"))
			g.Expect(service.Spec.Ports).To(ContainElement(SatisfyAll(
				HaveField("Name", "https"),
				HaveField("Port", metricsPort),
			)))

			// The metrics Service must target the OPERAND manager Pod, not this module
			// operator's own Pod. Both carry control-plane=controller-manager and differ
			// only on app.kubernetes.io/name, so a selector that dropped the name key
			// would route /metrics scrapes to the wrong workload - the same class of
			// mis-route the webhook Service hit in PR 436. Assert the selector matches
			// the operand Pod template and explicitly does NOT match the module Pod.
			operand := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: operandDeployment}, operand)).To(Succeed())
			g.Expect(labels.SelectorFromSet(service.Spec.Selector).Matches(labels.Set(operand.Spec.Template.Labels))).To(BeTrue(),
				"metrics Service selector %v does not match operand Pod labels %v", service.Spec.Selector, operand.Spec.Template.Labels)

			module := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: moduleOperatorDeployment}, module)).To(Succeed())
			g.Expect(labels.SelectorFromSet(service.Spec.Selector).Matches(labels.Set(module.Spec.Template.Labels))).To(BeFalse(),
				"metrics Service selector %v matches module Pod labels %v", service.Spec.Selector, module.Spec.Template.Labels)
		}, timeout, interval).Should(Succeed())

		By("Verifying the metrics Service has an EndpointSlice backed by a ready operand Pod")
		// A populated EndpointSlice proves the Service resolves to a live, Ready operand
		// Pod: the endpoint controller only marks an address Ready once the Pod passes
		// its readiness probe. An empty slice (selector mismatch, or no ready Pod) is
		// the failure we want to catch, independent of any HTTP call below.
		Eventually(func(g Gomega) {
			endpointSlices := &discoveryv1.EndpointSliceList{}
			g.Expect(k8sClient.List(ctx, endpointSlices,
				client.InNamespace(operandNamespace),
				client.MatchingLabels{discoveryv1.LabelServiceName: metricsService},
			)).To(Succeed())

			readyAddresses := 0
			servesMetricsPort := false
			for _, slice := range endpointSlices.Items {
				for _, endpoint := range slice.Endpoints {
					// Ready is a *bool; nil conservatively means Ready per the API.
					if endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready {
						readyAddresses += len(endpoint.Addresses)
					}
				}
				for _, port := range slice.Ports {
					if port.Port != nil && *port.Port == metricsPort {
						servesMetricsPort = true
					}
				}
			}
			g.Expect(readyAddresses).To(BeNumerically(">=", 1), "metrics Service %q has no ready endpoints", metricsService)
			g.Expect(servesMetricsPort).To(BeTrue(), "metrics Service %q endpoints do not expose port %d", metricsService, metricsPort)
		}, timeout, interval).Should(Succeed())

		By("Hitting the metrics endpoint through the API server Service proxy")
		// The operand serves controller-runtime metrics over HTTPS with
		// authentication and authorization enabled. The API server Service proxy does
		// not forward the caller's bearer token to the backend, so a reachable metrics
		// server answers the unauthenticated scrape with 401, which still proves the
		// Service routes to a live endpoint. Through the proxy that 401 surfaces as a
		// client-go Unauthorized error with the status code left unset, so accept an
		// Unauthorized error (or an explicit 200/401 status) as proof a live metrics
		// server answered. A 403 is almost certainly the API server denying the
		// services/proxy subresource (RBAC) before the request ever reaches the
		// operand, and any other error or a status 0 (transport failure, or a 503 from
		// a Service with no backing endpoints) is likewise a failure; all are rejected
		// and the captured request error is surfaced for triage.
		clientset, err := kubernetes.NewForConfig(restConfig)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			result := clientset.CoreV1().RESTClient().Get().
				Namespace(operandNamespace).
				Resource("services").
				Name(fmt.Sprintf("https:%s:%d", metricsService, metricsPort)).
				SubResource("proxy").
				Suffix("metrics").
				Do(ctx)
			var statusCode int
			result.StatusCode(&statusCode)
			reqErr := result.Error()
			// A 401 proves reachability but surfaces as an Unauthorized error with
			// statusCode unset, so check both the status and the error. A Forbidden
			// error is deliberately not accepted: it means the API server denied the
			// proxy subresource rather than the operand answering.
			reachable := statusCode == http.StatusOK ||
				statusCode == http.StatusUnauthorized ||
				k8serr.IsUnauthorized(reqErr)
			g.Expect(reachable).To(BeTrue(),
				"metrics endpoint not reachable: HTTP %d via Service proxy (request err: %v)", statusCode, reqErr)
		}, timeout, interval).Should(Succeed())
	})

	It("should reject an MCPLifecycleOperator CR whose name is not default", func() {
		By("Creating the MCPLifecycleOperator CR with a non-default name")
		cr := &v1alpha1.MCPLifecycleOperator{
			ObjectMeta: metav1.ObjectMeta{
				Name: "not-default",
			},
			Spec: v1alpha1.MCPLifecycleOperatorSpec{
				ManagementSpec: platformcommon.ManagementSpec{
					ManagementState: platformcommon.Managed,
				},
			},
		}

		// Guard against a CEL regression: if the API server accepts the
		// non-default CR, ensure it is cleaned up so it does not leak into
		// later specs (AfterEach only deletes the "default" singleton).
		DeferCleanup(func() {
			err := k8sClient.Delete(ctx, cr)
			if err != nil && !k8serr.IsNotFound(err) {
				Fail("failed to delete non-default MCPLifecycleOperator CR: " + err.Error())
			}
		})

		By("Verifying the API server rejects the CR via the singleton CEL rule")
		err := k8sClient.Create(ctx, cr)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("must be default"))
	})

	It("should report the module release metadata in status.releases", func() {
		createManagedCR(ctx)
		waitForOperandReady(ctx)

		By("Verifying status.releases contains the module release metadata")
		Eventually(func(g Gomega) {
			cr := &v1alpha1.MCPLifecycleOperator{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: v1alpha1.MCPLifecycleOperatorInstanceName,
			}, cr)).To(Succeed())

			release := findRelease(cr.Status.Releases, v1alpha1.MCPLifecycleOperatorServiceName)
			g.Expect(release).NotTo(BeNil())
			g.Expect(release.RepoURL).To(Equal("https://github.com/opendatahub-io/mcp-lifecycle-module-operator"))
			g.Expect(release.Version).NotTo(BeEmpty())
		}, timeout, interval).Should(Succeed())
	})

	It("should garbage-collect the operand but keep namespace and module operator when CR is deleted", func() {
		createManagedCR(ctx)
		waitForOperandReady(ctx)

		By("Fetching the CR so its UID can anchor the ownerReference assertion")
		cr := &v1alpha1.MCPLifecycleOperator{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: v1alpha1.MCPLifecycleOperatorInstanceName,
		}, cr)).To(Succeed())

		By("Verifying the operand deployment carries a controller ownerReference to the CR")
		// The cluster-scoped CR legitimately owns the namespaced operand
		// Deployment; this controller ownerReference is what lets
		// kube-controller-manager garbage-collect the operand when the CR is
		// deleted. Poll, matching the convention of the other operand checks and
		// tolerating a reconcile that re-stamps the ownerReference.
		Eventually(func(g Gomega) {
			operand := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: operandNamespace,
				Name:      operandDeployment,
			}, operand)).To(Succeed())
			g.Expect(operand.OwnerReferences).To(ContainElement(SatisfyAll(
				HaveField("Kind", "MCPLifecycleOperator"),
				HaveField("UID", cr.UID),
				HaveField("Controller", HaveValue(BeTrue())),
			)))
		}, timeout, interval).Should(Succeed())

		By("Deleting the MCPLifecycleOperator CR")
		Expect(k8sClient.Delete(ctx, cr)).To(Succeed())

		By("Waiting for the CR to be gone")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name: v1alpha1.MCPLifecycleOperatorInstanceName,
			}, &v1alpha1.MCPLifecycleOperator{})
			g.Expect(k8serr.IsNotFound(err)).To(BeTrue())
		}, timeout, interval).Should(Succeed())

		By("Verifying the operand deployment is garbage-collected via its ownerReference")
		// Owner GC runs asynchronously in kube-controller-manager, so poll.
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, types.NamespacedName{
				Namespace: operandNamespace,
				Name:      operandDeployment,
			}, &appsv1.Deployment{})
			g.Expect(k8serr.IsNotFound(err)).To(BeTrue())
		}, timeout, interval).Should(Succeed())

		By("Verifying the namespace still exists")
		Consistently(func(g Gomega) {
			ns := &corev1.Namespace{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: operandNamespace}, ns)).To(Succeed())
		}, consistentDuration, consistentInterval).Should(Succeed())

		By("Verifying the module operator deployment is still available")
		Consistently(func(g Gomega) {
			dep := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: operandNamespace,
				Name:      moduleOperatorDeployment,
			}, dep)).To(Succeed())
			g.Expect(dep.Status.AvailableReplicas).To(BeNumerically(">=", int32(1)))
		}, consistentDuration, consistentInterval).Should(Succeed())
	})

	It("should keep namespace and module operator when ManagementState is set to Removed", func() {
		createManagedCR(ctx)
		waitForOperandReady(ctx)

		By("Setting ManagementState to Removed")
		cr := &v1alpha1.MCPLifecycleOperator{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: v1alpha1.MCPLifecycleOperatorInstanceName,
		}, cr)).To(Succeed())
		cr.Spec.ManagementState = platformcommon.Removed
		Expect(k8sClient.Update(ctx, cr)).To(Succeed())

		By("Waiting for the operand deployment to be removed")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, types.NamespacedName{
				Namespace: operandNamespace,
				Name:      operandDeployment,
			}, &appsv1.Deployment{})
			g.Expect(k8serr.IsNotFound(err)).To(BeTrue())
		}, timeout, interval).Should(Succeed())

		By("Verifying the namespace still exists")
		Consistently(func(g Gomega) {
			ns := &corev1.Namespace{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: operandNamespace}, ns)).To(Succeed())
		}, consistentDuration, consistentInterval).Should(Succeed())

		By("Verifying the module operator deployment is still available")
		Consistently(func(g Gomega) {
			dep := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: operandNamespace,
				Name:      moduleOperatorDeployment,
			}, dep)).To(Succeed())
			g.Expect(dep.Status.AvailableReplicas).To(BeNumerically(">=", int32(1)))
		}, consistentDuration, consistentInterval).Should(Succeed())
	})

	It("should update observedGeneration to match generation after a spec change", func() {
		createManagedCR(ctx)
		waitForOperandReady(ctx)

		By("Recording the current generation of the CR")
		cr := &v1alpha1.MCPLifecycleOperator{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: v1alpha1.MCPLifecycleOperatorInstanceName,
		}, cr)).To(Succeed())
		genBefore := cr.Generation

		By("Changing ManagementState to trigger a spec update")
		Eventually(func(g Gomega) {
			fresh := &v1alpha1.MCPLifecycleOperator{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: v1alpha1.MCPLifecycleOperatorInstanceName,
			}, fresh)).To(Succeed())
			fresh.Spec.ManagementState = platformcommon.Removed
			g.Expect(k8sClient.Update(ctx, fresh)).To(Succeed())
		}, timeout, interval).Should(Succeed())

		By("Verifying observedGeneration catches up to the new generation")
		Eventually(func(g Gomega) {
			updated := &v1alpha1.MCPLifecycleOperator{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: v1alpha1.MCPLifecycleOperatorInstanceName,
			}, updated)).To(Succeed())

			g.Expect(updated.Generation).To(BeNumerically(">", genBefore))
			g.Expect(updated.Status.Status.ObservedGeneration).To(Equal(updated.Generation))
		}, timeout, interval).Should(Succeed())
	})

	It("reports Ready=False during an operand outage and recovers", func() {
		createManagedCR(ctx)
		waitForOperandReady(ctx)

		By("Finding the operand Pod")
		pods := &corev1.PodList{}
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.List(ctx, pods,
				client.InNamespace(operandNamespace),
				client.MatchingLabels{"app.kubernetes.io/name": "mcp-lifecycle-operator", "control-plane": "controller-manager"},
			)).To(Succeed())
			g.Expect(pods.Items).To(HaveLen(1))
			g.Expect(pods.Items[0].DeletionTimestamp).To(BeNil())
		}, timeout, interval).Should(Succeed())
		pod := pods.Items[0].DeepCopy()
		Expect(pod.Spec.Containers).To(HaveLen(1))
		Expect(pod.Spec.Containers[0].Name).To(Equal("manager"))
		DeferCleanup(func() {
			err := k8sClient.Delete(ctx, pod)
			Expect(err == nil || k8serr.IsNotFound(err)).To(BeTrue(), "delete patched operand Pod: %v", err)
		})

		By("Making only the running operand Pod unpullable")
		original := pod.DeepCopy()
		pod.Spec.Containers[0].Image = "registry.invalid/mcplmo-e2e/unpullable:latest"
		Expect(k8sClient.Patch(ctx, pod, client.MergeFrom(original))).To(Succeed())

		By("Verifying the operator reports the unavailable operand")
		Eventually(func(g Gomega) {
			dep := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: operandDeployment}, dep)).To(Succeed())
			g.Expect(dep.Status.AvailableReplicas).To(Equal(int32(0)))

			cr := &v1alpha1.MCPLifecycleOperator{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: v1alpha1.MCPLifecycleOperatorInstanceName}, cr)).To(Succeed())
			g.Expect(cr.Status.Conditions).To(ContainElements(
				SatisfyAll(
					HaveField("Type", string(v1alpha1.ConditionMCPLifecycleOperatorAvailable)),
					HaveField("Status", metav1.ConditionFalse),
					HaveField("Reason", "DeploymentNotReady"),
				),
				SatisfyAll(
					HaveField("Type", string(platformcommon.ConditionTypeReady)),
					HaveField("Status", metav1.ConditionFalse),
					HaveField("Reason", "OperandDeploymentFailed"),
				),
				SatisfyAll(
					HaveField("Type", string(platformcommon.ConditionTypeDegraded)),
					HaveField("Status", metav1.ConditionFalse),
				),
			))
		}, timeout, interval).Should(Succeed())

		By("Deleting the patched Pod so the ReplicaSet creates a healthy replacement")
		Expect(k8sClient.Delete(ctx, pod)).To(Succeed())
		waitForOperandReady(ctx)
	})

	// Downstream bundle smoke test for the conversion webhook. Conversion
	// correctness (field mappings, round-trip, fuzz) is owned and tested upstream
	// in sigs.k8s.io/mcp-lifecycle-operator; here we only prove the webhook this
	// operator deploys is wired and reachable on the shipped bundle. The MCPServer
	// CRD stores v1beta1, so creating at v1alpha1 up-converts on write, and reading
	// back at v1alpha1 down-converts on read (the webhook is invoked for the
	// non-storage version). Reading at v1beta1 returns the stored object directly.
	It("serves a v1alpha1-created MCPServer at both versions via the conversion webhook", func() {
		createManagedCR(ctx)
		waitForOperandReady(ctx)

		// Unique per run so the AlreadyExists tolerance below can only ever mean
		// "this run's own retried Create persisted", never a leftover object from a
		// prior run masking a broken write conversion.
		serverName := fmt.Sprintf("mcplmo-e2e-conversion-%d", time.Now().UnixNano())
		const serverPort = int64(8080)

		// Assert the bundle actually declares a Webhook conversion strategy.
		// Without this, schema-identical fields would also survive a "None"
		// (pass-through) strategy, so the round-trip alone could not tell a wired
		// webhook from no conversion at all.
		By("Verifying the shipped MCPServer CRD declares a Webhook conversion strategy")
		Eventually(func(g Gomega) {
			crd := &extv1.CustomResourceDefinition{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: operandCRD}, crd)).To(Succeed())
			g.Expect(crd.Spec.Conversion).NotTo(BeNil())
			g.Expect(crd.Spec.Conversion.Strategy).To(Equal(extv1.WebhookConverter))
		}, timeout, interval).Should(Succeed())

		By("Creating an MCPServer at mcp.x-k8s.io/v1alpha1 (up-converts on write)")
		alpha := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "mcp.x-k8s.io/v1alpha1",
			"kind":       "MCPServer",
			"metadata": map[string]interface{}{
				"name":      serverName,
				"namespace": operandNamespace,
			},
			"spec": map[string]interface{}{
				"source": map[string]interface{}{
					"type":           "ContainerImage",
					"containerImage": map[string]interface{}{"ref": "quay.io/containers/kubernetes_mcp_server:latest"},
				},
				"config": map[string]interface{}{"port": serverPort},
			},
		}}
		// Register cleanup before the Create attempt so a Create that persists
		// server-side but then fails the retry loop cannot leak the object into the
		// shared namespace. Delete tolerates NotFound, so it is a no-op if the
		// Create never succeeded.
		DeferCleanup(func() {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "mcp.x-k8s.io", Version: "v1alpha1", Kind: "MCPServer"})
			obj.SetName(serverName)
			obj.SetNamespace(operandNamespace)
			err := k8sClient.Delete(ctx, obj)
			Expect(err == nil || k8serr.IsNotFound(err)).To(BeTrue(), "delete MCPServer: %v", err)
		})
		// Retry tolerates webhook warmup; treat AlreadyExists as success so a
		// Create that persisted server-side but returned a transient client error
		// does not wedge the retry loop until timeout.
		Eventually(func(g Gomega) {
			err := k8sClient.Create(ctx, alpha.DeepCopy())
			if k8serr.IsAlreadyExists(err) {
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
		}, timeout, interval).Should(Succeed())

		By("Reading it back at mcp.x-k8s.io/v1beta1 (the stored version)")
		beta := &unstructured.Unstructured{}
		beta.SetGroupVersionKind(schema.GroupVersionKind{Group: "mcp.x-k8s.io", Version: "v1beta1", Kind: "MCPServer"})
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: serverName}, beta)).To(Succeed())
			expectMCPServerSpec(g, beta, serverPort)
		}, timeout, interval).Should(Succeed())

		By("Reading it back at mcp.x-k8s.io/v1alpha1 (down-converts on read)")
		roundTrip := &unstructured.Unstructured{}
		roundTrip.SetGroupVersionKind(schema.GroupVersionKind{Group: "mcp.x-k8s.io", Version: "v1alpha1", Kind: "MCPServer"})
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: operandNamespace, Name: serverName}, roundTrip)).To(Succeed())
			expectMCPServerSpec(g, roundTrip, serverPort)
		}, timeout, interval).Should(Succeed())
	})
})

// expectMCPServerSpec asserts the shared spec fields survive conversion in either
// direction, so both the v1beta1 and v1alpha1 reads check source as well as port.
func expectMCPServerSpec(g Gomega, obj *unstructured.Unstructured, wantPort int64) {
	port, found, err := unstructured.NestedInt64(obj.Object, "spec", "config", "port")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(port).To(Equal(wantPort))

	sourceType, found, err := unstructured.NestedString(obj.Object, "spec", "source", "type")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(found).To(BeTrue())
	g.Expect(sourceType).To(Equal("ContainerImage"))
}

func createManagedCR(ctx context.Context) {
	By("Creating the MCPLifecycleOperator CR")
	cr := &v1alpha1.MCPLifecycleOperator{
		ObjectMeta: metav1.ObjectMeta{
			Name: v1alpha1.MCPLifecycleOperatorInstanceName,
		},
		Spec: v1alpha1.MCPLifecycleOperatorSpec{
			ManagementSpec: platformcommon.ManagementSpec{
				ManagementState: platformcommon.Managed,
			},
		},
	}
	Expect(k8sClient.Create(ctx, cr)).To(Succeed())
}

func waitForOperandReady(ctx context.Context) {
	By("Waiting for the operand namespace to be created")
	Eventually(func(g Gomega) {
		ns := &corev1.Namespace{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: operandNamespace}, ns)).To(Succeed())
	}, timeout, interval).Should(Succeed())

	By("Waiting for the operand CRD to be installed")
	Eventually(func(g Gomega) {
		crd := &extv1.CustomResourceDefinition{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: operandCRD}, crd)).To(Succeed())
	}, timeout, interval).Should(Succeed())

	By("Waiting for the operand deployment to become available")
	Eventually(func(g Gomega) {
		dep := &appsv1.Deployment{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: operandNamespace,
			Name:      operandDeployment,
		}, dep)).To(Succeed())

		desiredReplicas := int32(1)
		if dep.Spec.Replicas != nil {
			desiredReplicas = *dep.Spec.Replicas
		}
		g.Expect(dep.Status.AvailableReplicas).To(BeNumerically(">=", desiredReplicas))
	}, timeout, interval).Should(Succeed())

	By("Verifying the CR reaches Ready=True")
	Eventually(func(g Gomega) {
		updated := &v1alpha1.MCPLifecycleOperator{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: v1alpha1.MCPLifecycleOperatorInstanceName,
		}, updated)).To(Succeed())

		readyCondition := findCondition(updated.Status.Conditions, string(platformcommon.ConditionTypeReady))
		g.Expect(readyCondition).NotTo(BeNil())
		g.Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
	}, timeout, interval).Should(Succeed())
}

func findRelease(releases []platformcommon.ComponentRelease, name string) *platformcommon.ComponentRelease {
	for i := range releases {
		if releases[i].Name == name {
			return &releases[i]
		}
	}
	return nil
}

func findCondition(conditions []platformcommon.Condition, condType string) *platformcommon.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
