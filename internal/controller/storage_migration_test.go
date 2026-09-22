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

package controller

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	platformcommon "github.com/opendatahub-io/odh-platform-utilities/api/common"

	v1alpha1 "github.com/opendatahub-io/mcp-lifecycle-module-operator/api/v1alpha1"
)

// storageMigrationListKinds maps the StorageVersionMigration GVR to its list
// kind so the fake dynamic client can serve get/create/delete for the
// unstructured migration.k8s.io type, which is absent from testScheme.
var storageMigrationListKinds = map[schema.GroupVersionResource]string{
	storageVersionMigrationGVR: "StorageVersionMigrationList",
}

func newStorageMigrationDynamicClient(objs ...runtime.Object) *fakedynamic.FakeDynamicClient {
	return fakedynamic.NewSimpleDynamicClientWithCustomListKinds(testScheme, storageMigrationListKinds, objs...)
}

// newStorageMigrationDiscovery returns a discovery client that either serves the
// migration.k8s.io/v1alpha1 storageversionmigrations resource (served) or does
// not (the migrator-absent case).
func newStorageMigrationDiscovery(served bool) discovery.DiscoveryInterface {
	cs := kubefake.NewSimpleClientset()
	fd, _ := cs.Discovery().(*fakediscovery.FakeDiscovery)
	if served {
		fd.Resources = []*metav1.APIResourceList{
			{
				GroupVersion: storageVersionMigrationGVR.GroupVersion().String(),
				APIResources: []metav1.APIResource{
					{Name: storageVersionMigrationGVR.Resource, Namespaced: false, Kind: "StorageVersionMigration"},
				},
			},
		}
	}
	return cs.Discovery()
}

// newStorageMigrationReconciler builds a reconciler wired with only the fields
// reconcileStorageMigration touches (the dynamic and discovery clients).
func newStorageMigrationReconciler(dyn *fakedynamic.FakeDynamicClient, served bool) *MCPLifecycleOperatorReconciler {
	return &MCPLifecycleOperatorReconciler{
		Scheme:          testScheme,
		DynamicClient:   dyn,
		DiscoveryClient: newStorageMigrationDiscovery(served),
		OperatorVersion: testOperatorVersion,
		PodNamespace:    testPodNamespace,
	}
}

// svmCondition builds one entry for a StorageVersionMigration status.conditions
// list. An empty message is omitted.
func svmCondition(condType, status, message string) map[string]interface{} {
	c := map[string]interface{}{
		"type":   condType,
		"status": status,
	}
	if message != "" {
		c["message"] = message
	}
	return c
}

// newSVM builds an unstructured StorageVersionMigration named
// storageMigrationName carrying the given status.conditions.
func newSVM(conditions ...map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storageVersionMigrationGVR.Group + "/" + storageVersionMigrationGVR.Version,
		"kind":       "StorageVersionMigration",
		"metadata": map[string]interface{}{
			"name": storageMigrationName,
		},
		"spec": map[string]interface{}{
			"resource": map[string]interface{}{
				"group":    "mcp.x-k8s.io",
				"version":  "v1beta1",
				"resource": "mcpservers",
			},
		},
	}}

	if len(conditions) > 0 {
		conds := make([]interface{}, 0, len(conditions))
		for _, c := range conditions {
			conds = append(conds, c)
		}
		if err := unstructured.SetNestedSlice(obj.Object, conds, "status", "conditions"); err != nil {
			panic(err)
		}
	}

	return obj
}

// withStorageMigrationAttempt stamps the attempt annotation this operator uses
// to bound the delete-and-recreate retry budget onto an SVM.
func withStorageMigrationAttempt(obj *unstructured.Unstructured, attempt int) *unstructured.Unstructured {
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[storageMigrationAttemptAnnotation] = strconv.Itoa(attempt)
	obj.SetAnnotations(ann)
	return obj
}

func countStorageMigrationVerb(dyn *fakedynamic.FakeDynamicClient, verb string) int {
	n := 0
	for _, a := range dyn.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == "storageversionmigrations" {
			n++
		}
	}
	return n
}

