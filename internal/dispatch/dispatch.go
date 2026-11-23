// Package dispatch creates one worker Job per investigation from the
// chart's Job template. Kubernetes handles retries, deadlines and cleanup.
package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/yaml"
)

// Labels and env set on every dispatched Job.
const (
	LabelPlaybook  = "heraion.io/playbook"
	LabelHook      = "heraion.io/hook"
	LabelTrigger   = "heraion.io/trigger"
	ContainerName  = "worker"
	EnvEventFile   = "HERAION_EVENT_FILE"
	EnvEventID     = "HERAION_EVENT_ID" // EventID of the event, for create_issue de-duplication
	EnvPlaybook    = "HERAION_PLAYBOOK"
	maxJobNameBase = 52 // + "-" + 10 hash chars = 63
)

// The webhook body is passed in a Secret mounted at EventDir, not in an env
// var: Linux limits a single env string to 128 KiB (MAX_ARG_STRLEN), and a
// Secret keeps the untrusted payload out of the Job spec.
const (
	EventDir    = "/var/run/heraion/event"
	EventKey    = "payload"
	eventVolume = "heraion-event"
)

// EventSecretName is the Secret holding the payload of Job job, e.g.
// "heraion-alertmanager-triage-3f9a1c2b7d-event".
func EventSecretName(job string) string { return job + "-event" }

// ErrAtCapacity means MaxActive worker Jobs are already running.
var ErrAtCapacity = errors.New("maximum number of concurrent investigations reached")

// ErrRetryLater means the run for this event failed and its Job is being
// deleted: a retry of the same event starts a new run.
var ErrRetryLater = errors.New("the previous run for this event failed; retry")

// Dispatcher creates Jobs. The Job name is a hash of the event identity, so
// a repeated delivery finds the Job instead of starting a second run, even
// when several gateway replicas receive it. A failed run is never a
// duplicate: its Job is deleted, so the sender's retry runs again.
type Dispatcher struct {
	Client    kubernetes.Interface
	Namespace string
	Template  *batchv1.Job
	// MaxActive caps unfinished Jobs (0 = no limit). Replicas can race past
	// it; use a ResourceQuota for a hard cap.
	MaxActive int
	Log       *slog.Logger // optional
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

// JobName is the same for the same event identity, e.g.
// "heraion-alertmanager-triage-<10 hex chars>".
func JobName(playbook, hook string, identity []byte) string {
	h := sha256.New()
	h.Write([]byte(hook + "\x00" + playbook + "\x00"))
	h.Write(identity)
	base := "heraion-" + playbook
	if len(base) > maxJobNameBase {
		base = strings.TrimRight(base[:maxJobNameBase], "-")
	}
	return base + "-" + hex.EncodeToString(h.Sum(nil))[:10]
}

// EventID names the event across playbooks and runs, e.g. "1b2c3d4e5f607182".
func EventID(hook string, identity []byte) string {
	h := sha256.New()
	h.Write([]byte(hook + "\x00"))
	h.Write(identity)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// build returns the Job for one investigation without creating it.
func (d *Dispatcher) build(playbook, hook string, identity, event []byte) *batchv1.Job {
	job := d.Template.DeepCopy()
	job.Name = JobName(playbook, hook, identity)
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
	if len(identity) > 0 {
		env = append(env, corev1.EnvVar{Name: EnvEventID, Value: EventID(hook, identity)})
	}
	if len(event) > 0 {
		env = append(env, corev1.EnvVar{Name: EnvEventFile, Value: EventDir + "/" + EventKey})
		// 0444: the pod's runAsUser may differ from the file owner, and only
		// this container mounts the volume.
		mode := int32(0o444)
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: eventVolume, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: EventSecretName(job.Name), DefaultMode: &mode},
		}})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: eventVolume, MountPath: EventDir, ReadOnly: true})
	}
	for _, e := range c.Env {
		if e.Name != EnvPlaybook && e.Name != EnvEventFile && e.Name != EnvEventID {
			env = append(env, e)
		}
	}
	c.Env = env
	return job
}

