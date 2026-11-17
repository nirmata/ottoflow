/*
Copyright 2026 Nirmata, Inc.

Use of this source code is governed by the Business Source License 1.1
that can be found in the LICENSE.md file.
*/

package executor

import (
	"context"
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ottoflowv1alpha1 "github.com/nirmata/ottoflow/api/v1alpha1"
)

var _ = Describe("checkpointConfigMapName", func() {
	It("returns the name unchanged when it is 63 chars or fewer", func() {
		name := checkpointConfigMapName("my-run")
		Expect(name).To(Equal("ottoflow-cp-my-run"))
		Expect(len(name)).To(BeNumerically("<=", 63))
	})

	It("handles underscores by converting to hyphens", func() {
		name := checkpointConfigMapName("my_run_name")
		Expect(name).To(Equal("ottoflow-cp-my-run-name"))
	})

	It("truncates with a hash suffix when the base exceeds 63 chars", func() {
		// 64-char run name: "a" * 64
		longName := strings.Repeat("a", 64)
		result := checkpointConfigMapName(longName)
		Expect(len(result)).To(BeNumerically("<=", 63))
		// The result must contain a "-" separating prefix from hash suffix.
		Expect(result).To(ContainSubstring("-"))
		// The last segment after the final "-" must be all hex digits (the hash).
		lastDash := strings.LastIndex(result, "-")
		Expect(lastDash).To(BeNumerically(">=", 0))
		hashPart := result[lastDash+1:]
		Expect(hashPart).NotTo(BeEmpty())
		for _, r := range hashPart {
			Expect("0123456789abcdef").To(ContainSubstring(string(r)))
		}
	})

	It("produces the same result for the same input (deterministic)", func() {
		name := strings.Repeat("x", 64)
		Expect(checkpointConfigMapName(name)).To(Equal(checkpointConfigMapName(name)))
	})
})

var _ = Describe("auditConfigMapName", func() {
	It("uses a distinct prefix from checkpointConfigMapName", func() {
		name := auditConfigMapName("my-run")
		Expect(name).To(Equal("ottoflow-audit-my-run"))
		Expect(name).NotTo(Equal(checkpointConfigMapName("my-run")))
	})
})

var _ = Describe("NewCheckpointManager", func() {
	It("is enabled when checkpointing spec is set to true", func() {
		enabled := true
		workflowRun := &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: "my-run", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowRunSpec{
				Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
					Checkpointing: &ottoflowv1alpha1.CheckpointingConfig{
						Enabled: enabled,
					},
				},
			},
		}
		mgr := NewCheckpointManager(nil, workflowRun)
		Expect(mgr.Enabled()).To(BeTrue())
	})

	It("is disabled when Execution is nil", func() {
		workflowRun := &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: "my-run", Namespace: "default"},
			Spec:       ottoflowv1alpha1.WorkflowRunSpec{},
		}
		mgr := NewCheckpointManager(nil, workflowRun)
		Expect(mgr.Enabled()).To(BeFalse())
	})

	It("is disabled when Checkpointing is nil", func() {
		workflowRun := &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: "my-run", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowRunSpec{
				Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{},
			},
		}
		mgr := NewCheckpointManager(nil, workflowRun)
		Expect(mgr.Enabled()).To(BeFalse())
	})

	It("is disabled when Enabled is false", func() {
		workflowRun := &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: "my-run", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowRunSpec{
				Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
					Checkpointing: &ottoflowv1alpha1.CheckpointingConfig{
						Enabled: false,
					},
				},
			},
		}
		mgr := NewCheckpointManager(nil, workflowRun)
		Expect(mgr.Enabled()).To(BeFalse())
	})
})