func storageMigrationExists(t *testing.T, dyn *fakedynamic.FakeDynamicClient) bool {
	t.Helper()
	_, err := dyn.Resource(storageVersionMigrationGVR).Get(context.Background(), storageMigrationName, metav1.GetOptions{})
	if err == nil {
		return true
	}
	if k8serr.IsNotFound(err) {
		return false
	}
	t.Fatalf("unexpected error getting migration: %v", err)
	return false
}

// --- API-availability probe (finding #1) ---

func TestStorageMigrationAPIServed(t *testing.T) {
	t.Run("served when resource present", func(t *testing.T) {
		r := newStorageMigrationReconciler(newStorageMigrationDynamicClient(), true)
		if !r.storageMigrationAPIServed() {
			t.Error("expected API to be reported as served")
		}
	})
	t.Run("not served when group absent", func(t *testing.T) {
		r := newStorageMigrationReconciler(newStorageMigrationDynamicClient(), false)
		if r.storageMigrationAPIServed() {
			t.Error("expected API to be reported as absent")
		}
	})
}

// --- US1: happy-path drive-and-observe (contract rows C1, C3, C4, C5) ---

// C1: an already-succeeded migration is steady state; no migration.k8s.io API
// call is made.
func TestReconcileStorageMigration_SettledSkipsAPICalls(t *testing.T) {
	dyn := newStorageMigrationDynamicClient()
	dyn.PrependReactor("*", "storageversionmigrations", func(clienttesting.Action) (bool, runtime.Object, error) {
		t.Fatal("no migration.k8s.io API call expected once the migration has succeeded")
		return true, nil, nil
	})

	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)
	cm.MarkTrueWithReason(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationSucceeded)

	r := newStorageMigrationReconciler(dyn, true)
	res := r.reconcileStorageMigration(context.Background(), cr, cm)

	if res.RequeueAfter != 0 {
		t.Errorf("settled migration must not requeue, got RequeueAfter=%s", res.RequeueAfter)
	}
}

// C3: the migration is absent -> create it (owned by the CR) and report Running
// with a requeue.
func TestReconcileStorageMigration_CreatesWhenAbsent(t *testing.T) {
	dyn := newStorageMigrationDynamicClient()
	cr := newTestCR()
	cr.UID = "cr-uid-123"
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)

	r := newStorageMigrationReconciler(dyn, true)
	res := r.reconcileStorageMigration(context.Background(), cr, cm)

	if res.RequeueAfter != storageMigrationRequeueDelay {
		t.Errorf("expected requeue after %s, got %s", storageMigrationRequeueDelay, res.RequeueAfter)
	}
	assertMigrationCondition(t, cr, metav1.ConditionFalse, reasonStorageMigrationRunning)

	got, err := dyn.Resource(storageVersionMigrationGVR).Get(context.Background(), storageMigrationName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected StorageVersionMigration to have been created: %v", err)
	}
	spec, _, _ := unstructured.NestedMap(got.Object, "spec", "resource")
	if spec["group"] != "mcp.x-k8s.io" || spec["version"] != "v1beta1" || spec["resource"] != "mcpservers" {
		t.Errorf("unexpected spec.resource: %v", spec)
	}

	owners := got.GetOwnerReferences()
	if len(owners) != 1 || owners[0].Kind != "MCPLifecycleOperator" || owners[0].Name != cr.Name || string(owners[0].UID) != "cr-uid-123" {
		t.Errorf("expected an owner reference to the CR, got %v", owners)
	}
}

// C3 (idempotency): a second reconcile does not create a duplicate migration.
func TestReconcileStorageMigration_CreateIsIdempotent(t *testing.T) {
	dyn := newStorageMigrationDynamicClient()
	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)
	r := newStorageMigrationReconciler(dyn, true)

	r.reconcileStorageMigration(context.Background(), cr, cm)
	r.reconcileStorageMigration(context.Background(), cr, cm)

	if got := countStorageMigrationVerb(dyn, "create"); got != 1 {
		t.Errorf("expected exactly one create across two reconciles, got %d", got)
	}
	// Second reconcile observed the existing (condition-less) migration.
	assertMigrationCondition(t, cr, metav1.ConditionFalse, reasonStorageMigrationRunning)
}

