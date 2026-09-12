//go:build e2e

/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package e2e

import (
	"encoding/base64"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nirmata/ottoflow/test/utils"
)

const partOfLabel = "app.kubernetes.io/part-of=ottoflow"

// kubectl runs kubectl and returns trimmed stdout+stderr. The error carries the full
// command and output because a bare "exit status 1" in an e2e failure is unactionable.
func kubectl(args ...string) (string, error) {
	out, err := utils.Run(exec.Command("kubectl", args...))
	return strings.TrimSpace(string(out)), err
}

// kubectlIgnoreErr runs kubectl and discards failures. For cleanup only.
func kubectlIgnoreErr(args ...string) string {
	out, _ := kubectl(args...)
	return out
}

// applyYAML pipes a manifest to `kubectl apply -f -`. Using stdin rather than a temp file
// keeps the manifest visible in the Ginkgo log next to the failure it caused.
func applyYAML(manifest string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := utils.Run(cmd)
	if err != nil {
		return fmt.Errorf("kubectl apply failed: %w\nmanifest:\n%s\noutput:\n%s", err, manifest, string(out))
	}
	return nil
}

// mustApplyYAML applies a manifest and registers its deletion for spec teardown.
//
// The apply is retried, bounded, because admission is eventually consistent in two windows a
// spec cannot avoid: right after install, until the controller has injected the webhook CA
// bundle (cmd/controller/main.go, PatchValidatingWebhookConfigCA); and right after an Agent
// or sub-Workflow is created, until the controller's informer has seen it — the Workflow
// webhook resolves agentRef and workflowRef against the manager cache, so a Workflow applied
// in the same batch as the Agent it names can be rejected with "not found in namespace". A
// WorkflowRun must NOT be in such a batch: the reconciler fails a run whose
// Workflow does not exist yet terminally, so a run created before its Workflow is admitted
// stays Failed no matter how the retry goes. Specs apply the run in a separate call. The bound
// keeps a real rejection visible: the last error is what the spec reports.
func mustApplyYAML(manifest string) {
	GinkgoHelper()
	Eventually(func() error { return applyYAML(manifest) }, 45*time.Second, 2*time.Second).Should(Succeed())
	DeferCleanup(func() {
		cmd := exec.Command("kubectl", "delete", "--ignore-not-found=true", "--wait=false", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		_, _ = utils.Run(cmd)
	})
}

// mustCreateSecret creates an Opaque Secret from key/value pairs and registers its deletion
// for spec teardown. The values are credentials (a ServiceAccount token, a kubeconfig), so
// they must never reach the Ginkgo log: utils.Run prints every command line it executes,
// which rules out --from-literal arguments, and mustApplyYAML echoes the manifest into a
// failure message. The Secret is therefore built as a manifest with base64 data, piped over
// stdin, and only kubectl's own output is reported when the apply fails.
func mustCreateSecret(ns, name string, data map[string]string) {
	GinkgoHelper()
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\ntype: Opaque\ndata:\n", name, ns)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %s\n", k, base64.StdEncoding.EncodeToString([]byte(data[k])))
	}
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(b.String())
	out, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "creating Secret %s/%s (keys %v): %s", ns, name, keys, string(out))
	DeferCleanup(func() {
		_, _ = kubectl("delete", "secret", name, "-n", ns, "--ignore-not-found=true", "--wait=false")
	})
}

// serviceAccountToken mints a short-lived token for an existing ServiceAccount via the
// TokenRequest API. The token is what a tenant would hand a run through a Secret: a
// credential that is NOT any OttoFlow identity, so anything done with it cannot have been
// done with OttoFlow's own RBAC.
func serviceAccountToken(ns, sa string) string {
	GinkgoHelper()
	token, err := kubectl("create", "token", sa, "-n", ns, "--duration=1h")
	Expect(err).NotTo(HaveOccurred(), "minting a token for ServiceAccount %s/%s: %s", ns, sa, token)
	Expect(token).NotTo(BeEmpty(), "kubectl create token returned an empty token for %s/%s", ns, sa)
	return token
}

// canI asks the API server whether a ServiceAccount may perform verb on resource in ns,
// returning kubectl's one-word verdict ("yes" or "no"). resource may carry a name
// ("secrets/my-secret"). kubectl exits non-zero on "no", which is not an error here.
func canI(verb, resource, ns, saNamespace, saName string) string {
	out, _ := kubectl("auth", "can-i", verb, resource, "-n", ns,
		"--as", "system:serviceaccount:"+saNamespace+":"+saName)
	return strings.TrimSpace(out)
}

