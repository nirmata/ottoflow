//go:build e2e

/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nirmata/ottoflow/test/utils"
)

const namespace = "ottoflow"

// agentExecutorCASecretDefault matches charts/ottoflow with HELM_RELEASE_NAME=ottoflow,
// HELM_NAMESPACE=ottoflow, default agentExecutor naming (ottoflow-agent-executor).
const agentExecutorCASecretDefault = "ottoflow-agent-executor.ottoflow.svc.tls-ca"

// agentExecutorDeploymentDefault matches include "ottoflow.agentExecutor.fullname" for release ottoflow.
const agentExecutorDeploymentDefault = "ottoflow-agent-executor"

// agentExecutorServiceHost is the Service DNS name the agent-executor certificate carries as a SAN.
const agentExecutorServiceHost = agentExecutorDeploymentDefault + "." + namespace + ".svc"

// controllerDeploymentDefault matches include "ottoflow.fullname" for release ottoflow.
const controllerDeploymentDefault = "ottoflow"

var manifestPath string

// waitForSecret polls until the Secret exists (e.g. agent-executor CA from the TLS cert controller).
func waitForSecret(name, ns string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		cmd := exec.Command("kubectl", "get", "secret", name, "-n", ns)
		if _, err := utils.Run(cmd); err == nil {
			return nil
		}
		<-ticker.C
	}
	return fmt.Errorf("secret %s/%s did not appear within %v", ns, name, timeout)
}

func waitForDeploymentRollout(name, ns string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// kubectl rollout status respects --timeout on the client side
	cmd := exec.CommandContext(ctx, "kubectl", "rollout", "status",
		fmt.Sprintf("deployment/%s", name), "-n", ns, "--timeout", timeout.String())
	out, err := utils.Run(cmd)
	if err != nil {
		return fmt.Errorf("rollout status for deployment %s/%s: %w (%s)", ns, name, err, string(out))
	}
	return nil
}

// Run e2e tests using the Ginkgo runner.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting ottoflow suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	var err error

	// Install Prometheus operator in background when not skipped (best-effort; tests don't depend on it)
	if os.Getenv("SKIP_PROMETHEUS_OPERATOR") != "1" {
		By("installing prometheus operator in background (best-effort for metrics)")
		go func() {
			if installErr := utils.InstallPrometheusOperator(); installErr != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "warning: Prometheus operator install failed: %v\n", installErr)
			}
		}()
	}

	By("creating manager namespace")
	cmd := exec.Command("kubectl", "create", "ns", namespace)
	_, _ = utils.Run(cmd)

	By("installing CRDs")
	cmd = exec.Command("make", "install")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	By("building and loading controller, workflow-runner, and agent-executor images in parallel")
	var controllerImage, runnerImage, agentExecutorImage string
	var errController, errRunner, errAgentExecutor error
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		controllerImage, errController = buildAndLoadLocalImageResult("github.com/nirmata/ottoflow/cmd/controller")
	}()
	go func() {
		defer wg.Done()
		runnerImage, errRunner = buildAndLoadLocalImageResult("github.com/nirmata/ottoflow/cmd/workflow-runner")
	}()
	go func() {
		defer wg.Done()
		agentExecutorImage, errAgentExecutor = buildAndLoadLocalImageResult("github.com/nirmata/ottoflow/cmd/agent-executor")
	}()
	wg.Wait()
	Expect(errController).NotTo(HaveOccurred())
	Expect(errRunner).NotTo(HaveOccurred())
	Expect(errAgentExecutor).NotTo(HaveOccurred())

	By("generating install manifests with local image overrides")
	manifestPath = filepath.Join("config", "generated", "install-e2e.yaml")
	helmValues := filepath.Join("test", "e2e", "helm-values-e2e.yaml")
	cmd = exec.Command(
		"make",
		"generate-manifests",
		fmt.Sprintf("IMG=%s", controllerImage),
		fmt.Sprintf("WORKFLOW_RUNNER_IMG=%s", runnerImage),
		fmt.Sprintf("AGENT_EXECUTOR_IMG=%s", agentExecutorImage),
		fmt.Sprintf("HELM_OUTPUT_FILE=%s", manifestPath),
		fmt.Sprintf("HELM_VALUES_FILE=%s", helmValues),
	)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	By("deploying the controller-manager")
	cmd = exec.Command("kubectl", "apply", "-f", manifestPath)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	By("waiting for controller-manager Deployment ready (webhook server listening)")
	Expect(waitForDeploymentRollout(controllerDeploymentDefault, namespace, 90*time.Second)).To(Succeed())

	By("waiting for agent-executor internal TLS CA secret (async after controller start)")
	Expect(waitForSecret(agentExecutorCASecretDefault, namespace, 2*time.Minute)).To(Succeed())

	By("waiting for agent-executor Deployment ready (TLS mounts + probe)")
	Expect(waitForDeploymentRollout(agentExecutorDeploymentDefault, namespace, 3*time.Minute)).To(Succeed())
})

var _ = AfterSuite(func() {
	if manifestPath != "" {
		By("undeploying the controller-manager")
		cmd := exec.Command("kubectl", "delete", "--ignore-not-found=true", "-f", manifestPath)
		_, _ = utils.Run(cmd)
	}

	By("uninstalling the Prometheus manager bundle")
	utils.UninstallPrometheusOperator()

	By("removing manager namespace")
	cmd := exec.Command("kubectl", "delete", "ns", namespace)
	_, _ = utils.Run(cmd)
})