var _ = Describe("CheckpointManager", func() {
	var (
		ctx         context.Context
		k8sClient   client.Client
		workflowRun *ottoflowv1alpha1.WorkflowRun
		scheme      *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))

		workflowRun = &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-run",
				Namespace: "default",
				UID:       types.UID("run-uid-1234"),
			},
			Spec: ottoflowv1alpha1.WorkflowRunSpec{
				Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
					Checkpointing: &ottoflowv1alpha1.CheckpointingConfig{
						Enabled: true,
					},
				},
			},
		}
	})

	newFakeClient := func(objs ...client.Object) client.Client {
		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	}

	Describe("Load", func() {
		It("returns nil, nil when the ConfigMap does not exist", func() {
			k8sClient = newFakeClient()
			mgr := NewCheckpointManager(k8sClient, workflowRun)

			snap, err := mgr.Load(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(snap).To(BeNil())
		})

		It("returns nil, nil when the stored UID does not match (stale checkpoint)", func() {
			// Build a snapshot belonging to a different run UID.
			staleSnapshot := CheckpointSnapshot{
				Version:           1,
				WorkflowRunUID:    "different-uid",
				LastCompletedStep: "step1",
				StepStatuses:      map[string]ottoflowv1alpha1.StepStatus{},
				Context:           map[string]interface{}{},
			}
			raw, err := json.Marshal(staleSnapshot)
			Expect(err).NotTo(HaveOccurred())

			cmName := checkpointConfigMapName(workflowRun.Name)
			staleCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      cmName,
					Namespace: workflowRun.Namespace,
				},
				Data: map[string]string{
					"checkpoint": string(raw),
				},
			}

			k8sClient = newFakeClient(staleCM)
			mgr := NewCheckpointManager(k8sClient, workflowRun)

			snap, err := mgr.Load(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(snap).To(BeNil())
		})

		It("returns the snapshot when ConfigMap exists and UIDs match", func() {
			snapshot := CheckpointSnapshot{
				Version:           1,
				WorkflowRunUID:    string(workflowRun.UID),
				LastCompletedStep: "step1",
				StepStatuses: map[string]ottoflowv1alpha1.StepStatus{
					"step1": {Phase: ottoflowv1alpha1.StepPhaseSucceeded},
				},
				Context: map[string]interface{}{"key": "value"},
			}
			raw, err := json.Marshal(snapshot)
			Expect(err).NotTo(HaveOccurred())

			cmName := checkpointConfigMapName(workflowRun.Name)
			existingCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      cmName,
					Namespace: workflowRun.Namespace,
				},
				Data: map[string]string{
					"checkpoint": string(raw),
				},
			}

			k8sClient = newFakeClient(existingCM)
			mgr := NewCheckpointManager(k8sClient, workflowRun)

			snap, err := mgr.Load(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(snap).NotTo(BeNil())
			Expect(snap.WorkflowRunUID).To(Equal(string(workflowRun.UID)))
			Expect(snap.LastCompletedStep).To(Equal("step1"))
		})

		It("returns nil, nil (no-op) when checkpointing is disabled", func() {
			workflowRun.Spec.Execution.Checkpointing.Enabled = false
			k8sClient = newFakeClient()
			mgr := NewCheckpointManager(k8sClient, workflowRun)

			snap, err := mgr.Load(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(snap).To(BeNil())
		})

		// CEL types int64 as int and float64 as double, and has no int + double overload,
		// so a restored context must keep the Go number types it was saved with.
		Context("number types across SaveAsync and Load", func() {
			// numberContext holds an int, a whole float and a float written with an exponent,
			// at the top level, inside a nested map and inside a slice.
			numberContext := func() map[string]interface{} {
				return map[string]interface{}{
					"count": int64(2), "ratio": float64(2), "big": float64(1e21),
					"nested": map[string]interface{}{
						"count": int64(2), "ratio": float64(2), "big": float64(1e21),
					},
					"list": []interface{}{int64(2), float64(2), float64(1e21)},
				}
			}

			save := func(live map[string]interface{}) *CheckpointManager {
				k8sClient = newFakeClient()
				mgr := NewCheckpointManager(k8sClient, workflowRun)
				mgr.SaveAsync(ctx, CheckpointSnapshot{
					Version:        1,
					WorkflowRunUID: string(workflowRun.UID),
					StepStatuses:   map[string]ottoflowv1alpha1.StepStatus{},
					Context:        live,
				})
				Expect(mgr.Flush(ctx)).To(Succeed())
				return mgr
			}

			load := func(mgr *CheckpointManager) map[string]interface{} {
				snap, err := mgr.Load(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(snap).NotTo(BeNil())
				return snap.Context
			}

			It("restores integers as int64 and floats as float64", func() {
				Expect(load(save(numberContext()))).To(Equal(numberContext()))
			})

			It("restores a prometheusQuery result unchanged", func() {
				result := prometheusResultToMap(&vectorResult{vector: model.Vector{
					{Metric: model.Metric{"job": "a"}, Value: 42, Timestamp: model.Time(1000)},
				}})
				Expect(load(save(map[string]interface{}{"result": result}))).To(Equal(map[string]interface{}{"result": result}))
			})

			It("leaves the caller's context map unchanged", func() {
				live := numberContext()
				save(live)
				Expect(live).To(Equal(numberContext()))
			})
		})

		It("returns an error when data follows the snapshot", func() {
			raw, err := json.Marshal(CheckpointSnapshot{
				Version:        1,
				WorkflowRunUID: string(workflowRun.UID),
				Context:        map[string]interface{}{"key": "value"},
			})
			Expect(err).NotTo(HaveOccurred())

			for _, trailing := range []string{"}x", " null"} {
				existingCM := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      checkpointConfigMapName(workflowRun.Name),
						Namespace: workflowRun.Namespace,
					},
					Data: map[string]string{"checkpoint": string(raw) + trailing},
				}
				k8sClient = newFakeClient(existingCM)
				mgr := NewCheckpointManager(k8sClient, workflowRun)

				snap, err := mgr.Load(ctx)
				Expect(err).To(MatchError(ContainSubstring("failed to unmarshal checkpoint data")), "trailing %q", trailing)
				Expect(snap).To(BeNil(), "trailing %q", trailing)
			}
		})
	})

	Describe("Flush", func() {
		It("returns nil immediately when there are no pending writes", func() {
			k8sClient = newFakeClient()
			mgr := NewCheckpointManager(k8sClient, workflowRun)

			err := mgr.Flush(ctx)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Delete", func() {
		It("returns nil when the ConfigMap does not exist (idempotent)", func() {
			k8sClient = newFakeClient()
			mgr := NewCheckpointManager(k8sClient, workflowRun)

			Expect(mgr.Delete(ctx)).To(Succeed())
		})

		It("deletes the ConfigMap when it exists", func() {
			cmName := checkpointConfigMapName(workflowRun.Name)
			existingCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      cmName,
					Namespace: workflowRun.Namespace,
				},
				Data: map[string]string{"checkpoint": "{}"},
			}

			k8sClient = newFakeClient(existingCM)
			mgr := NewCheckpointManager(k8sClient, workflowRun)

			Expect(mgr.Delete(ctx)).To(Succeed())

			// Verify it is gone.
			cm := &corev1.ConfigMap{}
			err := k8sClient.Get(ctx, client.ObjectKey{Name: cmName, Namespace: workflowRun.Namespace}, cm)
			Expect(err).To(HaveOccurred())
		})

		It("is a no-op when checkpointing is disabled", func() {
			workflowRun.Spec.Execution.Checkpointing.Enabled = false
			k8sClient = newFakeClient()
			mgr := NewCheckpointManager(k8sClient, workflowRun)

			Expect(mgr.Delete(ctx)).To(Succeed())
		})
	})

	Describe("SaveAuditSnapshot", func() {
		It("writes the snapshot even when checkpointing is disabled", func() {
			workflowRun.Spec.Execution.Checkpointing.Enabled = false
			k8sClient = newFakeClient()
			mgr := NewCheckpointManager(k8sClient, workflowRun)
			Expect(mgr.Enabled()).To(BeFalse())

			name, err := mgr.SaveAuditSnapshot(ctx, CheckpointSnapshot{
				Version:        1,
				WorkflowRunUID: string(workflowRun.UID),
				Context:        map[string]interface{}{"variables": map[string]interface{}{"v1": "done"}},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(name).To(Equal(auditConfigMapName(workflowRun.Name)))

			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: workflowRun.Namespace}, cm)).To(Succeed())
			Expect(cm.Data["snapshot"]).To(ContainSubstring("done"))
			Expect(cm.OwnerReferences).To(HaveLen(1))
			Expect(cm.OwnerReferences[0].Name).To(Equal(workflowRun.Name))
		})

		It("overwrites an existing audit ConfigMap on a second call", func() {
			k8sClient = newFakeClient()
			mgr := NewCheckpointManager(k8sClient, workflowRun)

			_, err := mgr.SaveAuditSnapshot(ctx, CheckpointSnapshot{
				Version: 1, WorkflowRunUID: string(workflowRun.UID),
				Context: map[string]interface{}{"variables": map[string]interface{}{"v1": "first"}},
			})
			Expect(err).NotTo(HaveOccurred())

			name, err := mgr.SaveAuditSnapshot(ctx, CheckpointSnapshot{
				Version: 1, WorkflowRunUID: string(workflowRun.UID),
				Context: map[string]interface{}{"variables": map[string]interface{}{"v1": "second"}},
			})
			Expect(err).NotTo(HaveOccurred())

			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: workflowRun.Namespace}, cm)).To(Succeed())
			Expect(cm.Data["snapshot"]).To(ContainSubstring("second"))
			Expect(cm.Data["snapshot"]).NotTo(ContainSubstring("first"))
		})

		It("truncates large array-valued context variables so the write stays small", func() {
			k8sClient = newFakeClient()
			mgr := NewCheckpointManager(k8sClient, workflowRun)

			bigArr := make([]interface{}, auditSnapshotMaxArrayEntries*10)
			for i := range bigArr {
				// ~2KB payload each so the marshaled snapshot comfortably exceeds the threshold.
				bigArr[i] = map[string]interface{}{"name": strings.Repeat("x", 2048), "index": i}
			}

			name, err := mgr.SaveAuditSnapshot(ctx, CheckpointSnapshot{
				Version:        1,
				WorkflowRunUID: string(workflowRun.UID),
				Context:        map[string]interface{}{"variables": map[string]interface{}{"pods": bigArr}},
			})
			Expect(err).NotTo(HaveOccurred())

			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: workflowRun.Namespace}, cm)).To(Succeed())
			Expect(len(cm.Data["snapshot"])).To(BeNumerically("<", auditSnapshotSizeThreshold))

			var saved CheckpointSnapshot
			Expect(json.Unmarshal([]byte(cm.Data["snapshot"]), &saved)).To(Succeed())
			vars := saved.Context["variables"].(map[string]interface{})
			pods := vars["pods"].(map[string]interface{})
			Expect(pods["truncated"]).To(Equal(true))
			Expect(pods["originalCount"]).To(Equal(float64(len(bigArr))))
			Expect(pods["keptCount"]).To(Equal(float64(auditSnapshotMaxArrayEntries)))
		})
	})

	Describe("SaveAsync", func() {
		// Regression test: ExecuteWorkflow initializes a StepStatuses entry for every workflow
		// step up front (all Pending) before the first checkpoint save ever happens, so
		// len(StepStatuses) is constant for the entire life of a run. Comparing that total length
		// (the old guard) therefore ties on the very first comparison after the ConfigMap is
		// created, and every later save — including the one that should have replaced it — is
		// silently dropped, permanently freezing the checkpoint (and its CompletionOrder) at
		// whichever snapshot happened to win the Create. Comparing terminal step count instead
		// fixes this, since that count strictly increases as steps actually complete.
		It("replaces an existing checkpoint when terminal step count increases, even though total StepStatuses length stays the same", func() {
			k8sClient = newFakeClient()
			mgr := NewCheckpointManager(k8sClient, workflowRun)

			first := CheckpointSnapshot{
				Version:           1,
				WorkflowRunUID:    string(workflowRun.UID),
				LastCompletedStep: "step1",
				StepStatuses: map[string]ottoflowv1alpha1.StepStatus{
					"step1": {Phase: ottoflowv1alpha1.StepPhaseSucceeded},
					"step2": {Phase: ottoflowv1alpha1.StepPhasePending},
				},
				CompletionOrder: []string{"step1"},
			}
			mgr.SaveAsync(ctx, first)
			Expect(mgr.Flush(ctx)).To(Succeed())

			// Same total StepStatuses length (2) as "first", but step2 is now terminal too —
			// this must be recognized as more advanced and must win.
			second := CheckpointSnapshot{
				Version:           1,
				WorkflowRunUID:    string(workflowRun.UID),
				LastCompletedStep: "step2",
				StepStatuses: map[string]ottoflowv1alpha1.StepStatus{
					"step1": {Phase: ottoflowv1alpha1.StepPhaseSucceeded},
					"step2": {Phase: ottoflowv1alpha1.StepPhaseSucceeded},
				},
				CompletionOrder: []string{"step1", "step2"},
			}
			mgr.SaveAsync(ctx, second)
			Expect(mgr.Flush(ctx)).To(Succeed())

			loaded, err := mgr.Load(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())
			Expect(loaded.LastCompletedStep).To(Equal("step2"))
			Expect(loaded.CompletionOrder).To(Equal([]string{"step1", "step2"}))
		})

		It("does not let a lower terminal-count save downgrade a checkpoint that already reflects more progress", func() {
			k8sClient = newFakeClient()
			mgr := NewCheckpointManager(k8sClient, workflowRun)

			newer := CheckpointSnapshot{
				Version:           1,
				WorkflowRunUID:    string(workflowRun.UID),
				LastCompletedStep: "step2",
				StepStatuses: map[string]ottoflowv1alpha1.StepStatus{
					"step1": {Phase: ottoflowv1alpha1.StepPhaseSucceeded},
					"step2": {Phase: ottoflowv1alpha1.StepPhaseSucceeded},
				},
				CompletionOrder: []string{"step1", "step2"},
			}
			mgr.SaveAsync(ctx, newer)
			Expect(mgr.Flush(ctx)).To(Succeed())

			// A late-arriving write for an older, less-advanced snapshot (e.g. a delayed retry)
			// must not overwrite the already-persisted, more-advanced checkpoint.
			stale := CheckpointSnapshot{
				Version:           1,
				WorkflowRunUID:    string(workflowRun.UID),
				LastCompletedStep: "step1",
				StepStatuses: map[string]ottoflowv1alpha1.StepStatus{
					"step1": {Phase: ottoflowv1alpha1.StepPhaseSucceeded},
					"step2": {Phase: ottoflowv1alpha1.StepPhasePending},
				},
				CompletionOrder: []string{"step1"},
			}
			mgr.SaveAsync(ctx, stale)
			Expect(mgr.Flush(ctx)).To(Succeed())

			loaded, err := mgr.Load(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())
			Expect(loaded.LastCompletedStep).To(Equal("step2"))
			Expect(loaded.CompletionOrder).To(Equal([]string{"step1", "step2"}))
		})
	})
})