// C4: the migration exists but has not reported a terminal condition -> Running.
func TestReconcileStorageMigration_RunningWhileInProgress(t *testing.T) {
	dyn := newStorageMigrationDynamicClient(newSVM())
	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)

	r := newStorageMigrationReconciler(dyn, true)
	res := r.reconcileStorageMigration(context.Background(), cr, cm)

	if res.RequeueAfter != storageMigrationRequeueDelay {
		t.Errorf("expected requeue after %s, got %s", storageMigrationRequeueDelay, res.RequeueAfter)
	}
	assertMigrationCondition(t, cr, metav1.ConditionFalse, reasonStorageMigrationRunning)
	if got := countStorageMigrationVerb(dyn, "create"); got != 0 {
		t.Errorf("must not create when the migration already exists, got %d creates", got)
	}
}

// C5: the migrator reports Succeeded=True -> mark complete, no requeue.
func TestReconcileStorageMigration_SucceededMarksComplete(t *testing.T) {
	dyn := newStorageMigrationDynamicClient(newSVM(svmCondition(svmConditionSucceeded, "True", "")))
	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)

	r := newStorageMigrationReconciler(dyn, true)
	res := r.reconcileStorageMigration(context.Background(), cr, cm)

	if res.RequeueAfter != 0 {
		t.Errorf("succeeded migration must not requeue, got %s", res.RequeueAfter)
	}
	assertMigrationCondition(t, cr, metav1.ConditionTrue, reasonStorageMigrationSucceeded)
}

// C5 (vacuous): a cluster with zero stored MCPServers still reports Succeeded;
// there is no distinct "nothing to migrate" branch.
func TestReconcileStorageMigration_VacuousSuccess(t *testing.T) {
	dyn := newStorageMigrationDynamicClient(newSVM(svmCondition(svmConditionSucceeded, "True", "")))
	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)

	r := newStorageMigrationReconciler(dyn, true)
	r.reconcileStorageMigration(context.Background(), cr, cm)

	if !storageMigrationSucceeded(cr) {
		t.Error("vacuous (zero-object) success must still assert the completion signal")
	}
}

// --- US2: environment-dependent failure classification (rows C2, C6, C7) ---

// C2: the migration.k8s.io API is not served (migrator absent) -> Pending, and
// no dynamic client call is attempted.
func TestReconcileStorageMigration_PendingWhenAPIAbsent(t *testing.T) {
	dyn := newStorageMigrationDynamicClient()
	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)

	r := newStorageMigrationReconciler(dyn, false)
	res := r.reconcileStorageMigration(context.Background(), cr, cm)

	if res.RequeueAfter != storageMigrationRequeueDelay {
		t.Errorf("expected requeue after %s, got %s", storageMigrationRequeueDelay, res.RequeueAfter)
	}
	assertMigrationCondition(t, cr, metav1.ConditionFalse, reasonStorageMigrationPending)
	if len(dyn.Actions()) != 0 {
		t.Errorf("must not touch the dynamic client when the API is absent, got %v", dyn.Actions())
	}
}

