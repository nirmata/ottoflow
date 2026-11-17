/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package chart

import (
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// renderAgentExecutor renders the chart with the given values (empty for defaults) and returns
// the agent-executor Deployment's pod spec.
func renderAgentExecutor(t *testing.T, values string) corev1.PodSpec {
	t.Helper()
	valuesFile := ""
	if values != "" {
		valuesFile = filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(valuesFile, []byte(values), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	output, err := helmTemplate(t, "ottoflow", valuesFile)
	if err != nil {
		t.Fatalf("helm template failed: %v", err)
	}
	for _, doc := range splitYAMLDocs(output) {
		var d appsv1.Deployment
		if yaml.Unmarshal([]byte(doc), &d) != nil || d.Kind != "Deployment" {
			continue
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			if c.Name == "agent-executor" {
				return d.Spec.Template.Spec
			}
		}
	}
	t.Fatal("agent-executor Deployment not rendered")
	return corev1.PodSpec{}
}

const llmCredentialsValues = "agentExecutor:\n  llmCredentialsSecret: my-llm-keys\n"

// The Secret is mounted as a directory (no subPath), so kubelet keeps it current after a rotation.
func TestAgentExecutorLLMCredentials_MountedAsDirectory(t *testing.T) {
	spec := renderAgentExecutor(t, llmCredentialsValues)
	assertLLMCredentialsMount(t, spec)

	var vol *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == "llm-credentials" {
			vol = &spec.Volumes[i]
		}
	}
	if vol == nil || vol.Secret == nil {
		t.Fatalf("llm-credentials Secret volume not rendered: %+v", spec.Volumes)
	}
	if vol.Secret.SecretName != "my-llm-keys" || vol.Secret.Optional == nil || !*vol.Secret.Optional ||
		vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != 0o400 {
		t.Errorf("llm-credentials volume = %+v, want secret my-llm-keys, optional, mode 0400", vol.Secret)
	}

	c := spec.Containers[0]
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef == nil || c.EnvFrom[0].SecretRef.Name != "my-llm-keys" {
		t.Errorf("envFrom = %+v, want the unchanged my-llm-keys secretRef", c.EnvFrom)
	}
}

func assertLLMCredentialsMount(t *testing.T, spec corev1.PodSpec) {
	t.Helper()
	for _, m := range spec.Containers[0].VolumeMounts {
		if m.Name != "llm-credentials" {
			continue
		}
		if m.MountPath != "/etc/ottoflow/llm-credentials" || !m.ReadOnly {
			t.Errorf("mount = %+v, want read-only at /etc/ottoflow/llm-credentials", m)
		}
		if m.SubPath != "" {
			t.Errorf("mount subPath = %q, want empty: a subPath mount never receives Secret updates", m.SubPath)
		}
		return
	}
	t.Fatalf("llm-credentials mount not rendered: %+v", spec.Containers[0].VolumeMounts)
}

// No llmCredentialsSecret, no volume.
func TestAgentExecutorLLMCredentials_AbsentByDefault(t *testing.T) {
	spec := renderAgentExecutor(t, "")
	for _, v := range spec.Volumes {
		if v.Name == "llm-credentials" {
			t.Errorf("llm-credentials volume rendered by default")
		}
	}
}
