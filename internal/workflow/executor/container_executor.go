/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	kptr "k8s.io/utils/ptr"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

const (
	// ContainerStepAllowedImagesEnv carries the controller's container step allowlist to the runner (comma-separated).
	ContainerStepAllowedImagesEnv = "OTTOFLOW_CONTAINER_STEP_ALLOWED_IMAGES"
	// ContainerStepMaxOutputBytesEnv carries the controller's container step output cap to the runner.
	ContainerStepMaxOutputBytesEnv = "OTTOFLOW_CONTAINER_STEP_MAX_OUTPUT_BYTES"

	DefaultContainerStepMaxOutputBytes int64 = 1 << 20
	defaultContainerStepTimeout              = 10 * time.Minute
	containerStepCleanupTimeout              = 30 * time.Second
	containerStepTTLSeconds            int32 = 300
	containerStepContainerName               = "step"
	containerStepKubeconfigDir               = "/kube"
	containerStepUID                   int64 = 65532

	containerStepLabel = "ottoflow.nirmata.io/step"
	workflowRunLabel   = "ottoflow.nirmata.io/workflowrun"
	partOfLabel        = "app.kubernetes.io/part-of"
)

// containerStepWaitGrace lets the Job controller report the activeDeadlineSeconds failure before the runner gives up.
var containerStepWaitGrace = 30 * time.Second

// ErrContainerStepsDisabled is returned for a container step when the controller allowlist is empty.
var ErrContainerStepsDisabled = errors.New("container steps are disabled: the controller's container step allowlist (containerStep.allowedImages) is empty")

// ContainerStepConfig is the controller-level configuration for container steps.
type ContainerStepConfig struct {
	AllowedImages  []string
	MaxOutputBytes int64
	// Clientset talks to the cluster that runs the WorkflowRun (the control plane), not the clusterRef target.
	Clientset kubernetes.Interface
	// KubeconfigKey is the data key of the clusterRef kubeconfig Secret the runner found mounted, used when the ref names no key.
	KubeconfigKey string
}

// ContainerStepConfigFromEnv reads the allowlist and output cap the controller set on the runner Job.
func ContainerStepConfigFromEnv() (ContainerStepConfig, error) {
	cfg := ContainerStepConfig{AllowedImages: ParseAllowedImages(os.Getenv(ContainerStepAllowedImagesEnv))}
	if v := strings.TrimSpace(os.Getenv(ContainerStepMaxOutputBytesEnv)); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid %s %q: must be a positive integer", ContainerStepMaxOutputBytesEnv, v)
		}
		cfg.MaxOutputBytes = n
	}
	return cfg, nil
}

// SetContainerStepConfig enables container steps for this executor.
func (e *WorkflowExecutor) SetContainerStepConfig(cfg ContainerStepConfig) {
	e.containerStep = cfg
}