// ensureNamespace creates ns and blocks until it is usable, then schedules its deletion
// for spec teardown. Tenant isolation specs use several of these at once, which is the
// point: a single-namespace suite cannot observe cross-tenant leakage at all.
//
// The wait is not incidental. Teardown deletes namespaces asynchronously, so a later spec
// reusing the same name finds it Terminating and every create in it is rejected with
// "unable to create new content in namespace ... because it is being terminated" — a
// failure that has nothing to do with what the spec is testing. Waiting for the previous
// incarnation to finish disappearing, and for the new one to reach Active, keeps a spec's
// result about the product rather than about the one before it.
func ensureNamespace(ns string) {
	GinkgoHelper()

	waitGone := func() {
		Eventually(func() bool {
			_, err := kubectl("get", "ns", ns)
			return err != nil
		}, 3*time.Minute, 2*time.Second).Should(BeTrue(),
			"namespace %s is stuck terminating; a spec cannot create anything in it.\n%s",
			ns, kubectlIgnoreErr("get", "ns", ns, "-o", "yaml"))
	}

	if phase, err := kubectl("get", "ns", ns, "-o", "jsonpath={.status.phase}"); err == nil && phase == "Terminating" {
		waitGone()
	}

	Eventually(func() error {
		if phase, err := kubectl("get", "ns", ns, "-o", "jsonpath={.status.phase}"); err == nil {
			if phase == "Active" {
				return nil
			}
			return fmt.Errorf("namespace %s is %s", ns, phase)
		}
		if _, err := kubectl("create", "ns", ns); err != nil {
			return err
		}
		return fmt.Errorf("namespace %s just created, waiting for Active", ns)
	}, 3*time.Minute, 2*time.Second).Should(Succeed(), "namespace %s never became Active", ns)

	DeferCleanup(func() {
		// --wait=false keeps teardown quick; the next ensureNamespace for this name is
		// what actually waits, and only if it needs to.
		_, _ = kubectl("delete", "ns", ns, "--ignore-not-found=true", "--wait=false")
	})
}

// runPhase returns .status.phase, or "" when the run or field is absent.
func runPhase(ns, name string) string {
	out, err := kubectl("get", "workflowrun", name, "-n", ns, "-o", "jsonpath={.status.phase}")
	if err != nil {
		return ""
	}
	return out
}

// runMessage returns .status.message.
func runMessage(ns, name string) string {
	out, _ := kubectl("get", "workflowrun", name, "-n", ns, "-o", "jsonpath={.status.message}")
	return out
}

// runFailureReason returns .status.failureReason (e.g. "SecretAccessDenied",
// "RunnerRefUnresolved"), or "" when the run has not failed or failed for an unclassified cause.
func runFailureReason(ns, name string) string {
	out, _ := kubectl("get", "workflowrun", name, "-n", ns, "-o", "jsonpath={.status.failureReason}")
	return out
}

// runOutput returns one workflow-level output from .status.outputs, as its raw JSON text.
func runOutput(ns, name, output string) string {
	out, _ := kubectl("get", "workflowrun", name, "-n", ns,
		"-o", fmt.Sprintf("jsonpath={.status.outputs.%s}", output))
	return out
}

// stepMessage returns .status.stepStatuses.<step>.message (falling back to .error), which
// is where a step-level failure reason lands when the run message is generic.
func stepMessage(ns, name, step string) string {
	msg, _ := kubectl("get", "workflowrun", name, "-n", ns,
		"-o", fmt.Sprintf("jsonpath={.status.stepStatuses.%s.message}", step))
	if msg != "" {
		return msg
	}
	errMsg, _ := kubectl("get", "workflowrun", name, "-n", ns,
		"-o", fmt.Sprintf("jsonpath={.status.stepStatuses.%s.error}", step))
	return errMsg
}

// runnerJobVolumes returns the runner Job's pod-template volumes as JSON, waiting for the
// controller to record the Job name on the run first. The volumes are the controller's
// single statement of HOW a value reaches the runner — a Secret-backed volume under
// /etc/ottoflow/secrets, a ConfigMap-backed CA, or nothing at all — so a spec that cares
// about the delivery mechanism, not just the outcome, asserts on this.
func runnerJobVolumes(ns, run string) string {
	GinkgoHelper()
	var jobName string
	Eventually(func() string {
		jobName, _ = kubectl("get", "workflowrun", run, "-n", ns, "-o", "jsonpath={.status.execution.jobName}")
		return jobName
	}, 2*time.Minute, 2*time.Second).ShouldNot(BeEmpty(),
		lazyRunDiagnostics(ns, run, "WorkflowRun %s/%s never recorded a runner Job.", ns, run))
	vols, err := kubectl("get", "job", jobName, "-n", ns, "-o", "jsonpath={.spec.template.spec.volumes}")
	Expect(err).NotTo(HaveOccurred(), "reading volumes of runner Job %s/%s: %s", ns, jobName, vols)
	return vols
}