var _ = Describe("truncateAuditSnapshot", func() {
	It("leaves small arrays, scalars, and non-map sections untouched", func() {
		snapshot := CheckpointSnapshot{
			Context: map[string]interface{}{
				"variables": map[string]interface{}{
					"small":  []interface{}{"a", "b"},
					"scalar": "value",
				},
				"lastCompletedStep": "step1",
			},
		}
		out := truncateAuditSnapshot(snapshot)
		Expect(out.Context).To(Equal(snapshot.Context))
	})

	It("truncates only array values exceeding auditSnapshotMaxArrayEntries, one level into each section", func() {
		big := make([]interface{}, auditSnapshotMaxArrayEntries+10)
		for i := range big {
			big[i] = i
		}
		snapshot := CheckpointSnapshot{
			Context: map[string]interface{}{
				"variables": map[string]interface{}{"pods": big, "other": "kept-as-is"},
			},
		}
		out := truncateAuditSnapshot(snapshot)

		vars := out.Context["variables"].(map[string]interface{})
		Expect(vars["other"]).To(Equal("kept-as-is"))
		truncated := vars["pods"].(map[string]interface{})
		Expect(truncated["truncated"]).To(Equal(true))
		Expect(truncated["originalCount"]).To(Equal(len(big)))
		Expect(truncated["keptCount"]).To(Equal(auditSnapshotMaxArrayEntries))
		Expect(truncated["items"].([]interface{})).To(HaveLen(auditSnapshotMaxArrayEntries))
		Expect(out.Context["_auditSnapshotTruncatedKeys"]).To(ConsistOf("variables.pods"))
	})
})