// C6: the migrator reports Failed=True -> delete the terminal migration and
// recreate a fresh one in the same reconcile (self-heal), within the budget.
func TestReconcileStorageMigration_FailedRecreatesForRetry(t *testing.T) {
	const msg = "conversion webhook for mcpservers.mcp.x-k8s.io refused connection"
	dyn := newStorageMigrationDynamicClient(newSVM(svmCondition(svmConditionFailed, "True", msg)))
	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)

	r := newStorageMigrationReconciler(dyn, true)
	res := r.reconcileStorageMigration(context.Background(), cr, cm)

	if res.RequeueAfter != storageMigrationRequeueDelay {
		t.Errorf("expected requeue after %s, got %s", storageMigrationRequeueDelay, res.RequeueAfter)
	}
	// The terminal migration is deleted and a fresh one recreated in the same
	// reconcile, so it is Running again and still present.
	if got := countStorageMigrationVerb(dyn, "delete"); got != 1 {
		t.Errorf("expected the failed migration to be deleted once, got %d deletes", got)
	}
	if got := countStorageMigrationVerb(dyn, "create"); got != 1 {
		t.Errorf("expected a fresh migration to be recreated once, got %d creates", got)
	}
	assertMigrationCondition(t, cr, metav1.ConditionFalse, reasonStorageMigrationRunning)
	if !storageMigrationExists(t, dyn) {
		t.Error("expected a fresh migration to have been recreated")
	}
	// The recreated migration carries the incremented attempt so the budget holds.
	obj, err := dyn.Resource(storageVersionMigrationGVR).Get(context.Background(), storageMigrationName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting recreated migration: %v", err)
	}
	if got := storageMigrationAttempt(obj); got != 1 {
		t.Errorf("expected recreated migration to carry attempt 1, got %d", got)
	}
}

// C6 (bound): once the retry budget is spent, a persistently failing migration
// is left in place and surfaced as needing manual intervention - no more churn.
func TestReconcileStorageMigration_GivesUpAfterMaxRetries(t *testing.T) {
	const msg = "conversion webhook permanently unavailable"
	failed := withStorageMigrationAttempt(newSVM(svmCondition(svmConditionFailed, "True", msg)), maxStorageMigrationRetries)
	dyn := newStorageMigrationDynamicClient(failed)
	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)

	r := newStorageMigrationReconciler(dyn, true)
	res := r.reconcileStorageMigration(context.Background(), cr, cm)

	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue once the retry budget is spent, got %s", res.RequeueAfter)
	}
	if got := countStorageMigrationVerb(dyn, "delete"); got != 0 {
		t.Errorf("expected no delete after giving up, got %d", got)
	}
	if got := countStorageMigrationVerb(dyn, "create"); got != 0 {
		t.Errorf("expected no recreate after giving up, got %d", got)
	}
	c := assertMigrationCondition(t, cr, metav1.ConditionFalse, reasonStorageMigrationFailed)
	if !strings.Contains(c.Message, msg) || !strings.Contains(c.Message, "manual intervention") {
		t.Errorf("expected the migrator message and a manual-intervention note in %q", c.Message)
	}
	if !storageMigrationExists(t, dyn) {
		t.Error("expected the terminal migration to be left in place as evidence")
	}
}

// C7: an unexpected API error -> Failed, self-heals on the next requeue.
func TestReconcileStorageMigration_UnexpectedErrorFails(t *testing.T) {
	dyn := newStorageMigrationDynamicClient()
	dyn.PrependReactor("get", "storageversionmigrations", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("etcdserver: request timed out")
	})

	cr := newTestCR()
	cm := v1alpha1.NewConditionsManager(cr, cr.Generation)

	r := newStorageMigrationReconciler(dyn, true)
	res := r.reconcileStorageMigration(context.Background(), cr, cm)

	if res.RequeueAfter != storageMigrationRequeueDelay {
		t.Errorf("expected requeue after %s, got %s", storageMigrationRequeueDelay, res.RequeueAfter)
	}
	c := assertMigrationCondition(t, cr, metav1.ConditionFalse, reasonStorageMigrationFailed)
	if c.Message != "etcdserver: request timed out" {
		t.Errorf("expected the raw error as message, got %q", c.Message)
	}
}

// --- US3: completion signal + readiness independence (FR-007, FR-008) ---

