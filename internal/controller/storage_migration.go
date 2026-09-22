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
	"fmt"
	"strconv"
	"time"

	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	libconditions "github.com/opendatahub-io/odh-platform-utilities/pkg/controller/conditions"

	v1alpha1 "github.com/opendatahub-io/mcp-lifecycle-module-operator/api/v1alpha1"
)

const (
	// storageMigrationName is the fixed name of the StorageVersionMigration this
	// operator drives. It matches the vendored reference manifest so an operator
	// applying it manually and the reconciler converge on the same object.
	storageMigrationName = "mcpservers-v1beta1"

	reasonStorageMigrationPending   = "StorageMigrationPending"
	reasonStorageMigrationRunning   = "StorageMigrationRunning"
	reasonStorageMigrationSucceeded = "StorageMigrationSucceeded"
	reasonStorageMigrationFailed    = "StorageMigrationFailed"

	// Condition types the storage-version migrator sets on the
	// StorageVersionMigration's status.conditions (migration.k8s.io/v1alpha1).
	svmConditionSucceeded = "Succeeded"
	svmConditionFailed    = "Failed"

	// storageMigrationRequeueDelay is how often to re-check a pending, running,
	// or failed migration. It is deliberately slower than defaultRequeueDelay:
	// the migration is a background signal excluded from operand readiness, and
	// re-running the whole reconcile every few seconds while it progresses (or
	// indefinitely on a cluster without the migrator) is wasteful.
	storageMigrationRequeueDelay = time.Minute

	// storageMigrationPendingMessage is surfaced while the migration.k8s.io API
	// is not served, so the condition is actionable rather than a hard failure.
	storageMigrationPendingMessage = "The migration.k8s.io storage-version migrator API is not available yet; " +
		"install the storage-version migrator to migrate stored MCPServer objects to v1beta1"

	// storageMigrationAttemptAnnotation records how many times this operator has
	// created the migration. It is carried on the object and threaded through
	// each delete-and-recreate so a persistent failure cannot churn forever.
	storageMigrationAttemptAnnotation = "mcp.x-k8s.io/storage-migration-attempt"

	// maxStorageMigrationRetries bounds the delete-and-recreate cycle. A
	// permanently broken conversion webhook would otherwise recreate the
	// migration - and poll discovery - every storageMigrationRequeueDelay
	// indefinitely. Once the budget is spent we stop and ask for intervention.
	maxStorageMigrationRetries = 5
)

// storageVersionMigrationGVR is the cluster-scoped StorageVersionMigration
// resource. On OpenShift it is served by the openshift-kube-storage-version-
// migrator operator; where absent, the migration reports Pending rather than
// failing. The type is not in the manager scheme (nor vendored), so it is
// handled via the dynamic client as unstructured.
var storageVersionMigrationGVR = schema.GroupVersionResource{
	Group:    "migration.k8s.io",
	Version:  "v1alpha1",
	Resource: "storageversionmigrations",
}

// reconcileStorageMigration drives and observes the storage-version migration of
// stored MCPServer objects to v1beta1. It creates a StorageVersionMigration (at
// most once) and maps its outcome onto the standalone MCPServerStorageMigrated
// condition. It never returns an error and never touches the operand-availability
// conditions or re-runs AggregateReady: a pending, running, or failed migration
// only sets its own condition and schedules a requeue. The returned Result has
// RequeueAfter set while the migration is pending/running/failed and zero once it
// has succeeded.
func (r *MCPLifecycleOperatorReconciler) reconcileStorageMigration(
	ctx context.Context,
	cr *v1alpha1.MCPLifecycleOperator,
	cm *v1alpha1.ConditionsManager,
) ctrl.Result {
	log := logf.FromContext(ctx)

	// C1: already succeeded - steady state, skip all migration.k8s.io API calls.
	if storageMigrationSettled(cr) {
		return ctrl.Result{}
	}

	// C2: the migration.k8s.io API is not served (migrator absent). The dynamic
	// client issues raw REST calls with no RESTMapper or scheme, so an absent
	// group surfaces as NotFound, not a no-match error; discovery is the reliable
	// way to detect availability without misclassifying it as "object absent".
	if !r.storageMigrationAPIServed() {
		cm.MarkFalse(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationPending,
			storageMigrationPendingMessage)

		return ctrl.Result{RequeueAfter: storageMigrationRequeueDelay}
	}

	obj, err := r.DynamicClient.Resource(storageVersionMigrationGVR).Get(ctx, storageMigrationName, metav1.GetOptions{})
	if err != nil {
		if k8serr.IsNotFound(err) {
			return r.createStorageMigration(ctx, cr, cm, 1)
		}

		// C7: unexpected error - actionable, self-heals on requeue.
		cm.MarkFalse(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationFailed, err.Error())

		return ctrl.Result{RequeueAfter: storageMigrationRequeueDelay}
	}

	log.V(1).Info("Observing storage-version migration", "name", storageMigrationName)

	return r.observeStorageMigration(ctx, cr, cm, obj)
}

