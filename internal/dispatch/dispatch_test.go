package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func newDispatcher(t *testing.T, max int) (*Dispatcher, *fake.Clientset) {
	t.Helper()
	tmpl, err := LoadTemplate("testdata/job.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientset()
	return &Dispatcher{Client: c, Namespace: "heraion", Template: tmpl, MaxActive: max}, c
}

func TestDispatchBuildsJobAndDeduplicates(t *testing.T) {
	d, c := newDispatcher(t, 0)
	ctx := context.Background()
	name, created, err := d.Dispatch(ctx, "alertmanager-triage", "alertmanager", []byte(`{"a":1}`))
	if err != nil || !created {
		t.Fatalf("%v %v", created, err)
	}
	job, err := c.BatchV1().Jobs("heraion").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ct := job.Spec.Template.Spec.Containers[0]
	if strings.Join(ct.Args, " ") != "run --playbook alertmanager-triage" {
		t.Errorf("args %v", ct.Args)
	}
	env := map[string]string{}
	for _, e := range ct.Env {
		env[e.Name] = e.Value
	}
	// The template's stale HERAION_PLAYBOOK is replaced; the payload is a file.
	if env[EnvEventFile] != EventDir+"/payload" || env[EnvPlaybook] != "alertmanager-triage" || env["KEEP"] != "1" || len(ct.Env) != 3 {
		t.Errorf("env %v", ct.Env)
	}
	sec, err := c.CoreV1().Secrets("heraion").Get(ctx, EventSecretName(name), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(sec.Data[EventKey]) != `{"a":1}` {
		t.Errorf("secret data %q", sec.Data)
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Kind != "Job" || sec.OwnerReferences[0].Name != name {
		t.Errorf("secret must be owned by its job: %+v", sec.OwnerReferences)
	}
	vols := job.Spec.Template.Spec.Volumes
	if len(vols) != 1 || vols[0].Secret == nil || vols[0].Secret.SecretName != EventSecretName(name) {
		t.Errorf("volumes %+v", vols)
	}
	if len(ct.VolumeMounts) != 1 || ct.VolumeMounts[0].MountPath != EventDir {
		t.Errorf("mounts %+v", ct.VolumeMounts)
	}
	if job.Labels[LabelPlaybook] != "alertmanager-triage" || job.Labels["app.kubernetes.io/component"] != "worker" {
		t.Errorf("labels %v", job.Labels)
	}
	if d.Template.Spec.Template.Spec.Containers[0].Args != nil {
		t.Error("template was mutated")
	}

	// Same delivery again: no second Job.
	name2, created, err := d.Dispatch(ctx, "alertmanager-triage", "alertmanager", []byte(`{"a":1}`))
	if err != nil || created || name2 != name {
		t.Fatalf("dedup: %s %v %v", name2, created, err)
	}
	// Different payload: new Job.
	if _, created, _ := d.Dispatch(ctx, "alertmanager-triage", "alertmanager", []byte(`{"a":2}`)); !created {
		t.Fatal("different payload must create a new job")
	}
}

func TestCapacity(t *testing.T) {
	d, c := newDispatcher(t, 1)
	ctx := context.Background()
	name, _, err := d.Dispatch(ctx, "p", "h", []byte("1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Dispatch(ctx, "p", "h", []byte("2")); !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("want ErrAtCapacity, got %v", err)
	}
	job, _ := c.BatchV1().Jobs("heraion").Get(ctx, name, metav1.GetOptions{})
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if _, err := c.BatchV1().Jobs("heraion").UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, created, err := d.Dispatch(ctx, "p", "h", []byte("2")); err != nil || !created {
		t.Fatalf("finished jobs must not count: %v", err)
	}
}

func TestLargeEventAndRedeliveryAtCapacity(t *testing.T) {
	d, c := newDispatcher(t, 1)
	ctx := context.Background()
	// Over Linux's 128 KiB limit for one env string: must not be an env var.
	big := []byte(strings.Repeat("x", 200<<10))
	name, created, err := d.Dispatch(ctx, "p", "h", big)
	if err != nil || !created {
		t.Fatalf("%v %v", created, err)
	}
	job, _ := c.BatchV1().Jobs("heraion").Get(ctx, name, metav1.GetOptions{})
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if len(e.Value) > 1<<10 {
			t.Fatalf("env %s carries the payload (%d bytes)", e.Name, len(e.Value))
		}
	}
	// At capacity, a redelivery of the same event is a duplicate, not a 429.
	name2, created, err := d.Dispatch(ctx, "p", "h", big)
	if err != nil || created || name2 != name {
		t.Fatalf("redelivery: %s %v %v", name2, created, err)
	}
}

func TestNoEventNoSecret(t *testing.T) {
	d, c := newDispatcher(t, 0)
	ctx := context.Background()
	name, _, err := d.Dispatch(ctx, "p", "h", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CoreV1().Secrets("heraion").Get(ctx, EventSecretName(name), metav1.GetOptions{}); err == nil {
		t.Fatal("no event must create no secret")
	}
	job, _ := c.BatchV1().Jobs("heraion").Get(ctx, name, metav1.GetOptions{})
	if len(job.Spec.Template.Spec.Volumes) != 0 {
		t.Fatal("no event must mount nothing")
	}
}

func TestFailedJobCreateDeletesSecret(t *testing.T) {
	d, c := newDispatcher(t, 0)
	c.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("quota exceeded")
	})
	ctx := context.Background()
	if _, _, err := d.Dispatch(ctx, "p", "h", []byte("e")); err == nil {
		t.Fatal("expected error")
	}
	list, _ := c.CoreV1().Secrets("heraion").List(ctx, metav1.ListOptions{})
	if len(list.Items) != 0 {
		t.Fatalf("orphaned secrets: %d", len(list.Items))
	}
}

func TestJobName(t *testing.T) {
	n := JobName(strings.Repeat("a", 52), "h", nil)
	if len(n) > 63 {
		t.Fatalf("name too long: %d", len(n))
	}
	if JobName("p", "h", []byte("x")) == JobName("p", "h2", []byte("x")) {
		t.Fatal("hook must be part of the name hash")
	}
}

func TestTemplateNeedsWorkerContainer(t *testing.T) {
	if _, err := LoadTemplate("testdata/missing.yaml"); err == nil {
		t.Fatal("expected error")
	}
}

func TestMetrics(t *testing.T) {
	d, c := newDispatcher(t, 0)
	ctx := context.Background()
	done := metav1.NewTime(time.Unix(1_700_000_000, 0))
	for _, j := range []struct {
		name, pb string
		cond     batchv1.JobConditionType
	}{
		{"a", "trivy", batchv1.JobComplete},
		{"b", "trivy", batchv1.JobFailed},
		{"c", "trivy", ""},
		{"d", "triage", ""},
	} {
		job := d.build(j.pb, "", nil)
		job.Name = j.name
		if j.cond != "" {
			job.Status.Conditions = []batchv1.JobCondition{{Type: j.cond, Status: corev1.ConditionTrue}}
			job.Status.CompletionTime = &done
		}
		if _, err := c.BatchV1().Jobs("heraion").Create(ctx, job, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	want := `
# HELP heraion_job_last_success_timestamp_seconds Completion time of the newest succeeded worker Job per playbook, e.g. to alert when a schedule stops succeeding.
# TYPE heraion_job_last_success_timestamp_seconds gauge
heraion_job_last_success_timestamp_seconds{playbook="trivy"} 1.7e+09
# HELP heraion_jobs Worker Jobs by playbook and status (active, succeeded, failed). Finished Jobs count until ttlSecondsAfterFinished removes them.
# TYPE heraion_jobs gauge
heraion_jobs{playbook="triage",status="active"} 1
heraion_jobs{playbook="triage",status="failed"} 0
heraion_jobs{playbook="triage",status="succeeded"} 0
heraion_jobs{playbook="trivy",status="active"} 1
heraion_jobs{playbook="trivy",status="failed"} 1
heraion_jobs{playbook="trivy",status="succeeded"} 1
`
	if err := testutil.CollectAndCompare(d.Metrics(), strings.NewReader(want)); err != nil {
		t.Error(err)
	}
}