// ParseAllowedImages splits a comma-separated allowlist, dropping empty entries.
func ParseAllowedImages(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ImageAllowed reports whether image matches an allowlist entry. An entry ending in "/" matches
// every image under that path; any other entry matches that repository exactly, with any tag or digest.
func ImageAllowed(image string, allowed []string) bool {
	if image == "" || strings.ContainsAny(image, " \t\n\r") {
		return false
	}
	for _, a := range allowed {
		switch {
		case strings.HasSuffix(a, "/"):
			if strings.HasPrefix(image, a) && len(image) > len(a) {
				return true
			}
		case image == a, strings.HasPrefix(image, a+":"), strings.HasPrefix(image, a+"@"):
			return true
		}
	}
	return false
}

var containerTemplatePattern = regexp.MustCompile(`\{\{(.*?)\}\}`)

// ContainerTemplateExpressions returns the CEL expressions inside {{ }} segments of a container step's fields.
func ContainerTemplateExpressions(c *ottoflowv1alpha1.StepContainer) []string {
	if c == nil {
		return nil
	}
	vals := append([]string{c.Image}, c.Args...)
	for _, e := range c.Env {
		vals = append(vals, e.Value)
	}
	if c.Files != nil {
		vals = append(vals, c.Files.ConfigMapRef)
	}
	var out []string
	for _, f := range vals {
		for _, m := range containerTemplatePattern.FindAllStringSubmatch(f, -1) {
			if expr := strings.TrimSpace(m[1]); expr != "" {
				out = append(out, expr)
			}
		}
	}
	return out
}

// IsContainerTemplate reports whether s contains a {{ }} segment.
func IsContainerTemplate(s string) bool {
	return containerTemplatePattern.MatchString(s)
}

func (e *WorkflowExecutor) renderContainerTemplate(ctx context.Context, s string, vars map[string]interface{}) (string, error) {
	var firstErr error
	out := containerTemplatePattern.ReplaceAllStringFunc(s, func(m string) string {
		if firstErr != nil {
			return ""
		}
		expr := strings.TrimSpace(m[2 : len(m)-2])
		v, err := e.celEvaluator.EvaluateExpression(ctx, expr, vars)
		if err != nil {
			firstErr = fmt.Errorf("evaluate %q: %w", expr, err)
			return ""
		}
		if str, ok := v.(string); ok {
			return str
		}
		b, err := json.Marshal(v)
		if err != nil {
			firstErr = fmt.Errorf("format %q: %w", expr, err)
			return ""
		}
		return string(b)
	})
	return out, firstErr
}

// resolvedContainer is a StepContainer with its templates evaluated.
type resolvedContainer struct {
	Image        string
	Args         []string
	Env          []corev1.EnvVar
	ConfigMap    string
	MountPath    string
	Kubeconfig   bool
	Resources    corev1.ResourceRequirements
	Timeout      time.Duration
	SuccessCodes []int32
}

func (e *WorkflowExecutor) resolveContainer(ctx context.Context, c *ottoflowv1alpha1.StepContainer, vars map[string]interface{}) (*resolvedContainer, error) {
	r := &resolvedContainer{Kubeconfig: c.Kubeconfig, Timeout: defaultContainerStepTimeout, SuccessCodes: c.SuccessExitCodes}
	var err error
	if r.Image, err = e.renderContainerTemplate(ctx, c.Image, vars); err != nil {
		return nil, fmt.Errorf("image: %w", err)
	}
	for i, a := range c.Args {
		v, err := e.renderContainerTemplate(ctx, a, vars)
		if err != nil {
			return nil, fmt.Errorf("args[%d]: %w", i, err)
		}
		r.Args = append(r.Args, v)
	}
	for _, env := range c.Env {
		v, err := e.renderContainerTemplate(ctx, env.Value, vars)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", env.Name, err)
		}
		r.Env = append(r.Env, corev1.EnvVar{Name: env.Name, Value: v})
	}
	if c.Files != nil {
		if r.ConfigMap, err = e.renderContainerTemplate(ctx, c.Files.ConfigMapRef, vars); err != nil {
			return nil, fmt.Errorf("files.configMapRef: %w", err)
		}
		if r.ConfigMap == "" {
			return nil, fmt.Errorf("files.configMapRef evaluated to an empty name")
		}
		r.MountPath = c.Files.MountPath
	}
	if c.Resources != nil {
		r.Resources = *c.Resources.DeepCopy()
	}
	if c.Timeout != "" {
		d, err := time.ParseDuration(c.Timeout)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid timeout %q", c.Timeout)
		}
		r.Timeout = d
	}
	if len(r.SuccessCodes) == 0 {
		r.SuccessCodes = []int32{0}
	}
	return r, nil
}