// storageMigrationAPIServed reports whether the cluster serves the
// migration.k8s.io/v1alpha1 storageversionmigrations resource. On OpenShift it
// is provided by the kube-storage-version-migrator; where absent, discovery
// omits it (or errors), and the caller reports Pending rather than failing.
func (r *MCPLifecycleOperatorReconciler) storageMigrationAPIServed() bool {
	list, err := r.DiscoveryClient.ServerResourcesForGroupVersion(storageVersionMigrationGVR.GroupVersion().String())
	if err != nil {
		return false
	}

	for _, res := range list.APIResources {
		if res.Name == storageVersionMigrationGVR.Resource {
			return true
		}
	}

	return false
}

// createStorageMigration builds and creates the StorageVersionMigration. It is
// reached when the object is absent (attempt 1, C3) and again when a failed
// migration is recreated to retry (attempt+1); it never updates or patches an
// existing migration. The attempt number is stamped on the object so the retry
// budget survives the delete-and-recreate. The object is owned by the
// cluster-scoped MCPLifecycleOperator CR so it is garbage-collected on uninstall.
func (r *MCPLifecycleOperatorReconciler) createStorageMigration(
	ctx context.Context,
	cr *v1alpha1.MCPLifecycleOperator,
	cm *v1alpha1.ConditionsManager,
	attempt int,
) ctrl.Result {
	migration := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": storageVersionMigrationGVR.Group + "/" + storageVersionMigrationGVR.Version,
		"kind":       "StorageVersionMigration",
		"metadata": map[string]interface{}{
			"name": storageMigrationName,
			"annotations": map[string]interface{}{
				storageMigrationAttemptAnnotation: strconv.Itoa(attempt),
			},
			"labels": map[string]interface{}{
				"app.kubernetes.io/name":      "mcp-lifecycle-operator",
				"app.kubernetes.io/component": "storage-version-migration",
			},
			"ownerReferences": []interface{}{
				map[string]interface{}{
					"apiVersion":         v1alpha1.GroupVersion.String(),
					"kind":               "MCPLifecycleOperator",
					"name":               cr.Name,
					"uid":                string(cr.UID),
					"controller":         true,
					"blockOwnerDeletion": true,
				},
			},
		},
		"spec": map[string]interface{}{
			"resource": map[string]interface{}{
				"group":    "mcp.x-k8s.io",
				"version":  "v1beta1",
				"resource": "mcpservers",
			},
		},
	}}

	if _, err := r.DynamicClient.Resource(storageVersionMigrationGVR).
		Create(ctx, migration, metav1.CreateOptions{}); err != nil {
		if k8serr.IsAlreadyExists(err) {
			// A concurrent create won the race; treat as running and re-observe.
			cm.MarkFalse(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationRunning,
				"StorageVersionMigration already exists; awaiting completion")
		} else {
			cm.MarkFalse(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationFailed, err.Error())
		}

		return ctrl.Result{RequeueAfter: storageMigrationRequeueDelay}
	}

	cm.MarkFalse(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationRunning,
		"Created StorageVersionMigration; awaiting completion")

	return ctrl.Result{RequeueAfter: storageMigrationRequeueDelay}
}

