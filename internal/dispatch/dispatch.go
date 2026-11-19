// Package dispatch creates one worker Job per investigation from the
// chart's Job template. Kubernetes handles retries, deadlines and cleanup.
package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// Labels and env set on every dispatched Job.
const (
	LabelPlaybook  = "heraion.io/playbook"
	LabelHook      = "heraion.io/hook"
	LabelTrigger   = "heraion.io/trigger"
	ContainerName  = "worker"
	EnvEvent       = "HERAION_EVENT"
	EnvPlaybook    = "HERAION_PLAYBOOK"
	maxJobNameBase = 52 // + "-" + 10 hash chars = 63
)

// ErrAtCapacity means MaxActive worker Jobs are already running.
var ErrAtCapacity = errors.New("maximum number of concurrent investigations reached")

// Dispatcher creates Jobs. The Job name is a hash of the event, so a repeated
// delivery gets AlreadyExists instead of starting a second run, even when
// several gateway replicas receive it.
type Dispatcher struct {
	Client    kubernetes.Interface
	Namespace string
	Template  *batchv1.Job
	// MaxActive caps unfinished Jobs (0 = no limit). Replicas can race past
	// it; use a ResourceQuota for a hard cap.
	MaxActive int
}

// LoadTemplate reads a batch/v1 Job manifest and checks that it has a
// container named "worker".
func LoadTemplate(path string) (*batchv1.Job, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read job template: %w", err)
	}
	job := &batchv1.Job{}
	if err := yaml.UnmarshalStrict(b, job); err != nil {
		return nil, fmt.Errorf("parse job template %s: %w", path, err)
	}
	if workerIndex(job) < 0 {
		return nil, fmt.Errorf("job template %s has no container named %q", path, ContainerName)
	}
	return job, nil
}

func workerIndex(job *batchv1.Job) int {
	for i, c := range job.Spec.Template.Spec.Containers {
		if c.Name == ContainerName {
			return i
		}
	}
	return -1
}

// JobName is the same for the same input, e.g.
// "heraion-alertmanager-triage-<10 hex chars>".
func JobName(playbook, hook string, event []byte) string {
	h := sha256.New()
	h.Write([]byte(hook + "\x00" + playbook + "\x00"))
	h.Write(event)
	base := "heraion-" + playbook
	if len(base) > maxJobNameBase {
		base = strings.TrimRight(base[:maxJobNameBase], "-")
	}
	return base + "-" + hex.EncodeToString(h.Sum(nil))[:10]
}

// Build returns the Job for one investigation without creating it.
func (d *Dispatcher) Build(playbook, hook string, event []byte) *batchv1.Job {
	job := d.Template.DeepCopy()
	job.Name = JobName(playbook, hook, event)
	job.GenerateName = ""
	job.Namespace = d.Namespace
	job.ResourceVersion = ""
	if job.Labels == nil {
		job.Labels = map[string]string{}
	}
	job.Labels[LabelPlaybook] = playbook
	job.Labels[LabelTrigger] = "webhook"
	if hook != "" {
		job.Labels[LabelHook] = hook
	}
	pod := &job.Spec.Template
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[LabelPlaybook] = playbook

	c := &pod.Spec.Containers[workerIndex(job)]
	c.Args = []string{"run", "--playbook", playbook}
	env := []corev1.EnvVar{{Name: EnvPlaybook, Value: playbook}}
	if len(event) > 0 {
		env = append(env, corev1.EnvVar{Name: EnvEvent, Value: string(event)})
	}
	for _, e := range c.Env {
		if e.Name != EnvPlaybook && e.Name != EnvEvent {
			env = append(env, e)
		}
	}
	c.Env = env
	return job
}

// Dispatch creates the Job. created is false if this event was already
// dispatched.
func (d *Dispatcher) Dispatch(ctx context.Context, playbook, hook string, event []byte) (name string, created bool, err error) {
	job := d.Build(playbook, hook, event)
	if d.MaxActive > 0 {
		n, err := d.active(ctx)
		if err != nil {
			return "", false, err
		}
		if n >= d.MaxActive {
			return "", false, ErrAtCapacity
		}
	}
	_, err = d.Client.BatchV1().Jobs(d.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return job.Name, false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("create job: %w", err)
	}
	return job.Name, true, nil
}

func (d *Dispatcher) active(ctx context.Context) (int, error) {
	sel := labels.Set(d.Template.Labels).AsSelector().String()
	list, err := d.Client.BatchV1().Jobs(d.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return 0, fmt.Errorf("list jobs: %w", err)
	}
	n := 0
	for _, j := range list.Items {
		if !finished(&j) {
			n++
		}
	}
	return n, nil
}

func finished(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
