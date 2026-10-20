package providers

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// Readiness is checked against what `kubectl rollout status` treats as a
// finished rollout, not against kraze's own implementation. Each case is a
// status snapshot a real controller produces at some point of a rollout.

// workload builds an object of kind at metadata.generation, with spec and
// status (nil status: the controller hasn't written one yet)
func workload(kind string, generation int64, spec, status map[string]interface{}) *unstructured.Unstructured {
	obj := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": "w", "namespace": "ns", "generation": generation},
		"spec":       spec,
	}
	if status != nil {
		obj["status"] = status
	}
	return &unstructured.Unstructured{Object: obj}
}

type readinessCase struct {
	name       string
	obj        *unstructured.Unstructured
	wantReady  bool
	wantFailed bool // a terminal failure: waiting longer can't help
}

func runReadinessCases(t *testing.T, cases []readinessCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ready, err := isResourceReady(tc.obj, tc.obj.GetKind())
			if tc.wantFailed {
				if !errors.Is(err, errResourceFailed) {
					t.Errorf("want a terminal failure, got ready=%v err=%v", ready, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ready != tc.wantReady {
				t.Errorf("ready = %v, want %v", ready, tc.wantReady)
			}
		})
	}
}

func replicas(n int64) map[string]interface{} { return map[string]interface{}{"replicas": n} }

func TestDeploymentReadiness(t *testing.T) {
	runReadinessCases(t, []readinessCase{
		{name: "rolled out",
			obj: workload("Deployment", 2, replicas(2), map[string]interface{}{
				"observedGeneration": int64(2), "replicas": int64(2), "updatedReplicas": int64(2), "availableReplicas": int64(2)}),
			wantReady: true},
		{name: "replicas defaults to 1",
			obj: workload("Deployment", 1, map[string]interface{}{}, map[string]interface{}{
				"observedGeneration": int64(1), "replicas": int64(1), "updatedReplicas": int64(1), "availableReplicas": int64(1)}),
			wantReady: true},
		{name: "no status yet", obj: workload("Deployment", 1, replicas(1), nil)},
		{name: "spec changed, controller hasn't seen it (status describes the old version)",
			obj: workload("Deployment", 3, replicas(1), map[string]interface{}{
				"observedGeneration": int64(2), "replicas": int64(1), "updatedReplicas": int64(1), "availableReplicas": int64(1)})},
		{name: "surge: new pod created but not available, old pod still serving",
			obj: workload("Deployment", 2, replicas(1), map[string]interface{}{
				"observedGeneration": int64(2), "replicas": int64(2), "updatedReplicas": int64(1), "availableReplicas": int64(1)})},
		{name: "new pods available, old ones still terminating",
			obj: workload("Deployment", 2, replicas(2), map[string]interface{}{
				"observedGeneration": int64(2), "replicas": int64(3), "updatedReplicas": int64(2), "availableReplicas": int64(3)})},
		{name: "scaled to zero (the controller omits zero counts)",
			obj:       workload("Deployment", 1, replicas(0), map[string]interface{}{"observedGeneration": int64(1)}),
			wantReady: true},
		{name: "progress deadline exceeded",
			obj: workload("Deployment", 2, replicas(1), map[string]interface{}{
				"observedGeneration": int64(2), "replicas": int64(2), "updatedReplicas": int64(1), "availableReplicas": int64(1),
				"conditions": []interface{}{map[string]interface{}{"type": "Progressing", "status": "False", "reason": "ProgressDeadlineExceeded"}}}),
			wantFailed: true},
	})
}

func TestStatefulSetReadiness(t *testing.T) {
	rolling := func(partition int64) map[string]interface{} {
		return map[string]interface{}{"replicas": int64(3), "updateStrategy": map[string]interface{}{
			"type": "RollingUpdate", "rollingUpdate": map[string]interface{}{"partition": partition}}}
	}
	runReadinessCases(t, []readinessCase{
		{name: "rolled out",
			obj: workload("StatefulSet", 1, replicas(2), map[string]interface{}{
				"observedGeneration": int64(1), "replicas": int64(2), "readyReplicas": int64(2), "updatedReplicas": int64(2),
				"currentRevision": "web-1", "updateRevision": "web-1"}),
			wantReady: true},
		{name: "no status yet", obj: workload("StatefulSet", 1, replicas(1), nil)},
		{name: "spec changed, controller hasn't seen it",
			obj: workload("StatefulSet", 2, replicas(2), map[string]interface{}{
				"observedGeneration": int64(1), "replicas": int64(2), "readyReplicas": int64(2), "updatedReplicas": int64(2),
				"currentRevision": "web-1", "updateRevision": "web-1"})},
		{name: "rolling update in progress: all ready, one pod still on the old revision",
			obj: workload("StatefulSet", 2, replicas(2), map[string]interface{}{
				"observedGeneration": int64(2), "replicas": int64(2), "readyReplicas": int64(2), "updatedReplicas": int64(1),
				"currentRevision": "web-1", "updateRevision": "web-2"})},
		{name: "not enough ready",
			obj: workload("StatefulSet", 1, replicas(3), map[string]interface{}{
				"observedGeneration": int64(1), "replicas": int64(3), "readyReplicas": int64(2),
				"currentRevision": "web-1", "updateRevision": "web-1"})},
		{name: "partitioned rollout done (pods below the partition stay on the old revision)",
			obj: workload("StatefulSet", 2, rolling(1), map[string]interface{}{
				"observedGeneration": int64(2), "replicas": int64(3), "readyReplicas": int64(3), "updatedReplicas": int64(2),
				"currentRevision": "web-1", "updateRevision": "web-2"}),
			wantReady: true},
		{name: "partitioned rollout not done",
			obj: workload("StatefulSet", 2, rolling(1), map[string]interface{}{
				"observedGeneration": int64(2), "replicas": int64(3), "readyReplicas": int64(3), "updatedReplicas": int64(1),
				"currentRevision": "web-1", "updateRevision": "web-2"})},
		{name: "scaled to zero",
			obj:       workload("StatefulSet", 1, replicas(0), map[string]interface{}{"observedGeneration": int64(1), "replicas": int64(0)}),
			wantReady: true},
	})
}