// observeStorageMigration maps the migrator's status.conditions onto our
// condition: Succeeded=True -> True/Succeeded (C5, no requeue, covers the
// vacuous zero-object case); Failed=True -> delete the terminal migration and
// recreate it to self-heal (C6), up to maxStorageMigrationRetries; otherwise
// still Running (C4). Succeeded is the only branch that asserts the completion
// signal (no false-complete).
func (r *MCPLifecycleOperatorReconciler) observeStorageMigration(
	ctx context.Context,
	cr *v1alpha1.MCPLifecycleOperator,
	cm *v1alpha1.ConditionsManager,
	obj *unstructured.Unstructured,
) ctrl.Result {
	if migrationConditionTrue(obj, svmConditionSucceeded) {
		cm.MarkTrueWithReason(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationSucceeded)

		return ctrl.Result{}
	}

	if migrationConditionTrue(obj, svmConditionFailed) {
		msg := migrationConditionMessage(obj, svmConditionFailed)
		attempt := storageMigrationAttempt(obj)

		// Stop the delete-and-recreate cycle once a persistent failure (e.g. a
		// permanently broken conversion webhook) has spent the retry budget, so
		// we neither churn the migration nor poll discovery indefinitely. Leave
		// the terminal object in place as evidence and return no requeue.
		if attempt >= maxStorageMigrationRetries {
			cm.MarkFalse(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationFailed,
				fmt.Sprintf("%s; giving up after %d attempts, manual intervention required", msg, attempt))

			return ctrl.Result{}
		}

		// A StorageVersionMigration is one-shot and terminal once Failed; the
		// migrator will not retry it. Delete the object and recreate a fresh
		// migration to self-heal after a transient failure (e.g. a conversion-
		// webhook outage), carrying the incremented attempt so the budget holds.
		if err := r.deleteStorageMigration(ctx); err != nil && !k8serr.IsNotFound(err) {
			cm.MarkFalse(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationFailed,
				fmt.Sprintf("%s; failed to delete the migration to retry: %v", msg, err))

			return ctrl.Result{RequeueAfter: storageMigrationRequeueDelay}
		}

		return r.createStorageMigration(ctx, cr, cm, attempt+1)
	}

	cm.MarkFalse(v1alpha1.ConditionMCPServerStorageMigrated, reasonStorageMigrationRunning,
		"StorageVersionMigration in progress; awaiting completion")

	return ctrl.Result{RequeueAfter: storageMigrationRequeueDelay}
}

// deleteStorageMigration removes the StorageVersionMigration so the next
// reconcile can recreate a fresh one.
func (r *MCPLifecycleOperatorReconciler) deleteStorageMigration(ctx context.Context) error {
	return r.DynamicClient.Resource(storageVersionMigrationGVR).
		Delete(ctx, storageMigrationName, metav1.DeleteOptions{})
}

// storageMigrationAttempt reads the attempt counter this operator stamped on the
// migration. A migration created outside this operator (or by an older version)
// carries no annotation and reads as 0, so it gets the full retry budget.
func storageMigrationAttempt(obj *unstructured.Unstructured) int {
	ann := obj.GetAnnotations()
	if ann == nil {
		return 0
	}

	n, err := strconv.Atoi(ann[storageMigrationAttemptAnnotation])
	if err != nil {
		return 0
	}

	return n
}

// storageMigrationSettled reports whether the MCPServerStorageMigrated condition
// is already True, so steady-state reconciles skip all migration.k8s.io API calls.
func storageMigrationSettled(cr *v1alpha1.MCPLifecycleOperator) bool {
	return storageMigrationSucceeded(cr)
}

// storageMigrationSucceeded reports whether the completion signal is asserted
// (MCPServerStorageMigrated == True). This is the durable signal the later
// (out-of-scope, upstream) v1alpha1-removal step gates on: it must proceed only
// when this is True. The condition is set True exclusively in the Succeeded
// branch of observeStorageMigration, so it can never report complete while the
// migration is pending, running, or failed.
func storageMigrationSucceeded(cr *v1alpha1.MCPLifecycleOperator) bool {
	c := libconditions.FindStatusCondition(cr, v1alpha1.ConditionMCPServerStorageMigrated)

	return c != nil && c.Status == metav1.ConditionTrue
}

// migrationConditionTrue reports whether the migrator's status.conditions holds
// a condition of the given type with status "True".
func migrationConditionTrue(obj *unstructured.Unstructured, condType string) bool {
	c := findMigrationCondition(obj, condType)

	return c != nil && c["status"] == string(metav1.ConditionTrue)
}

// migrationConditionMessage returns the message of the named migrator condition,
// or a fallback when absent.
func migrationConditionMessage(obj *unstructured.Unstructured, condType string) string {
	c := findMigrationCondition(obj, condType)
	if c != nil {
		if msg, ok := c["message"].(string); ok && msg != "" {
			return msg
		}
	}

	return fmt.Sprintf("StorageVersionMigration reported %s", condType)
}

func findMigrationCondition(obj *unstructured.Unstructured, condType string) map[string]interface{} {
	conds, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return nil
	}

	for _, raw := range conds {
		c, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if c["type"] == condType {
			return c
		}
	}

	return nil
}