// containerKubeconfigSecret returns the Secret name and key holding the WorkflowRun's target kubeconfig.
func containerKubeconfigSecret(run *ottoflowv1alpha1.WorkflowRun, mountedKey string) (string, string, error) {
	if run.Spec.ClusterRef == nil || run.Spec.ClusterRef.KubeConfigSecretRef == nil {
		return "", "", fmt.Errorf("kubeconfig: true needs spec.clusterRef.kubeConfigSecretRef on the WorkflowRun")
	}
	ref := run.Spec.ClusterRef.KubeConfigSecretRef
	if ref.Namespace != "" && ref.Namespace != run.Namespace {
		return "", "", fmt.Errorf("kubeconfig: true needs the kubeConfigSecretRef Secret in the WorkflowRun namespace %q, not %q", run.Namespace, ref.Namespace)
	}
	key := ref.Key
	if key == "" {
		key = mountedKey
	}
	if key == "" {
		return "", "", fmt.Errorf("kubeconfig: no data key found for Secret %q", ref.Name)
	}
	return ref.Name, key, nil
}

func buildContainerStepJob(run *ottoflowv1alpha1.WorkflowRun, stepName string, r *resolvedContainer, kubeconfigSecret, kubeconfigKey string) *batchv1.Job {
	workflowName := run.Spec.WorkflowRef.Name
	if workflowName == "" {
		workflowName = run.Name
	}
	labels := map[string]string{
		partOfLabel:                       "ottoflow",
		workflowRunLabel:                  run.Name,
		ottoflowv1alpha1.WorkflowLabelKey: workflowName,
		containerStepLabel:                stepName,
	}
	podLabels := map[string]string{
		partOfLabel:        "ottoflow",
		workflowRunLabel:   run.Name,
		containerStepLabel: stepName,
	}

	volumes := []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	mounts := []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}
	if r.ConfigMap != "" {
		volumes = append(volumes, corev1.Volume{Name: "files", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: r.ConfigMap},
			DefaultMode:          kptr.To(int32(0o444)),
		}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "files", MountPath: r.MountPath, ReadOnly: true})
	}
	if r.Kubeconfig {
		volumes = append(volumes, corev1.Volume{Name: "kubeconfig", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  kubeconfigSecret,
			Items:       []corev1.KeyToPath{{Key: kubeconfigKey, Path: "config"}},
			DefaultMode: kptr.To(int32(0o444)),
		}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "kubeconfig", MountPath: containerStepKubeconfigDir, ReadOnly: true})
	}
	env := append([]corev1.EnvVar{{Name: "HOME", Value: "/tmp"}}, r.Env...)

	deadline := int64(math.Ceil(r.Timeout.Seconds()))
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: containerStepJobPrefix(run.Name, stepName),
			Namespace:    run.Namespace,
			Labels:       labels,
			// Not a controller reference: the WorkflowRun controller owns only the runner Job.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: ottoflowv1alpha1.GroupVersion.String(),
				Kind:       "WorkflowRun",
				Name:       run.Name,
				UID:        run.UID,
			}},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            kptr.To(int32(0)),
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: kptr.To(containerStepTTLSeconds),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: kptr.To(false),
					EnableServiceLinks:           kptr.To(false),
					Volumes:                      volumes,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   kptr.To(true),
						RunAsUser:      kptr.To(containerStepUID),
						RunAsGroup:     kptr.To(containerStepUID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:                     containerStepContainerName,
						Image:                    r.Image,
						ImagePullPolicy:          corev1.PullIfNotPresent,
						Args:                     r.Args,
						Env:                      env,
						Resources:                r.Resources,
						VolumeMounts:             mounts,
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: kptr.To(false),
							ReadOnlyRootFilesystem:   kptr.To(true),
							Privileged:               kptr.To(false),
							RunAsNonRoot:             kptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
					}},
				},
			},
		},
	}
	return job
}

// containerStepJobPrefix keeps generated names within the 63-character label limit the Job controller copies into job-name.
func containerStepJobPrefix(runName, stepName string) string {
	p := strings.ToLower(runName + "-" + stepName)
	if len(p) > 50 {
		p = p[:50]
	}
	return strings.TrimRight(p, "-.") + "-"
}

// containerResult is what a finished container step exposes as `step`.
type containerResult struct {
	Stdout   string
	ExitCode int32
	Message  string
}

var errContainerStepTimeout = errors.New("container step timed out")