func TestDaemonSetReadiness(t *testing.T) {
	runReadinessCases(t, []readinessCase{
		{name: "rolled out",
			obj: workload("DaemonSet", 1, map[string]interface{}{}, map[string]interface{}{
				"observedGeneration": int64(1), "desiredNumberScheduled": int64(2), "updatedNumberScheduled": int64(2),
				"numberReady": int64(2), "numberAvailable": int64(2)}),
			wantReady: true},
		{name: "spec changed, controller hasn't seen it",
			obj: workload("DaemonSet", 2, map[string]interface{}{}, map[string]interface{}{
				"observedGeneration": int64(1), "desiredNumberScheduled": int64(2), "updatedNumberScheduled": int64(2),
				"numberReady": int64(2), "numberAvailable": int64(2)})},
		{name: "rolling update: all ready, one node still on the old pod",
			obj: workload("DaemonSet", 2, map[string]interface{}{}, map[string]interface{}{
				"observedGeneration": int64(2), "desiredNumberScheduled": int64(2), "updatedNumberScheduled": int64(1),
				"numberReady": int64(2), "numberAvailable": int64(2)})},
		{name: "ready but not yet available (minReadySeconds)",
			obj: workload("DaemonSet", 1, map[string]interface{}{}, map[string]interface{}{
				"observedGeneration": int64(1), "desiredNumberScheduled": int64(2), "updatedNumberScheduled": int64(2),
				"numberReady": int64(2), "numberAvailable": int64(1)})},
		{name: "no matching nodes",
			obj: workload("DaemonSet", 1, map[string]interface{}{}, map[string]interface{}{
				"observedGeneration": int64(1), "desiredNumberScheduled": int64(0), "numberReady": int64(0)}),
			wantReady: true},
	})
}

func TestJobReadiness(t *testing.T) {
	job := func(status map[string]interface{}) *unstructured.Unstructured {
		obj := workload("Job", 1, map[string]interface{}{}, status)
		obj.SetAPIVersion("batch/v1")
		return obj
	}
	condition := func(conditionType string) []interface{} {
		return []interface{}{map[string]interface{}{"type": conditionType, "status": "True"}}
	}
	runReadinessCases(t, []readinessCase{
		{name: "succeeded", obj: job(map[string]interface{}{"succeeded": int64(1)}), wantReady: true},
		{name: "complete", obj: job(map[string]interface{}{"conditions": condition("Complete")}), wantReady: true},
		{name: "running", obj: job(map[string]interface{}{"active": int64(1)})},
		{name: "failed", obj: job(map[string]interface{}{"failed": int64(4), "conditions": condition("Failed")}), wantFailed: true},
	})
}

// A terminal failure must end the wait at once rather than being retried
// until the timeout
func TestWaitStopsOnTerminalFailure(t *testing.T) {
	interval := readinessPollInterval
	readinessPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { readinessPollInterval = interval })

	failed := workload("Job", 1, map[string]interface{}{}, map[string]interface{}{
		"conditions": []interface{}{map[string]interface{}{"type": "Failed", "status": "True"}}})
	failed.SetAPIVersion("batch/v1")
	jobGVR := schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{jobGVR: "JobList"}, failed)
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}, meta.RESTScopeNamespace)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := waitForResourceReady(ctx, dyn, fake.NewSimpleClientset(), mapper, failed, false)
	if !errors.Is(err, errResourceFailed) {
		t.Errorf("want the job failure, got %v", err)
	}
	if ctx.Err() != nil {
		t.Error("the wait ran until the timeout instead of stopping on the failure")
	}
}
