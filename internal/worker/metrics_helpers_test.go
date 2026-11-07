package worker

import (
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// metricValue reads a counter or gauge.
func metricValue(m prometheus.Metric) float64 {
	var d dto.Metric
	if err := m.Write(&d); err != nil {
		panic(err)
	}
	switch {
	case d.Counter != nil:
		return d.Counter.GetValue()
	case d.Gauge != nil:
		return d.Gauge.GetValue()
	}
	panic("not a counter or gauge")
}

// seriesCount returns the number of series c currently exports.
func seriesCount(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric, 4096)
	c.Collect(ch)
	close(ch)
	return len(ch)
}
