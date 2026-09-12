//go:build e2e

/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nirmata/ottoflow/internal/workflow/controller"
)

// Zero-by-default Secret RBAC — the headline property of the RBAC hardening.
//
// Every spec here runs with NO operator grant in place, which is the default every fresh
// install runs with, and pins what that default does: no OttoFlow ClusterRole grants any
// verb on Secrets, the controller genuinely holds no Secret read in a tenant namespace, a
// denied read surfaces as an actionable failure naming the exact Role to add, and a
// break-by-default feature degrades benignly with a Warning event rather than failing the
// run.
var _ = Describe("Zero-by-default Secret RBAC", Label("hardening"), func() {

	const tenant = "e2e-secret-zero"

	BeforeEach(func() {
		ensureNamespace(tenant)
	})

	It("ships no ClusterRole that grants any verb on secrets", func() {
		// The chart is the one place a cluster-wide Secret grant could come back from, so
		// read the installed ClusterRoles themselves rather than inferring from behaviour:
		// a rule on any of them — the controller's, the runner's, the agent-executor's, or
		// an aggregated fragment — is a regression regardless of which identity it reaches.
		raw, err := kubectl("get", "clusterroles", "-o", "json")
		Expect(err).NotTo(HaveOccurred(), raw)

		var list struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Rules []struct {
					APIGroups []string `json:"apiGroups"`
					Resources []string `json:"resources"`
					Verbs     []string `json:"verbs"`
				} `json:"rules"`
			} `json:"items"`
		}
		Expect(json.Unmarshal([]byte(raw), &list)).To(Succeed())

		var seen, offending []string
		for _, cr := range list.Items {
			if !strings.HasPrefix(cr.Metadata.Name, "ottoflow") {
				continue
			}
			seen = append(seen, cr.Metadata.Name)
			for _, rule := range cr.Rules {
				coreGroup := false
				for _, g := range rule.APIGroups {
					if g == "" || g == "*" {
						coreGroup = true
					}
				}
				if !coreGroup {
					continue
				}
				for _, res := range rule.Resources {
					if res == "secrets" || res == "*" {
						offending = append(offending, fmt.Sprintf("%s: resources=%v verbs=%v",
							cr.Metadata.Name, rule.Resources, rule.Verbs))
					}
				}
			}
		}
		Expect(seen).NotTo(BeEmpty(), "no ClusterRole named ottoflow* is installed; the chart install did not land")
		Expect(offending).To(BeEmpty(),
			"an OttoFlow ClusterRole grants access to secrets. OttoFlow ships with zero cluster-wide "+
				"Secret access for every component; Secret reads are granted per Secret, per namespace, "+
				"by the operator (docs/user/rbac-secret-access.md). ClusterRoles checked: %v", seen)
	})

	It("really holds no `secrets get` in a tenant namespace (kubectl auth can-i)", func() {
		// The cheapest, most decisive form of the claim: ask the API server directly.
		// Everything else in this Describe is about how a denial SURFACES; this one pins
		// that the denial exists at all — a chart regression that quietly restored a
		// cluster-wide grant would pass every behavioural spec here (reads simply start
		// succeeding) and only this one, and the ClusterRole scan above, would catch it.
		Expect(canI("get", "secrets", tenant, namespace, controllerServiceAccount)).To(Equal("no"),
			"the controller ServiceAccount can `get secrets` in tenant namespace %q with no "+
				"operator grant in place; the zero-by-default Secret RBAC has regressed", tenant)
		Expect(canI("list", "secrets", tenant, namespace, controllerServiceAccount)).To(Equal("no"),
			"the controller ServiceAccount can `list secrets` in tenant namespace %q", tenant)
		Expect(canI("get", "secrets", tenant, namespace, agentExecutorServiceAccount)).To(Equal("no"),
			"the agent-executor ServiceAccount can `get secrets` in tenant namespace %q with no "+
				"operator grant in place", tenant)
	})

	It("fails a Secret-volume run with the exact Role remediation when no grant exists", func() {
		// The Secret EXISTS; only the grant is missing. That distinction is the point:
		// the failure below must be diagnosed as RBAC (grant a Role), not as a missing
		// Secret (create the object), or the operator fixes the wrong thing.
		mustApplyYAML(fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: nogrant-vol-secret
  namespace: %[1]s
type: Opaque
stringData:
  value: "exists-but-ungranted"
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: nogrant-wf
  namespace: %[1]s
spec:
  steps:
    - name: hello
      expressions:
        - name: result
          expression: '"hello"'
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: nogrant-run
  namespace: %[1]s
spec:
  workflowRef:
    name: nogrant-wf
    namespace: %[1]s
  execution:
    job:
      volumes:
        - name: ungranted
          secret:
            secretName: nogrant-vol-secret
`, tenant))

		expectRunFailsWith(tenant, "nogrant-run", "", "Forbidden", 3*time.Minute)

		msg := runMessage(tenant, "nogrant-run")
		Expect(msg).To(SatisfyAll(
			ContainSubstring("Role"),
			ContainSubstring(fmt.Sprintf("%q", "nogrant-vol-secret")),
			ContainSubstring("docs/user/rbac-secret-access.md"),
		), "the run failed on the denied Secret read, but the message does not hand the operator "+
			"the exact remediation (a namespaced Role naming this Secret, and the doc that explains "+
			"it). Message: %s", msg)

		Expect(runFailureReason(tenant, "nogrant-run")).To(Equal("SecretAccessDenied"),
			"a controller-side Forbidden Secret read must classify status.failureReason as "+
				"SecretAccessDenied")
	})

	It("skips LLM credential injection benignly, with a Warning event, when the read is denied", func() {
		// Injection is a break-by-default convenience: a denied read must NOT fail the run
		// (an expressions-only run has no use for the credentials at all), but it must not
		// be silent either — the operator who expected injection needs the event. This
		// workflow is expressions-only (no agentRef step, see below), which is exactly what
		// selects the benign branch; the next spec pins the opposite branch (a Nirmata agent
		// step, where the same denied read is terminal).
		mustApplyYAML(fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: llmdeny-creds
  namespace: %[1]s
type: Opaque
stringData:
  NIRMATA_LLM_TOKEN: "exists-but-ungranted"
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: llmdeny-wf
  namespace: %[1]s
spec:
  steps:
    - name: hello
      expressions:
        - name: result
          expression: '"hello"'
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: llmdeny-run
  namespace: %[1]s
spec:
  workflowRef:
    name: llmdeny-wf
    namespace: %[1]s
  execution:
    llmCredentialsSecret:
      name: llmdeny-creds
`, tenant))

		expectRunSucceeds(tenant, "llmdeny-run", 3*time.Minute)

		Eventually(func() string { return eventsFor(tenant, "llmdeny-run") },
			30*time.Second, 2*time.Second).Should(ContainSubstring("LLMCredentialsForbidden"),
			"the denied credentials read produced no LLMCredentialsForbidden Warning event; an "+
				"operator who expected injection has no signal that RBAC, not the workflow, is why "+
				"their agent steps run without credentials.")
	})

	It("fails the run with the RBAC remediation when a Nirmata agent step's credentials read is denied", func() {
		// Same shape as the benign spec above, except the workflow has a Nirmata-provider
		// agentRef step. A reachable Nirmata AgentRef needs this credential and the run
		// supplies none of its own, so the denied read must now fail the run terminally
		// instead of proceeding to the generic, misleading "Nirmata LLM credentials required"
		// failure at the agent-executor.
		mustApplyYAML(fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: llmdeny-nirmata-creds
  namespace: %[1]s
type: Opaque
stringData:
  NIRMATA_LLM_TOKEN: "exists-but-ungranted"
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Agent
metadata:
  name: llmdeny-nirmata-agent
  namespace: %[1]s
spec:
  prompt: "You are helpful."
  modelProvider: nirmata
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: llmdeny-nirmata-wf
  namespace: %[1]s
spec:
  steps:
    - name: agentstep
      agentRef:
        name: llmdeny-nirmata-agent
        namespace: %[1]s
      expressions:
        - name: result
          expression: '"ok"'
`, tenant))
		// The run goes in its own apply, after the Workflow is admitted (see mustApplyYAML).
		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: llmdeny-nirmata-run
  namespace: %[1]s
spec:
  workflowRef:
    name: llmdeny-nirmata-wf
    namespace: %[1]s
  execution:
    llmCredentialsSecret:
      name: llmdeny-nirmata-creds
`, tenant))

		expectRunFailsWith(tenant, "llmdeny-nirmata-run", "", "Forbidden", 3*time.Minute)

		msg := runMessage(tenant, "llmdeny-nirmata-run")
		Expect(msg).To(SatisfyAll(
			ContainSubstring("Role"),
			ContainSubstring(fmt.Sprintf("%q", "llmdeny-nirmata-creds")),
			ContainSubstring("rbac.secretAccess"),
			ContainSubstring("docs/user/rbac-secret-access.md"),
			Not(ContainSubstring("set NIRMATA_LLM_TOKEN")),
			// This tenant carries no grant at all, so the SelfSubjectAccessReview gate (which
			// runs BEFORE the Secret Get) denies the read itself, on a real API server — the Get
			// never even fires. controller.SecretReadNotAuthorizedFragment is the one substring
			// that tells this apart from a denial the Get's own Forbidden caught: the rest of
			// the message is byte-identical either way by design.
			ContainSubstring(controller.SecretReadNotAuthorizedFragment),
		), "the run failed on the denied credentials read, but the message does not hand the operator "+
			"the exact remediation, or it leaked the generic agent-executor wording the preflight is "+
			"meant to replace, or it does not show the SelfSubjectAccessReview gate (not just the Get) "+
			"caught this denial. Message: %s", msg)

		Expect(runFailureReason(tenant, "llmdeny-nirmata-run")).To(Equal("SecretAccessDenied"),
			"a controller-side Forbidden Secret read must classify status.failureReason as "+
				"SecretAccessDenied")
	})

	It("proceeds past Job build when the run supplies its own credentials despite a denied read", func() {
		// Same shape again, but spec.execution.job.env supplies NIRMATA_LLM_TOKEN directly.
		// This placeholder token can never authenticate against the real Nirmata endpoint, so
		// the run cannot succeed end-to-end — it dies at the real LLM call, exactly as the
		// placeholder-token specs in llm_credentials_e2e_test.go do.
		//
		// What this spec actually guards (hasExplicitNirmataCreds): the denied well-known-Secret
		// read must not cost this run anything AT BUILD TIME. Injection is skipped benignly and
		// the runner Job is still built and executed — reaching the agent step, and failing only
		// there, is the proof. The load-bearing assertion is the negative one below: the failure
		// must NOT be the build-time RBAC remediation error, or injection-skip has regressed into
		// a hard failure again.
		mustApplyYAML(fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: llmdeny-explicit-creds
  namespace: %[1]s
type: Opaque
stringData:
  NIRMATA_LLM_TOKEN: "exists-but-ungranted"
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Agent
metadata:
  name: llmdeny-explicit-agent
  namespace: %[1]s
spec:
  prompt: "You are helpful."
  modelProvider: nirmata
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: llmdeny-explicit-wf
  namespace: %[1]s
spec:
  steps:
    - name: agentstep
      agentRef:
        name: llmdeny-explicit-agent
        namespace: %[1]s
      expressions:
        - name: result
          expression: '"ok"'
`, tenant))
		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: llmdeny-explicit-run
  namespace: %[1]s
spec:
  workflowRef:
    name: llmdeny-explicit-wf
    namespace: %[1]s
  execution:
    llmCredentialsSecret:
      name: llmdeny-explicit-creds
    job:
      env:
        - name: NIRMATA_LLM_TOKEN
          value: "supplied-directly-by-the-run"
`, tenant))

		expectRunFailsWith(tenant, "llmdeny-explicit-run", "agentstep", "agent execution failed", 3*time.Minute)

		msg := runMessage(tenant, "llmdeny-explicit-run")
		Expect(msg).To(SatisfyAll(
			Not(ContainSubstring("Failed to build runner Job")),
			Not(ContainSubstring("rbac.secretAccess")),
			Not(ContainSubstring("kubectl auth can-i")),
		), "the run failed with the build-time RBAC remediation instead of reaching the agent step — "+
			"injection-skip has regressed into a hard failure even though the run supplied its own "+
			"explicit credentials. Message: %s", msg)

		Eventually(func() string { return eventsFor(tenant, "llmdeny-explicit-run") },
			30*time.Second, 2*time.Second).Should(ContainSubstring("LLMCredentialsForbidden"),
			"the denied well-known-Secret read should still emit its Warning event even though the "+
				"run supplied its own credentials via job.env — injection is skipped, not silent.")
	})

	It("names the originating workflow field when a controller-minted Secret volume's Secret does not exist", func() {
		// The Secret is deliberately never created: an MCPServer auth.secretRef reaches the
		// runner as a controller-MINTED ottoflow-secret-* volume (collectSecretRefs /
		// buildSecretMounts), which the controller never reads itself — it only writes a
		// volume reference for the KUBELET to resolve under node credentials (see
		// docs/user/rbac-secret-access.md, Symptom D). No Role granted to the controller or
		// the runner ServiceAccount changes this outcome; the only authoritative signal is
		// the kubelet's own FailedMount report once the pod tries to start — this is runtime
		// mount validation, not an authorization pre-check, and the SelfSubjectAccessReview
		// gate does not apply to it (that gate covers only the well-known LLM-credentials
		// read).
		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: MCPServer
metadata:
  name: mintedmissing-server
  namespace: %[1]s
spec:
  transport:
    type: http
    address: http://127.0.0.1:1/mcp
  auth:
    type: bearer
    secretRef:
      name: mintedmissing-secret
      key: token
---
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: Workflow
metadata:
  name: mintedmissing-wf
  namespace: %[1]s
spec:
  steps:
    - name: callTool
      mcpToolCall:
        server: mintedmissing-server
        tool: whoami
`, tenant))
		mustApplyYAML(fmt.Sprintf(`
apiVersion: ottoflow.nirmata.io/v1alpha1
kind: WorkflowRun
metadata:
  name: mintedmissing-run
  namespace: %[1]s
spec:
  workflowRef:
    name: mintedmissing-wf
    namespace: %[1]s
`, tenant))

		expectRunFailsWith(tenant, "mintedmissing-run", "", "failed to mount", 3*time.Minute)

		Expect(runFailureReason(tenant, "mintedmissing-run")).To(Equal("RunnerRefUnresolved"),
			"a FailedMount on a controller-minted Secret volume must classify status.failureReason "+
				"as RunnerRefUnresolved, never SecretAccessDenied — the controller never attempted a "+
				"read of this Secret.")

		msg := runMessage(tenant, "mintedmissing-run")
		Expect(msg).To(SatisfyAll(
			ContainSubstring("callTool"),
			ContainSubstring("mcpToolCall"),
			ContainSubstring("auth.secretRef"),
		), "the message must name the workflow field that generated the failing volume (the "+
			"mcpToolCall step's auth.secretRef), not just the raw kubelet mount error. Message: %s", msg)
	})
})
