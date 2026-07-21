package doctor_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/doctor"
)

func TestUnsafeFeaturesDisabledCheck(t *testing.T) {
	tests := []struct {
		name      string
		operator  string
		dashboard string
		want      check.Status
		wantError string
	}{
		{name: "secure defaults", want: check.StatusPass},
		{name: "enabled", operator: "source.git", dashboard: "source.git", want: check.StatusFail},
		{name: "drift", operator: "source.git", dashboard: "", want: check.StatusFail},
		{name: "unknown ID is indeterminate", operator: "source.magic", dashboard: "source.magic", wantError: "unknown unsafe feature"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := probeCheck(t, doctor.Options{Client: fakeClient(t,
				featureDeployment("orkano-operator", "operator", tc.operator),
				featureDeployment("orkano-dashboard", "dashboard", tc.dashboard),
			)}, doctor.IDUnsafeFeaturesDisabled)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want %q", err, tc.wantError)
				}
				return
			}
			if err != nil || result.Status != tc.want {
				t.Fatalf("result = %+v, err = %v, want %s", result, err, tc.want)
			}
		})
	}
}

func TestUnsafeFeaturesDisabledCheckSkipsAbsentInstall(t *testing.T) {
	result, err := probeCheck(t, doctor.Options{Client: fakeClient(t)}, doctor.IDUnsafeFeaturesDisabled)
	if err != nil || result.Status != check.StatusSkip {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func featureDeployment(name, container, value string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "orkano-system"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: container,
			Env:  []corev1.EnvVar{{Name: "ORKANO_UNSAFE_FEATURES", Value: value}},
		}}}}},
	}
}
