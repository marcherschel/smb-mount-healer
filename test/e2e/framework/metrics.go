//go:build e2e

package framework

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

// Labels is a label subset used to select series.
type Labels map[string]string

// Scrape is one parsed /metrics response, keyed by metric family name.
type Scrape map[string]*dto.MetricFamily

// MetricsPort is the containerPort name/number of http-metrics (chart templates).
const MetricsPort = "8080"

// Metrics scrapes the healer component (metrics.ComponentControlPlane or
// metrics.ComponentWorker; the worker on the agent node) through the
// API-server pod proxy (/api/v1/namespaces/<ns>/pods/<pod>:8080/proxy/metrics)
// and parses the text exposition with expfmt. It retries until the pod is
// ready and the scrape parses.
func (f *F) Metrics(component string) Scrape {
	f.T.Helper()
	var s Scrape
	f.G.Eventually(func() error {
		var err error
		s, err = f.scrape(component)
		return err
	}).Should(gomega.Succeed())
	return s
}

func (f *F) scrape(component string) (Scrape, error) {
	node := ""
	switch component {
	case metrics.ComponentWorker:
		node = f.AgentNode
	case metrics.ComponentControlPlane:
	default:
		return nil, fmt.Errorf("unknown component %q", component)
	}
	pod, err := f.readyPod(HealerNamespace, ComponentLabel+"="+component, node)
	if err != nil {
		return nil, err
	}
	raw, err := f.Kube.CoreV1().Pods(HealerNamespace).
		ProxyGet("http", pod.Name, MetricsPort, "/metrics", nil).DoRaw(f.Ctx)
	if err != nil {
		return nil, fmt.Errorf("scrape %s/%s: %w", HealerNamespace, pod.Name, err)
	}
	return ParseMetrics(raw)
}

// ParseMetrics parses Prometheus text exposition.
func ParseMetrics(raw []byte) (Scrape, error) {
	p := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := p.TextToMetricFamilies(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse metrics: %w", err)
	}
	return Scrape(fams), nil
}

// Value sums every series of family name whose labels include want. Counters,
// gauges and untyped metrics contribute their value; histograms and
// summaries their sample count. A missing family or no matching series is 0,
// which is what a never-incremented counter vector looks like.
func (s Scrape) Value(name string, want Labels) float64 {
	fam, ok := s[name]
	if !ok {
		return 0
	}
	var sum float64
	for _, m := range fam.GetMetric() {
		if !matches(m, want) {
			continue
		}
		switch {
		case m.Counter != nil:
			sum += m.GetCounter().GetValue()
		case m.Gauge != nil:
			sum += m.GetGauge().GetValue()
		case m.Untyped != nil:
			sum += m.GetUntyped().GetValue()
		case m.Histogram != nil:
			sum += float64(m.GetHistogram().GetSampleCount())
		case m.Summary != nil:
			sum += float64(m.GetSummary().GetSampleCount())
		}
	}
	return sum
}

// Has reports whether family name has at least one series matching want.
func (s Scrape) Has(name string, want Labels) bool {
	fam, ok := s[name]
	if !ok {
		return false
	}
	for _, m := range fam.GetMetric() {
		if matches(m, want) {
			return true
		}
	}
	return false
}

// Series renders the series of family name, for failure messages.
func (s Scrape) Series(name string) string {
	fam, ok := s[name]
	if !ok {
		return name + ": <absent>"
	}
	var lines []string
	for _, m := range fam.GetMetric() {
		var ls []string
		for _, l := range m.GetLabel() {
			ls = append(ls, l.GetName()+"="+fmt.Sprintf("%q", l.GetValue()))
		}
		lines = append(lines, fmt.Sprintf("%s{%s} %v", name, strings.Join(ls, ","), Scrape{name: &dto.MetricFamily{Metric: []*dto.Metric{m}}}.Value(name, nil)))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func matches(m *dto.Metric, want Labels) bool {
	if len(want) == 0 {
		return true
	}
	have := make(map[string]string, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		have[l.GetName()] = l.GetValue()
	}
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// MetricDelta is the difference between two scrapes of the same component.
type MetricDelta struct {
	Before, After Scrape
}

// Delta pairs two scrapes; query it with Value/Get.
func Delta(before, after Scrape) MetricDelta { return MetricDelta{Before: before, After: after} }

// Value is after.Value(name, want) - before.Value(name, want).
func (d MetricDelta) Value(name string, want Labels) float64 {
	return d.After.Value(name, want) - d.Before.Value(name, want)
}

// Get is Value with labels as alternating key/value pairs:
// d.Get(metrics.WorkloadActionsTotal, "action", "scale_down", "mode", "live").
func (d MetricDelta) Get(name string, kv ...string) float64 {
	return d.Value(name, LabelsOf(kv...))
}

// LabelsOf builds Labels from alternating key/value pairs.
func LabelsOf(kv ...string) Labels {
	if len(kv)%2 != 0 {
		panic("LabelsOf: odd number of arguments")
	}
	l := make(Labels, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		l[kv[i]] = kv[i+1]
	}
	return l
}
