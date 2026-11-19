// Package dispatch creates one worker Job per investigation from the
// chart's Job template. Kubernetes handles retries, deadlines and cleanup.
package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"k8s.io/apimachinery/pkg/types"
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
	EnvEvent       = "HERAION_EVENT" // read by `heraion run`; no longer set by the gateway
	EnvEventFile   = "HERAION_EVENT_FILE"
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
		if e.Name != EnvPlaybook && e.Name != EnvEvent && e.Name != EnvEventFile {
			env = append(env, e)
		}
	}
	c.Env = env
	return job
}

// Dispatch creates the Job and, for a non-empty event, the Secret holding
// it. created is false if this event was already dispatched.
func (d *Dispatcher) Dispatch(ctx context.Context, playbook, hook string, event []byte) (name string, created bool, err error) {
	job := d.Build(playbook, hook, event)
	jobs := d.Client.BatchV1().Jobs(d.Namespace)
	// A redelivery is a duplicate even when at capacity, so check first.
	if _, err := jobs.Get(ctx, job.Name, metav1.GetOptions{}); err == nil {
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
	ownSecret := false
	if len(event) > 0 {
		if ownSecret, err = d.createEventSecret(ctx, job, event); err != nil {
			return "", false, err
		}
	}
	newJob, err := jobs.Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// Another replica won the race; its Job uses the same Secret name
		// and content, so keep the Secret.
		return job.Name, false, nil
	}
	if err != nil {
		if ownSecret {
			_ = d.secrets().Delete(context.WithoutCancel(ctx), EventSecretName(job.Name), metav1.DeleteOptions{})
		}
		return "", false, fmt.Errorf("create job: %w", err)
	}
	if len(event) > 0 {
		// The Secret is deleted with the Job (ttlSecondsAfterFinished).
		if err := d.ownSecret(ctx, newJob); err != nil && d.Log != nil {
			d.Log.Warn("event secret not owned by its job; it will not be garbage-collected",
				"secret", EventSecretName(job.Name), "err", err)
		}
	}
	return job.Name, true, nil
}

func (d *Dispatcher) secrets() corev1client.SecretInterface {
	return d.Client.CoreV1().Secrets(d.Namespace)
}

// createEventSecret reports whether it created the Secret; an existing one
// (same name, so same payload) is reused.
func (d *Dispatcher) createEventSecret(ctx context.Context, job *batchv1.Job, event []byte) (bool, error) {
	labels := map[string]string{}
	for k, v := range job.Labels {
		labels[k] = v
	}
	immutable := true
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: EventSecretName(job.Name), Namespace: d.Namespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Immutable:  &immutable,
		Data:       map[string][]byte{EventKey: event},
	}
	_, err := d.secrets().Create(ctx, sec, metav1.CreateOptions{})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsAlreadyExists(err):
		return false, nil
	default:
		return false, fmt.Errorf("create event secret: %w", err)
	}
}

func (d *Dispatcher) ownSecret(ctx context.Context, job *batchv1.Job) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"ownerReferences": []metav1.OwnerReference{{
		APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID,
	}}}})
	if err != nil {
		return err
	}
	_, err = d.secrets().Patch(ctx, EventSecretName(job.Name), types.MergePatchType, patch, metav1.PatchOptions{})
	return err
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