// Dispatch creates the Job and, for a non-empty event, the Secret holding
// it. identity names the event (Job name, HERAION_EVENT_ID); event is the
// payload the worker reads. created is false if a run for this identity is
// running or done. A failed run is deleted and ErrRetryLater returned.
func (d *Dispatcher) Dispatch(ctx context.Context, playbook, hook string, identity, event []byte) (name string, created bool, err error) {
	job := d.build(playbook, hook, identity, event)
	jobs := d.Client.BatchV1().Jobs(d.Namespace)
	// A redelivery is a duplicate even when at capacity, so check first.
	if old, err := jobs.Get(ctx, job.Name, metav1.GetOptions{}); err == nil {
		switch {
		case old.DeletionTimestamp != nil:
			return job.Name, false, ErrRetryLater
		case hasCondition(old, batchv1.JobFailed):
			// Foreground: the Job stays until its pods and event Secret are
			// gone, so a retry never reuses a Secret about to be collected.
			fg := metav1.DeletePropagationForeground
			if err := jobs.Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: &fg}); err != nil && !apierrors.IsNotFound(err) {
				return "", false, fmt.Errorf("delete failed job: %w", err)
			}
			return job.Name, false, ErrRetryLater
		}
		return job.Name, false, nil
	} else if !apierrors.IsNotFound(err) {
		return "", false, fmt.Errorf("get job: %w", err)
	}
	if d.MaxActive > 0 {
		n, err := d.active(ctx)
		if err != nil {
			return "", false, err
		}
		if n >= d.MaxActive {
			return "", false, ErrAtCapacity
		}
	}
	// The Job first, then its Secret with the owner reference set at
	// creation: the gateway needs no patch or delete on Secrets. The pod
	// waits for the Secret volume; the kubelet retries the mount.
	newJob, err := jobs.Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// Another replica won the race and creates the Secret.
		return job.Name, false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("create job: %w", err)
	}
	if len(event) > 0 {
		if err := d.createEventSecret(ctx, newJob, event); err != nil {
			bg := metav1.DeletePropagationBackground
			if derr := jobs.Delete(context.WithoutCancel(ctx), job.Name, metav1.DeleteOptions{PropagationPolicy: &bg}); derr != nil && d.Log != nil {
				d.Log.Warn("job without its event secret could not be deleted", "job", job.Name, "err", derr)
			}
			return "", false, err
		}
	}
	return job.Name, true, nil
}

func (d *Dispatcher) secrets() corev1client.SecretInterface {
	return d.Client.CoreV1().Secrets(d.Namespace)
}

// createEventSecret creates the Secret holding the payload of job, owned
// by it, so it is deleted with the Job. One that exists (another replica,
// same name and identity) is fine.
func (d *Dispatcher) createEventSecret(ctx context.Context, job *batchv1.Job, event []byte) error {
	labels := map[string]string{}
	for k, v := range job.Labels {
		labels[k] = v
	}
	immutable := true
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: EventSecretName(job.Name), Namespace: d.Namespace, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID}}},
		Type:      corev1.SecretTypeOpaque,
		Immutable: &immutable,
		Data:      map[string][]byte{EventKey: event},
	}
	_, err := d.secrets().Create(ctx, sec, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create event secret: %w", err)
	}
	return nil
}

// listJobs returns the worker Jobs: those with the template's labels.
func (d *Dispatcher) listJobs(ctx context.Context) ([]batchv1.Job, error) {
	sel := labels.Set(d.Template.Labels).AsSelector().String()
	list, err := d.Client.BatchV1().Jobs(d.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	return list.Items, nil
}

func (d *Dispatcher) active(ctx context.Context) (int, error) {
	jobs, err := d.listJobs(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range jobs {
		if !finished(&jobs[i]) {
			n++
		}
	}
	return n, nil
}

// finished reports whether the Job has a Complete or Failed condition.
func finished(j *batchv1.Job) bool {
	return hasCondition(j, batchv1.JobComplete) || hasCondition(j, batchv1.JobFailed)
}

// hasCondition reports whether condition t of the Job is true.
func hasCondition(j *batchv1.Job, t batchv1.JobConditionType) bool {
	for _, c := range j.Status.Conditions {
		if c.Status == corev1.ConditionTrue && c.Type == t {
			return true
		}
	}
	return false
}
