package metrics

import "github.com/prometheus/client_golang/prometheus"

// SetEnum sets an enum gauge: for each value in values, the series with
// base + {enumLabel: value} is set to 1 if value == current, else 0.
func SetEnum(g *prometheus.GaugeVec, base prometheus.Labels, enumLabel string, values []string, current string) {
	for _, v := range values {
		l := make(prometheus.Labels, len(base)+1)
		for k, bv := range base {
			l[k] = bv
		}
		l[enumLabel] = v
		val := 0.0
		if v == current {
			val = 1
		}
		g.With(l).Set(val)
	}
}

// DeleteEnum removes every series of an enum gauge matching base.
func DeleteEnum(g *prometheus.GaugeVec, base prometheus.Labels) {
	g.DeletePartialMatch(base)
}