// runContainerJob creates job, waits for it to finish, reads the step container's stdout and always deletes the Job.
func runContainerJob(ctx context.Context, cs kubernetes.Interface, job *batchv1.Job, timeout time.Duration, maxOutputBytes int64) (*containerResult, error) {
	jobs := cs.BatchV1().Jobs(job.Namespace)
	created, err := jobs.Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create container step Job: %w", err)
	}
	defer func() {
		delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), containerStepCleanupTimeout)
		defer cancel()
		// A failed delete leaves the Job to its TTL and the WorkflowRun owner reference.
		delErr := jobs.Delete(delCtx, created.Name, metav1.DeleteOptions{PropagationPolicy: kptr.To(metav1.DeletePropagationBackground)})
		if delErr != nil && !apierrors.IsNotFound(delErr) {
			klog.ErrorS(delErr, "failed to delete container step Job", "namespace", created.Namespace, "job", created.Name)
		}
	}()

	waitCtx, cancel := context.WithTimeout(ctx, timeout+containerStepWaitGrace)
	defer cancel()
	finished, err := waitForJob(waitCtx, cs, created)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("%w after %s (Job %s)", errContainerStepTimeout, timeout, created.Name)
		}
		return nil, err
	}
	if jobFailedReason(finished) == batchv1.JobReasonDeadlineExceeded {
		return nil, fmt.Errorf("%w after %s (Job %s)", errContainerStepTimeout, timeout, created.Name)
	}

	pod, err := containerStepPod(ctx, cs, finished)
	if err != nil {
		return nil, err
	}
	term := containerTerminated(pod)
	if term == nil {
		return nil, fmt.Errorf("container step Job %s finished without a terminated container (reason %q)", finished.Name, jobFailedReason(finished))
	}
	stdout, err := readContainerStdout(ctx, cs, pod, maxOutputBytes)
	if err != nil {
		return nil, err
	}
	return &containerResult{Stdout: stdout, ExitCode: term.ExitCode, Message: term.Message}, nil
}

func jobFinished(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobFailedReason(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return c.Reason
		}
	}
	return ""
}

// waitForJob watches job until it reports Complete or Failed, re-establishing the watch when it closes.
func waitForJob(ctx context.Context, cs kubernetes.Interface, job *batchv1.Job) (*batchv1.Job, error) {
	jobs := cs.BatchV1().Jobs(job.Namespace)
	selector := fields.OneTermEqualSelector("metadata.name", job.Name).String()
	for {
		current, err := jobs.Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get container step Job %s: %w", job.Name, err)
		}
		if jobFinished(current) {
			return current, nil
		}
		w, err := jobs.Watch(ctx, metav1.ListOptions{FieldSelector: selector, ResourceVersion: current.ResourceVersion})
		if err != nil {
			return nil, fmt.Errorf("watch container step Job %s: %w", job.Name, err)
		}
		done, err := drainJobWatch(ctx, w)
		w.Stop()
		if err != nil || done != nil {
			return done, err
		}
	}
}

// drainJobWatch returns the finished Job, or (nil, nil) when the watch closed and must be re-established.
func drainJobWatch(ctx context.Context, w watch.Interface) (*batchv1.Job, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ev, ok := <-w.ResultChan():
			if !ok {
				return nil, nil
			}
			switch ev.Type {
			case watch.Deleted:
				return nil, fmt.Errorf("container step Job was deleted before it finished")
			case watch.Error:
				return nil, nil
			}
			if j, ok := ev.Object.(*batchv1.Job); ok && jobFinished(j) {
				return j, nil
			}
		}
	}
}

func containerStepPod(ctx context.Context, cs kubernetes.Interface, job *batchv1.Job) (*corev1.Pod, error) {
	pods, err := cs.CoreV1().Pods(job.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "batch.kubernetes.io/controller-uid=" + string(job.UID)})
	if err != nil {
		return nil, fmt.Errorf("list pods of container step Job %s: %w", job.Name, err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("container step Job %s has no pod (reason %q)", job.Name, jobFailedReason(job))
	}
	return &pods.Items[0], nil
}

