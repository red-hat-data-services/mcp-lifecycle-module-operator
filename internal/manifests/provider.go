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

package manifests

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Params holds the dynamic values used to patch vendored operand manifests.
type Params struct {
	OperandNamespace string
	OperandImage     string
	TLSMinVersion    string
	TLSCipherSuites  string
}

// Provider abstracts how operand manifests are obtained and transformed.
type Provider interface {
	Manifests(ctx context.Context, params Params) ([]unstructured.Unstructured, error)
}