// The completion signal is asserted only when the migration has succeeded, so
// the (out-of-scope) v1alpha1-removal step can never proceed prematurely.
func TestStorageMigrationSucceeded_OnlyOnSuccess(t *testing.T) {
	cases := []struct {
		name  string
		apply func(cm *v1alpha1.ConditionsManager)
		want  bool
	}{
		{"absent", func(*v1alpha1.ConditionsManager) {}, false},
		{"pending", func(cm *v1alpha1.ConditionsManager) {
			cm.MarkFalse(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationPending, "x")
		}, false},
		{"running", func(cm *v1alpha1.ConditionsManager) {
			cm.MarkFalse(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationRunning, "x")
		}, false},
		{"failed", func(cm *v1alpha1.ConditionsManager) {
			cm.MarkFalse(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationFailed, "x")
		}, false},
		{"succeeded", func(cm *v1alpha1.ConditionsManager) {
			cm.MarkTrueWithReason(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationSucceeded)
		}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := newTestCR()
			cm := v1alpha1.NewConditionsManager(cr, cr.Generation)
			tc.apply(cm)

			if got := storageMigrationSucceeded(cr); got != tc.want {
				t.Errorf("storageMigrationSucceeded() = %v, want %v", got, tc.want)
			}
			if got := storageMigrationSettled(cr); got != tc.want {
				t.Errorf("storageMigrationSettled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The migration condition is excluded from AggregateReady: no migration state
// changes Ready / Degraded / ProvisioningSucceeded once the operand is
// available.
func TestStorageMigration_ReadinessIndependence(t *testing.T) {
	type readiness struct{ ready, degraded, provisioned *platformcommon.Condition }

	aggregate := func(apply func(cm *v1alpha1.ConditionsManager)) readiness {
		cr := newTestCR()
		cm := v1alpha1.NewConditionsManager(cr, cr.Generation)
		cm.MarkTrue(v1alpha1.ConditionMCPLifecycleOperatorAvailable)
		apply(cm)
		cm.AggregateReady()

		return readiness{
			ready:       findCondition(cr, string(platformcommon.ConditionTypeReady)),
			degraded:    findCondition(cr, string(platformcommon.ConditionTypeDegraded)),
			provisioned: findCondition(cr, string(platformcommon.ConditionTypeProvisioningSucceeded)),
		}
	}

	baseline := aggregate(func(*v1alpha1.ConditionsManager) {})

	states := map[string]func(cm *v1alpha1.ConditionsManager){
		"succeeded": func(cm *v1alpha1.ConditionsManager) {
			cm.MarkTrueWithReason(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationSucceeded)
		},
		"pending": func(cm *v1alpha1.ConditionsManager) {
			cm.MarkFalse(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationPending, "x")
		},
		"running": func(cm *v1alpha1.ConditionsManager) {
			cm.MarkFalse(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationRunning, "x")
		},
		"failed": func(cm *v1alpha1.ConditionsManager) {
			cm.MarkFalse(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationFailed, "x")
		},
	}

	for name, apply := range states {
		t.Run(name, func(t *testing.T) {
			got := aggregate(apply)
			assertSameStatus(t, "Ready", baseline.ready, got.ready)
			assertSameStatus(t, "Degraded", baseline.degraded, got.degraded)
			assertSameStatus(t, "ProvisioningSucceeded", baseline.provisioned, got.provisioned)
		})
	}
}

func assertMigrationCondition(t *testing.T, cr *v1alpha1.MCPLifecycleOperator, status metav1.ConditionStatus, reason string) *platformcommon.Condition {
	t.Helper()
	c := findCondition(cr, v1alpha1.ConditionMCPServerStorageMigrated)
	if c == nil {
		t.Fatalf("condition %s not set", v1alpha1.ConditionMCPServerStorageMigrated)
	}
	if c.Status != status {
		t.Errorf("condition status = %s, want %s", c.Status, status)
	}
	if c.Reason != reason {
		t.Errorf("condition reason = %s, want %s", c.Reason, reason)
	}
	return c
}

func assertSameStatus(t *testing.T, label string, want, got *platformcommon.Condition) {
	t.Helper()
	if want == nil || got == nil {
		if want != got {
			t.Errorf("%s presence differs: baseline=%v migration-state=%v", label, want, got)
		}
		return
	}
	if want.Status != got.Status {
		t.Errorf("%s status changed with migration state: baseline=%s, got=%s", label, want.Status, got.Status)
	}
}