var _ = Describe("truncateAuditSnapshot with nested map-shaped sections", func() {
	It("truncates large arrays nested under a forEach-shaped steps entry (map, not array, one level down)", func() {
		big := make([]interface{}, auditSnapshotMaxArrayEntries+25)
		for i := range big {
			big[i] = map[string]interface{}{"item": i, "outputs": map[string]interface{}{"result": i}}
		}
		snapshot := CheckpointSnapshot{
			Context: map[string]interface{}{
				"steps": map[string]interface{}{
					"collectNamespaceResources": map[string]interface{}{
						"results":   big,
						"succeeded": big,
						"failed":    []interface{}{},
					},
				},
			},
		}
		out := truncateAuditSnapshot(snapshot)

		stepEntry := out.Context["steps"].(map[string]interface{})["collectNamespaceResources"].(map[string]interface{})
		succeeded := stepEntry["succeeded"].(map[string]interface{})
		Expect(succeeded["truncated"]).To(Equal(true))
		Expect(succeeded["originalCount"]).To(Equal(len(big)))
		Expect(succeeded["items"].([]interface{})).To(HaveLen(auditSnapshotMaxArrayEntries))

		results := stepEntry["results"].(map[string]interface{})
		Expect(results["truncated"]).To(Equal(true))

		Expect(out.Context["_auditSnapshotTruncatedKeys"]).To(ConsistOf(
			"steps.collectNamespaceResources.results",
			"steps.collectNamespaceResources.succeeded",
		))
	})
})