// runDiagnostics assembles everything a human needs to understand a failed expectation
// about a WorkflowRun: phase, message, per-step statuses, the runner Job, and Events.
// Every Eventually on a run phase uses this as its failure annotation, because
// "Expected 'Failed' to equal 'Succeeded'" alone costs an extra debugging round trip.
func runDiagnostics(ns, name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n--- WorkflowRun %s/%s ---\n", ns, name)
	fmt.Fprintf(&b, "phase:   %q\n", runPhase(ns, name))
	fmt.Fprintf(&b, "message: %s\n", runMessage(ns, name))
	steps, stepsErr := kubectl("get", "workflowrun", name, "-n", ns, "-o", "jsonpath={.status.stepStatuses}")
	if stepsErr == nil && steps != "" {
		fmt.Fprintf(&b, "steps:   %s\n", steps)
	}
	execStatus, execErr := kubectl("get", "workflowrun", name, "-n", ns, "-o", "jsonpath={.status.execution}")
	if execErr == nil && execStatus != "" {
		fmt.Fprintf(&b, "exec:    %s\n", execStatus)
	}
	if evs := kubectlIgnoreErr("get", "events", "-n", ns,
		"--field-selector", "involvedObject.name="+name,
		"-o", "custom-columns=TYPE:.type,REASON:.reason,MSG:.message", "--no-headers"); evs != "" {
		fmt.Fprintf(&b, "events:\n%s\n", evs)
	}
	if jobs := kubectlIgnoreErr("get", "jobs", "-n", ns,
		"-l", "ottoflow.nirmata.io/workflowrun="+name, "--no-headers"); jobs != "" {
		fmt.Fprintf(&b, "jobs:\n%s\n", jobs)
	}
	return b.String()
}

// lazyRunDiagnostics builds the failure annotation for an Eventually as a func() string, which
// Gomega calls ONLY if the assertion fails, and only once it has.
//
// Passing runDiagnostics(ns, name) directly as a format argument reads correctly but is a trap:
// Go evaluates arguments before the call, so the diagnostics would be collected BEFORE
// Eventually polls anything and would describe the run's state at spec start — invariably
// `phase: ""` with no Job — no matter what the run went on to do. That turns the most
// informative part of a failure into actively misleading evidence.
//
// Gomega's AsyncAssertion.buildDescription accepts a lone func() string and defers it, so the
// description must be this single argument with the formatting folded in.
func lazyRunDiagnostics(ns, name, format string, args ...any) func() string {
	return func() string {
		return fmt.Sprintf(format, args...) + runDiagnostics(ns, name)
	}
}

// expectRunSucceeds asserts the run reaches Succeeded, annotating any failure with the
// full run diagnostics.
func expectRunSucceeds(ns, name string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func() string { return runPhase(ns, name) }, timeout, 2*time.Second).
		Should(Equal("Succeeded"),
			lazyRunDiagnostics(ns, name, "WorkflowRun %s/%s did not succeed.", ns, name))
}

// expectRunFailsWith asserts the run reaches Failed AND that its message (or the named
// step's message) contains substr. The substring check is the part with teeth: asserting
// only "Failed" would pass for any unrelated breakage, which is precisely how a green
// suite can coexist with a live defect.
func expectRunFailsWith(ns, name, step, substr string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func() string { return runPhase(ns, name) }, timeout, 2*time.Second).
		Should(Equal("Failed"),
			lazyRunDiagnostics(ns, name, "WorkflowRun %s/%s was expected to fail with %q.", ns, name, substr))

	combined := runMessage(ns, name)
	if step != "" {
		combined += " || " + stepMessage(ns, name, step)
	}
	Expect(combined).To(ContainSubstring(substr),
		"WorkflowRun %s/%s failed, but for the wrong reason. Expected the failure to mention %q.%s",
		ns, name, substr, runDiagnostics(ns, name))
}

// eventsFor returns "REASON\tMESSAGE" lines for one object.
func eventsFor(ns, name string) string {
	return kubectlIgnoreErr("get", "events", "-n", ns,
		"--field-selector", "involvedObject.name="+name,
		"-o", "custom-columns=REASON:.reason,MSG:.message", "--no-headers")
}

// runnerSAFor mirrors buildWorkflowRunnerJob's default derivation: with no explicit
// spec.execution.job.serviceAccountName and no --workflow-runner-service-account, the
// runner SA name is "<workflowRef.name>-runner". The controller creates it on the run's
// first reconcile, so a spec never has to.
func runnerSAFor(workflowName string) string {
	return workflowName + "-runner"
}

// controllerServiceAccount is the identity the controller runs as; the chart's
// ServiceAccount for the manager Deployment.
const controllerServiceAccount = "controller-manager"

// agentExecutorServiceAccount is the identity the agent-executor Deployment runs as, for the
// chart's default agentExecutor naming (see agentExecutorDeploymentDefault).
const agentExecutorServiceAccount = "ottoflow-agent-executor"
