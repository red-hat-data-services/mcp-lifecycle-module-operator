package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	configv1 "github.com/openshift/api/config/v1"
	utiltls "github.com/openshift/controller-runtime-common/pkg/tls"
	libgocrypto "github.com/openshift/library-go/pkg/crypto"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

func fetchTLSConfig(ctx context.Context, cl client.Client) (minVersion string, cipherSuites string, err error) {
	log := logf.FromContext(ctx)

	spec, err := utiltls.FetchAPIServerTLSProfile(ctx, cl)
	if err != nil {
		if meta.IsNoMatchError(err) || isNotRegisteredError(err) {
			log.Info("TLS profile not available (non-OpenShift cluster), using hardened defaults")
			return intermediateDefaults()
		}
		if k8serr.IsNotFound(err) {
			log.Info("APIServer resource not found, using hardened defaults")
			return intermediateDefaults()
		}

		return "", "", fmt.Errorf("fetching TLS profile: %w", err)
	}

	return tlsProfileSpecToStrings(spec)
}

func intermediateDefaults() (string, string, error) {
	defaultSpec := *configv1.TLSProfiles[configv1.TLSProfileIntermediateType]
	return tlsProfileSpecToStrings(defaultSpec)
}

func tlsProfileSpecToStrings(spec configv1.TLSProfileSpec) (string, string, error) {
	minVersion := string(spec.MinTLSVersion)
	if minVersion == "" {
		minVersion = string(configv1.VersionTLS12)
	}

	ianaCiphers := libgocrypto.OpenSSLToIANACipherSuites(spec.Ciphers)

	return minVersion, strings.Join(ianaCiphers, ","), nil
}

func isNotRegisteredError(err error) bool {
	for err != nil {
		if runtime.IsNotRegisteredError(err) {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

func isOpenShiftCluster(mgr ctrl.Manager) bool {
	_, err := mgr.GetRESTMapper().RESTMapping(
		schema.GroupKind{Group: "config.openshift.io", Kind: "APIServer"},
	)
	return err == nil
}