var _ = Describe("redactSensitiveContext", func() {
	It("redacts a sensitive workflow-level output but leaves non-sensitive ones intact", func() {
		ctxData := map[string]interface{}{
			"outputs": map[string]interface{}{
				"apiToken": "super-secret",
				"summary":  "ok",
			},
		}
		redacted := redactSensitiveContext(ctxData, map[string]bool{"apiToken": true})

		outputs := redacted["outputs"].(map[string]interface{})
		Expect(outputs["apiToken"]).To(Equal(sensitiveRedactedPlaceholder))
		Expect(outputs["summary"]).To(Equal("ok"))
		// The original, live context must be untouched (only the copy is redacted).
		Expect(ctxData["outputs"].(map[string]interface{})["apiToken"]).To(Equal("super-secret"))
	})

	It("redacts a sensitive step-level output flattened into variables", func() {
		ctxData := map[string]interface{}{
			"variables": map[string]interface{}{
				"dbPassword": "hunter2",
				"count":      3,
			},
		}
		redacted := redactSensitiveContext(ctxData, map[string]bool{"dbPassword": true})

		vars := redacted["variables"].(map[string]interface{})
		Expect(vars["dbPassword"]).To(Equal(sensitiveRedactedPlaceholder))
		Expect(vars["count"]).To(Equal(3))
	})

	It("redacts sensitive keys inside forEach per-item outputs nested under steps", func() {
		ctxData := map[string]interface{}{
			"steps": map[string]interface{}{
				"fetchSecrets": map[string]interface{}{
					"results": []interface{}{
						map[string]interface{}{
							"item":   "a",
							"status": "succeeded",
							"outputs": map[string]interface{}{
								"secret": "leak-me-not",
								"name":   "a",
							},
						},
					},
				},
			},
		}
		redacted := redactSensitiveContext(ctxData, map[string]bool{"secret": true})

		results := redacted["steps"].(map[string]interface{})["fetchSecrets"].(map[string]interface{})["results"].([]interface{})
		item := results[0].(map[string]interface{})
		outputs := item["outputs"].(map[string]interface{})
		Expect(outputs["secret"]).To(Equal(sensitiveRedactedPlaceholder))
		Expect(outputs["name"]).To(Equal("a"))
	})

	It("is a no-op copy when there are no sensitive names", func() {
		ctxData := map[string]interface{}{"variables": map[string]interface{}{"v1": "value"}}
		redacted := redactSensitiveContext(ctxData, map[string]bool{})
		Expect(redacted).To(Equal(ctxData))
	})
})

