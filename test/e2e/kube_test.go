//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sync"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
)

// kube is a minimal kube-apiserver for Jobs and Secrets. It decodes JSON and
// protobuf (client-go's default for built-in types) and enforces the
// validation the gateway relies on.
type kube struct {
	*httptest.Server
	mu            sync.Mutex
	jobs          map[string]*batchv1.Job
	secrets       map[string]*corev1.Secret
	verbs         map[[2]string]bool // resource, verb
	failJobCreate bool
}

var kubeRoute = regexp.MustCompile(`^/(?:apis/batch/v1|api/v1)/namespaces/[^/]+/(jobs|secrets)(?:/([^/]+))?$`)

func newKube(t *testing.T) *kube {
	k := &kube{jobs: map[string]*batchv1.Job{}, secrets: map[string]*corev1.Secret{}, verbs: map[[2]string]bool{}}
	k.Server = httptest.NewServer(http.HandlerFunc(k.handle))
	t.Cleanup(k.Close)
	return k
}

// kubeconfig writes a kubeconfig for the server and returns its path.
func (k *kube) kubeconfig(t *testing.T) string {
	return writeFile(t, filepath.Join(t.TempDir(), "kubeconfig"), `apiVersion: v1
kind: Config
clusters: [{name: mock, cluster: {server: "`+k.URL+`"}}]
users: [{name: gateway, user: {}}]
contexts: [{name: mock, context: {cluster: mock, user: gateway}}]
current-context: mock
`)
}

func (k *kube) finishAll() {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, j := range k.jobs {
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	}
}

func (k *kube) setFailJobCreate(v bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.failJobCreate = v
}

func (k *kube) job(name string) *batchv1.Job {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.jobs[name].DeepCopy()
}

func (k *kube) secret(name string) *corev1.Secret {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.secrets[name].DeepCopy()
}

func (k *kube) counts() (jobs, secrets int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.jobs), len(k.secrets)
}

func send(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func sendErr(w http.ResponseWriter, err *apierrors.StatusError) {
	st := err.ErrStatus
	st.Kind, st.APIVersion = "Status", "v1"
	send(w, int(st.Code), st)
}

func uid() types.UID {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return types.UID(hex.EncodeToString(b))
}

func (k *kube) handle(w http.ResponseWriter, r *http.Request) {
	m := kubeRoute.FindStringSubmatch(r.URL.Path)
	if m == nil {
		sendErr(w, apierrors.NewNotFound(schema.GroupResource{}, r.URL.Path))
		return
	}
	res, name := m[1], m[2]
	verb := map[string]string{"GET": "get", "POST": "create", "PATCH": "patch", "DELETE": "delete"}[r.Method]
	if verb == "get" && name == "" {
		verb = "list"
	}
	body, _ := io.ReadAll(r.Body)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.verbs[[2]string{res, verb}] = true
	jobsGR, secretsGR := schema.GroupResource{Group: "batch", Resource: "jobs"}, schema.GroupResource{Resource: "secrets"}

	switch res + " " + verb {
	case "jobs get":
		if j, ok := k.jobs[name]; ok {
			send(w, http.StatusOK, j)
		} else {
			sendErr(w, apierrors.NewNotFound(jobsGR, name))
		}
	case "jobs list":
		sel, err := labels.Parse(r.URL.Query().Get("labelSelector"))
		if err != nil {
			sendErr(w, apierrors.NewBadRequest(err.Error()))
			return
		}
		list := &batchv1.JobList{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "JobList"}}
		for _, j := range k.jobs {
			if sel.Matches(labels.Set(j.Labels)) {
				list.Items = append(list.Items, *j)
			}
		}
		send(w, http.StatusOK, list)
	case "jobs create":
		obj, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, nil)
		if err != nil {
			sendErr(w, apierrors.NewBadRequest("decode: "+err.Error()))
			return
		}
		j := obj.(*batchv1.Job)
		if msg := validateJob(j); msg != "" {
			sendErr(w, apierrors.NewInvalid(schema.GroupKind{Group: "batch", Kind: "Job"}, j.Name, nil))
			return
		}
		switch {
		case k.failJobCreate:
			sendErr(w, apierrors.NewForbidden(jobsGR, j.Name, errors.New("exceeded quota: count/jobs.batch")))
		case k.jobs[j.Name] != nil:
			sendErr(w, apierrors.NewAlreadyExists(jobsGR, j.Name))
		default:
			j.UID, j.TypeMeta = uid(), metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"}
			k.jobs[j.Name] = j
			send(w, http.StatusCreated, j)
		}
	case "secrets create":
		obj, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, nil)
		if err != nil {
			sendErr(w, apierrors.NewBadRequest("decode: "+err.Error()))
			return
		}
		s := obj.(*corev1.Secret)
		size := 0
		for _, v := range s.Data {
			size += len(v)
		}
		switch {
		case k.secrets[s.Name] != nil:
			sendErr(w, apierrors.NewAlreadyExists(secretsGR, s.Name))
		case size > 1<<20:
			sendErr(w, apierrors.NewBadRequest("Secret data must be no more than 1048576 bytes"))
		default:
			s.UID, s.TypeMeta = uid(), metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
			k.secrets[s.Name] = s
			send(w, http.StatusCreated, s)
		}
	case "secrets patch":
		s, ok := k.secrets[name]
		if !ok {
			sendErr(w, apierrors.NewNotFound(secretsGR, name))
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/merge-patch+json" {
			sendErr(w, apierrors.NewBadRequest("unsupported patch type "+ct))
			return
		}
		var p struct {
			Metadata struct {
				OwnerReferences []metav1.OwnerReference `json:"ownerReferences"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			sendErr(w, apierrors.NewBadRequest(err.Error()))
			return
		}
		s.OwnerReferences = p.Metadata.OwnerReferences
		send(w, http.StatusOK, s)
	case "secrets delete":
		if _, ok := k.secrets[name]; !ok {
			sendErr(w, apierrors.NewNotFound(secretsGR, name))
			return
		}
		delete(k.secrets, name)
		send(w, http.StatusOK, metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: "Success"})
	default:
		sendErr(w, apierrors.NewMethodNotSupported(schema.GroupResource{Resource: res}, verb))
	}
}

// validateJob applies the apiserver rules that bite Job names: the name is
// copied into a label, so both are limited to 63 characters.
func validateJob(j *batchv1.Job) string {
	if len(j.Name) > 63 {
		return "name longer than 63 characters"
	}
	for _, l := range []map[string]string{j.Labels, j.Spec.Template.Labels} {
		for _, v := range l {
			if len(v) > 63 {
				return "label value longer than 63 characters"
			}
		}
	}
	return ""
}