func containerTerminated(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == containerStepContainerName {
			return s.State.Terminated
		}
	}
	return nil
}

// readContainerStdout reads the step container's stdout, failing rather than truncating past maxBytes.
// Clusters without the PodLogsQuerySplitStreams feature reject the stream option; there the log has
// stdout and stderr interleaved.
func readContainerStdout(ctx context.Context, cs kubernetes.Interface, pod *corev1.Pod, maxBytes int64) (string, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultContainerStepMaxOutputBytes
	}
	opts := &corev1.PodLogOptions{
		Container:  containerStepContainerName,
		LimitBytes: kptr.To(maxBytes + 1),
		Stream:     kptr.To(corev1.LogStreamStdout),
	}
	stream, err := cs.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, opts).Stream(ctx)
	if apierrors.IsInvalid(err) {
		opts.Stream = nil
		stream, err = cs.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, opts).Stream(ctx)
	}
	if err != nil {
		return "", fmt.Errorf("read logs of container step pod %s: %w", pod.Name, err)
	}
	defer stream.Close() //nolint:errcheck
	return readCapped(stream, maxBytes)
}

func readCapped(r io.Reader, maxBytes int64) (string, error) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, maxBytes+1))
	if err != nil {
		return "", fmt.Errorf("read container step output: %w", err)
	}
	if n > maxBytes {
		return "", fmt.Errorf("container step output exceeds the %d-byte limit", maxBytes)
	}
	return buf.String(), nil
}

// executeContainerStep runs step.container as a child Job and exposes {stdout, exitCode} as `step` to the step's outputs.
func (e *WorkflowExecutor) executeContainerStep(ctx context.Context, workflowRun *ottoflowv1alpha1.WorkflowRun, step ottoflowv1alpha1.Step) (map[string]interface{}, error) {
	cfg := e.containerStep
	if len(cfg.AllowedImages) == 0 {
		return nil, ErrContainerStepsDisabled
	}
	if cfg.Clientset == nil {
		return nil, fmt.Errorf("container step: no Kubernetes client configured")
	}

	contextData, err := e.contextManager.ReadContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read context: %w", err)
	}
	vars := e.celEvaluator.BuildVariableMap(contextData)

	resolved, err := e.resolveContainer(ctx, step.Container, vars)
	if err != nil {
		return nil, fmt.Errorf("container step: %w", err)
	}
	if !ImageAllowed(resolved.Image, cfg.AllowedImages) {
		return nil, fmt.Errorf("container step: image %q is not in the controller's container step allowlist", resolved.Image)
	}
	var secretName, secretKey string
	if resolved.Kubeconfig {
		if secretName, secretKey, err = containerKubeconfigSecret(workflowRun, cfg.KubeconfigKey); err != nil {
			return nil, fmt.Errorf("container step: %w", err)
		}
	}

	job := buildContainerStepJob(workflowRun, step.Name, resolved, secretName, secretKey)
	res, err := runContainerJob(ctx, cfg.Clientset, job, resolved.Timeout, cfg.MaxOutputBytes)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(resolved.SuccessCodes, res.ExitCode) {
		msg := strings.TrimSpace(res.Message)
		if len(msg) > 2048 {
			msg = msg[len(msg)-2048:]
		}
		return nil, fmt.Errorf("container step exited with code %d: %s", res.ExitCode, msg)
	}

	stepResult := map[string]interface{}{"stdout": res.Stdout, "exitCode": int64(res.ExitCode)}
	outputs := stepResult
	if len(step.Outputs) > 0 {
		outputVars := e.celEvaluator.BuildVariableMap(contextData)
		outputVars["step"] = stepResult
		if outputs, err = e.celEvaluator.EvaluateStepOutputs(ctx, step, outputVars); err != nil {
			return nil, fmt.Errorf("failed to evaluate step outputs: %w", err)
		}
	}
	if err := e.contextManager.WriteStepOutputs(ctx, step.Name, outputs); err != nil {
		return nil, fmt.Errorf("failed to write step outputs: %w", err)
	}
	return outputs, nil
}