var _ = Describe("ExecuteWorkflow checkpoint restore", func() {
	var (
		ctx       context.Context
		scheme    *runtime.Scheme
		wfRunName = "restore-run"
		wfRunUID  = types.UID("uid-restore-1")
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		utilruntime.Must(ottoflowv1alpha1.AddToScheme(scheme))
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	})

	buildCheckpointCM := func(wfrName string, snap CheckpointSnapshot) *corev1.ConfigMap {
		raw, err := json.Marshal(snap)
		Expect(err).NotTo(HaveOccurred())
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: checkpointConfigMapName(wfrName), Namespace: "default"},
			Data:       map[string]string{"checkpoint": string(raw)},
		}
	}

	It("restores from checkpoint and skips completed steps when Attempts > 0", func() {
		snapshot := CheckpointSnapshot{
			Version:           1,
			WorkflowRunUID:    string(wfRunUID),
			LastCompletedStep: "step1",
			StepStatuses: map[string]ottoflowv1alpha1.StepStatus{
				"step1": {Phase: ottoflowv1alpha1.StepPhaseSucceeded},
			},
			Context: map[string]interface{}{
				"inputs":    map[string]interface{}{},
				"variables": map[string]interface{}{"v1": "restored"},
				"steps":     map[string]interface{}{},
			},
		}
		cm := buildCheckpointCM(wfRunName, snapshot)
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

		workflowRun := &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: wfRunName, Namespace: "default", UID: wfRunUID},
			Spec: ottoflowv1alpha1.WorkflowRunSpec{
				WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"},
				Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
					Checkpointing: &ottoflowv1alpha1.CheckpointingConfig{Enabled: true},
				},
			},
			Status: ottoflowv1alpha1.WorkflowRunStatus{
				Phase: ottoflowv1alpha1.WorkflowRunPhasePending,
				Execution: &ottoflowv1alpha1.WorkflowRunExecutionStatus{
					Attempts: 1,
				},
			},
		}

		exec, err := NewWorkflowExecutorWithMetrics(k8sClient, nil, nil, nil, workflowRun, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		exec.SetCheckpointManager(NewCheckpointManager(k8sClient, workflowRun))

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{Name: "step1", Outputs: []ottoflowv1alpha1.Output{{Name: "v1", Expression: `"fresh"`}}},
					{Name: "step2", DependsOn: []string{"step1"}, Outputs: []ottoflowv1alpha1.Output{{Name: "v2", Expression: `variables.v1 + "-step2"`}}},
				},
			},
		}

		Expect(exec.ExecuteWorkflow(ctx, workflow, workflowRun)).To(Succeed())

		// step1 status preserved from checkpoint (phase Succeeded from snapshot)
		Expect(workflowRun.Status.StepStatuses["step1"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))
		// step2 ran fresh and used the restored variable "restored" from the checkpoint context
		Expect(workflowRun.Status.StepStatuses["step2"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))
		// The variable v2 should use the restored v1 ("restored"), not the fresh expression result ("fresh")
		ctx2, _ := exec.contextManager.ReadContext(ctx)
		Expect(ctx2["variables"].(map[string]interface{})["v2"]).To(Equal("restored-step2"))
	})

	It("runs from scratch (no restore) when Attempts == 0 and Phase is Pending", func() {
		// Even if a checkpoint CM exists with matching UID, Attempts=0 + Pending → no restore.
		snapshot := CheckpointSnapshot{
			Version:        1,
			WorkflowRunUID: string(wfRunUID),
			StepStatuses: map[string]ottoflowv1alpha1.StepStatus{
				"step1": {Phase: ottoflowv1alpha1.StepPhaseSucceeded},
			},
			Context: map[string]interface{}{
				"inputs":    map[string]interface{}{},
				"variables": map[string]interface{}{"v1": "from-checkpoint"},
				"steps":     map[string]interface{}{},
			},
		}
		cm := buildCheckpointCM(wfRunName, snapshot)
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

		workflowRun := &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: wfRunName, Namespace: "default", UID: wfRunUID},
			Spec: ottoflowv1alpha1.WorkflowRunSpec{
				WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"},
				Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
					Checkpointing: &ottoflowv1alpha1.CheckpointingConfig{Enabled: true},
				},
			},
			Status: ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
		}

		exec, err := NewWorkflowExecutorWithMetrics(k8sClient, nil, nil, nil, workflowRun, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		exec.SetCheckpointManager(NewCheckpointManager(k8sClient, workflowRun))

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{Name: "step1", Outputs: []ottoflowv1alpha1.Output{{Name: "v1", Expression: `"fresh"`}}},
				},
			},
		}

		Expect(exec.ExecuteWorkflow(ctx, workflow, workflowRun)).To(Succeed())
		ctx2, _ := exec.contextManager.ReadContext(ctx)
		// v1 should be "fresh" (not "from-checkpoint") since no restore happened
		Expect(ctx2["variables"].(map[string]interface{})["v1"]).To(Equal("fresh"))
	})

	It("writes an audit snapshot on completion even when per-step checkpointing is disabled", func() {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

		workflowRun := &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: "audit-run", Namespace: "default", UID: types.UID("uid-audit-1")},
			Spec: ottoflowv1alpha1.WorkflowRunSpec{
				WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"},
				// No Execution/Checkpointing set at all: per-step checkpointing is disabled.
			},
			Status: ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
		}

		exec, err := NewWorkflowExecutorWithMetrics(k8sClient, nil, nil, nil, workflowRun, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		exec.SetCheckpointManager(NewCheckpointManager(k8sClient, workflowRun))
		Expect(exec.checkpointManager.Enabled()).To(BeFalse())

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{Name: "step1", Outputs: []ottoflowv1alpha1.Output{{Name: "v1", Expression: `"done"`}}},
				},
			},
		}

		Expect(exec.ExecuteWorkflow(ctx, workflow, workflowRun)).To(Succeed())
		Expect(workflowRun.Status.Phase).To(Equal(ottoflowv1alpha1.WorkflowRunPhaseSucceeded))
		Expect(workflowRun.Status.AuditSnapshotConfigMap).To(Equal(auditConfigMapName(workflowRun.Name)))

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{
			Name: workflowRun.Status.AuditSnapshotConfigMap, Namespace: workflowRun.Namespace,
		}, cm)).To(Succeed())
		Expect(cm.Data["snapshot"]).To(ContainSubstring("done"))

		// No checkpoint ConfigMap should exist since per-step checkpointing was disabled.
		checkpointCM := &corev1.ConfigMap{}
		err = k8sClient.Get(ctx, client.ObjectKey{
			Name: checkpointConfigMapName(workflowRun.Name), Namespace: workflowRun.Namespace,
		}, checkpointCM)
		Expect(err).To(HaveOccurred())
	})

	It("does not persist a sensitive:true workflow output's raw value in the audit snapshot", func() {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

		workflowRun := &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: "sensitive-run", Namespace: "default", UID: types.UID("uid-sensitive-1")},
			Spec: ottoflowv1alpha1.WorkflowRunSpec{
				WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"},
			},
			Status: ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
		}

		exec, err := NewWorkflowExecutorWithMetrics(k8sClient, nil, nil, nil, workflowRun, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		exec.SetCheckpointManager(NewCheckpointManager(k8sClient, workflowRun))

		workflow := &ottoflowv1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "default"},
			Spec: ottoflowv1alpha1.WorkflowSpec{
				Steps: []ottoflowv1alpha1.Step{
					{Name: "step1", Outputs: []ottoflowv1alpha1.Output{{Name: "v1", Expression: `"public-value"`}}},
				},
				Outputs: []ottoflowv1alpha1.Output{
					{Name: "apiToken", Expression: `"super-secret-token"`, Sensitive: true},
				},
			},
		}

		Expect(exec.ExecuteWorkflow(ctx, workflow, workflowRun)).To(Succeed())
		Expect(workflowRun.Status.AuditSnapshotConfigMap).To(Equal(auditConfigMapName(workflowRun.Name)))

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{
			Name: workflowRun.Status.AuditSnapshotConfigMap, Namespace: workflowRun.Namespace,
		}, cm)).To(Succeed())

		Expect(cm.Data["snapshot"]).NotTo(ContainSubstring("super-secret-token"))

		var saved CheckpointSnapshot
		Expect(json.Unmarshal([]byte(cm.Data["snapshot"]), &saved)).To(Succeed())
		outputs := saved.Context["outputs"].(map[string]interface{})
		Expect(outputs["apiToken"]).To(Equal(sensitiveRedactedPlaceholder))
		vars := saved.Context["variables"].(map[string]interface{})
		Expect(vars["v1"]).To(Equal("public-value"))
	})

	// These two cases call loadCheckpointIfNeeded directly (unexported, same package) rather
	// than going through ExecuteWorkflow. They pin down the precondition the controller's
	// waitForCallback resume fix depends on: setting Status.Phase=Running (with Attempts still
	// 0, since a callback resume is not a restart-attempt retry) must be sufficient on its own
	// to trigger a checkpoint restore when the runner Job is recreated after a callback.
	It("loadCheckpointIfNeeded returns true and restores state when Phase is Running (post-callback resume)", func() {
		snapshot := CheckpointSnapshot{
			Version:           1,
			WorkflowRunUID:    string(wfRunUID),
			LastCompletedStep: "pre",
			StepStatuses: map[string]ottoflowv1alpha1.StepStatus{
				"pre": {Phase: ottoflowv1alpha1.StepPhaseSucceeded},
			},
			Context: map[string]interface{}{
				"inputs":    map[string]interface{}{},
				"variables": map[string]interface{}{"v1": "restored-running"},
				"steps":     map[string]interface{}{},
			},
		}
		cm := buildCheckpointCM(wfRunName, snapshot)
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

		workflowRun := &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: wfRunName, Namespace: "default", UID: wfRunUID},
			Spec: ottoflowv1alpha1.WorkflowRunSpec{
				WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"},
				Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
					Checkpointing: &ottoflowv1alpha1.CheckpointingConfig{Enabled: true},
				},
			},
			// Attempts == 0 (default), but Phase == Running — exactly the state the controller
			// now persists before recreating the runner Job to resume a waitForCallback step.
			Status: ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhaseRunning},
		}

		exec, err := NewWorkflowExecutorWithMetrics(k8sClient, nil, nil, nil, workflowRun, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		exec.SetCheckpointManager(NewCheckpointManager(k8sClient, workflowRun))

		restored, err := exec.loadCheckpointIfNeeded(ctx, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(restored).To(BeTrue())
		Expect(workflowRun.Status.StepStatuses["pre"].Phase).To(Equal(ottoflowv1alpha1.StepPhaseSucceeded))
		ctx2, _ := exec.contextManager.ReadContext(ctx)
		Expect(ctx2["variables"].(map[string]interface{})["v1"]).To(Equal("restored-running"))
	})

	It("loadCheckpointIfNeeded returns false when Phase is Pending and Attempts is 0, even if a checkpoint exists", func() {
		snapshot := CheckpointSnapshot{
			Version:        1,
			WorkflowRunUID: string(wfRunUID),
			StepStatuses: map[string]ottoflowv1alpha1.StepStatus{
				"pre": {Phase: ottoflowv1alpha1.StepPhaseSucceeded},
			},
			Context: map[string]interface{}{},
		}
		cm := buildCheckpointCM(wfRunName, snapshot)
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

		workflowRun := &ottoflowv1alpha1.WorkflowRun{
			ObjectMeta: metav1.ObjectMeta{Name: wfRunName, Namespace: "default", UID: wfRunUID},
			Spec: ottoflowv1alpha1.WorkflowRunSpec{
				WorkflowRef: ottoflowv1alpha1.WorkflowRef{Name: "wf"},
				Execution: &ottoflowv1alpha1.WorkflowRunExecutionSpec{
					Checkpointing: &ottoflowv1alpha1.CheckpointingConfig{Enabled: true},
				},
			},
			Status: ottoflowv1alpha1.WorkflowRunStatus{Phase: ottoflowv1alpha1.WorkflowRunPhasePending},
		}

		exec, err := NewWorkflowExecutorWithMetrics(k8sClient, nil, nil, nil, workflowRun, 0, 5, nil)
		Expect(err).NotTo(HaveOccurred())
		exec.SetCheckpointManager(NewCheckpointManager(k8sClient, workflowRun))

		restored, err := exec.loadCheckpointIfNeeded(ctx, workflowRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(restored).To(BeFalse())
		Expect(workflowRun.Status.StepStatuses).To(BeEmpty())
	})
})
