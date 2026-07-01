package controller

import (
	"context"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var tlsTestScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(configv1.Install(s))
	return s
}()

func TestFetchTLSConfig_IntermediateProfile(t *testing.T) {
	apiServer := &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.APIServerSpec{
			TLSSecurityProfile: &configv1.TLSSecurityProfile{
				Type: configv1.TLSProfileIntermediateType,
			},
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		WithObjects(apiServer).
		Build()

	minVersion, cipherSuites, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if minVersion != "VersionTLS12" {
		t.Errorf("minVersion = %q, want %q", minVersion, "VersionTLS12")
	}

	ciphers := strings.Split(cipherSuites, ",")
	if len(ciphers) == 0 {
		t.Fatal("expected non-empty cipher suites")
	}

	expectedCiphers := []string{
		"TLS_AES_128_GCM_SHA256",
		"TLS_AES_256_GCM_SHA384",
		"TLS_CHACHA20_POLY1305_SHA256",
		"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
		"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
		"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
		"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
		"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256",
		"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256",
	}
	for _, expected := range expectedCiphers {
		if !strings.Contains(cipherSuites, expected) {
			t.Errorf("cipher suites missing %q", expected)
		}
	}
}

func TestFetchTLSConfig_ModernProfile(t *testing.T) {
	apiServer := &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.APIServerSpec{
			TLSSecurityProfile: &configv1.TLSSecurityProfile{
				Type: configv1.TLSProfileModernType,
			},
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		WithObjects(apiServer).
		Build()

	minVersion, cipherSuites, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if minVersion != "VersionTLS13" {
		t.Errorf("minVersion = %q, want %q", minVersion, "VersionTLS13")
	}

	ciphers := strings.Split(cipherSuites, ",")
	if len(ciphers) != 3 {
		t.Errorf("expected 3 TLS 1.3 ciphers, got %d: %v", len(ciphers), ciphers)
	}
}

func TestFetchTLSConfig_CustomProfile(t *testing.T) {
	apiServer := &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.APIServerSpec{
			TLSSecurityProfile: &configv1.TLSSecurityProfile{
				Type: configv1.TLSProfileCustomType,
				Custom: &configv1.CustomTLSProfile{
					TLSProfileSpec: configv1.TLSProfileSpec{
						MinTLSVersion: configv1.VersionTLS13,
						Ciphers:       []string{"TLS_AES_128_GCM_SHA256"},
					},
				},
			},
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		WithObjects(apiServer).
		Build()

	minVersion, cipherSuites, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if minVersion != "VersionTLS13" {
		t.Errorf("minVersion = %q, want %q", minVersion, "VersionTLS13")
	}

	if cipherSuites != "TLS_AES_128_GCM_SHA256" {
		t.Errorf("cipherSuites = %q, want %q", cipherSuites, "TLS_AES_128_GCM_SHA256")
	}
}

func TestFetchTLSConfig_NilProfile_ReturnsIntermediateDefaults(t *testing.T) {
	apiServer := &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
	}

	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		WithObjects(apiServer).
		Build()

	minVersion, cipherSuites, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if minVersion != "VersionTLS12" {
		t.Errorf("minVersion = %q, want %q (Intermediate default)", minVersion, "VersionTLS12")
	}

	if cipherSuites == "" {
		t.Error("expected non-empty cipher suites for Intermediate defaults")
	}
}

func TestFetchTLSConfig_APIServerNotFound_ReturnsDefaults(t *testing.T) {
	cli := fake.NewClientBuilder().
		WithScheme(tlsTestScheme).
		Build()

	minVersion, cipherSuites, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if minVersion != "VersionTLS12" {
		t.Errorf("minVersion = %q, want %q (Intermediate default)", minVersion, "VersionTLS12")
	}

	if cipherSuites == "" {
		t.Error("expected non-empty cipher suites for Intermediate defaults")
	}
}

func TestFetchTLSConfig_NoMatchError_ReturnsDefaults(t *testing.T) {
	cli := fake.NewClientBuilder().
		WithScheme(runtime.NewScheme()).
		Build()

	minVersion, cipherSuites, err := fetchTLSConfig(context.Background(), cli)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if minVersion != "VersionTLS12" {
		t.Errorf("minVersion = %q, want %q (Intermediate default)", minVersion, "VersionTLS12")
	}

	if cipherSuites == "" {
		t.Error("expected non-empty cipher suites for Intermediate defaults")
	}
}
