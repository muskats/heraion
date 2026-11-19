package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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
	if env[EnvEvent] != `{"a":1}` || env[EnvPlaybook] != "alertmanager-triage" || env["KEEP"] != "1" || len(ct.Env) != 3 {
		t.Errorf("env %v", ct.Env)
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
