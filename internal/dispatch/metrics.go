package dispatch

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// Job statuses reported by Metrics.
const (
	StatusActive    = "active"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

var (
	jobsDesc = prometheus.NewDesc("heraion_jobs",
		"Worker Jobs by playbook and status (active, succeeded, failed). Finished Jobs count until ttlSecondsAfterFinished removes them.",
		[]string{"playbook", "status"}, nil)
	lastSuccessDesc = prometheus.NewDesc("heraion_job_last_success_timestamp_seconds",
		"Completion time of the newest succeeded worker Job per playbook, e.g. to alert when a schedule stops succeeding.",
		[]string{"playbook"}, nil)
)

// Metrics returns a collector that lists the worker Jobs on each scrape:
// webhook Jobs and CronJob Jobs alike, since both carry the template's labels.
//
//	heraion_jobs{playbook="trivy-remediate",status="failed"} 1
//	heraion_job_last_success_timestamp_seconds{playbook="trivy-remediate"} 1.7637e+09
func (d *Dispatcher) Metrics() prometheus.Collector { return jobCollector{d} }

type jobCollector struct{ d *Dispatcher }

func (c jobCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- jobsDesc
	ch <- lastSuccessDesc
}

func (c jobCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	jobs, err := c.d.listJobs(ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(jobsDesc, err)
		return
	}
	counts := map[string]map[string]int{} // playbook -> status -> n
	last := map[string]time.Time{}
	for i := range jobs {
		j := &jobs[i]
		pb := j.Labels[LabelPlaybook]
		if pb == "" {
			continue
		}
		if counts[pb] == nil {
			counts[pb] = map[string]int{StatusActive: 0, StatusSucceeded: 0, StatusFailed: 0}
		}
		st := jobStatus(j)
		counts[pb][st]++
		if st == StatusSucceeded && j.Status.CompletionTime != nil && j.Status.CompletionTime.After(last[pb]) {
			last[pb] = j.Status.CompletionTime.Time
		}
	}
	for pb, byStatus := range counts {
		for st, n := range byStatus {
			ch <- prometheus.MustNewConstMetric(jobsDesc, prometheus.GaugeValue, float64(n), pb, st)
		}
	}
	for pb, t := range last {
		ch <- prometheus.MustNewConstMetric(lastSuccessDesc, prometheus.GaugeValue, float64(t.Unix()), pb)
	}
}

// jobStatus is active until the Job has a Complete or Failed condition.
func jobStatus(j *batchv1.Job) string {
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return StatusSucceeded
		case batchv1.JobFailed:
			return StatusFailed
		}
	}
	return StatusActive
}
