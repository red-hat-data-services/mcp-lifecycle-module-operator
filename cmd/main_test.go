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

package main

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStripCRDSchemaDropsSchemaAndManagedFields(t *testing.T) {
	crd := &extv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "gateways.gateway.networking.k8s.io",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kube-apiserver"}},
		},
		Spec: extv1.CustomResourceDefinitionSpec{
			Versions: []extv1.CustomResourceDefinitionVersion{
				{Name: "v1", Schema: &extv1.CustomResourceValidation{
					OpenAPIV3Schema: &extv1.JSONSchemaProps{Type: "object"},
				}},
				{Name: "v1beta1", Schema: &extv1.CustomResourceValidation{
					OpenAPIV3Schema: &extv1.JSONSchemaProps{Type: "object"},
				}},
			},
		},
	}

	out, err := stripCRDSchema(crd)
	if err != nil {
		t.Fatalf("stripCRDSchema returned error: %v", err)
	}

	got, ok := out.(*extv1.CustomResourceDefinition)
	if !ok {
		t.Fatalf("expected *CustomResourceDefinition, got %T", out)
	}

	if got.Name != "gateways.gateway.networking.k8s.io" {
		t.Errorf("name must be preserved, got %q", got.Name)
	}

	for i, v := range got.Spec.Versions {
		if v.Schema != nil {
			t.Errorf("version[%d] (%s) schema not stripped: %+v", i, v.Name, v.Schema)
		}
	}

	if got.ManagedFields != nil {
		t.Errorf("managed fields not stripped: %+v", got.ManagedFields)
	}
}

func TestStripCRDSchemaPassesThroughNonCRD(t *testing.T) {
	// A per-object Transform is only registered for CRDs, but defend against a
	// non-CRD object reaching it: it must not panic and must return the object.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "operand",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kube-apiserver"}},
		},
	}

	out, err := stripCRDSchema(deploy)
	if err != nil {
		t.Fatalf("stripCRDSchema returned error: %v", err)
	}

	got, ok := out.(*appsv1.Deployment)
	if !ok {
		t.Fatalf("expected *Deployment, got %T", out)
	}

	// TransformStripManagedFields still applies to non-CRD objects.
	if got.ManagedFields != nil {
		t.Errorf("managed fields not stripped: %+v", got.ManagedFields)
	}
}
