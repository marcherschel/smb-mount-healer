package sim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
)

// Event is one line of a golden trace. Fields are emitted in this order.
type Event struct {
	T      string `json:"t"`
	Kind   string `json:"kind"`
	Obj    string `json:"obj,omitempty"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Reason string `json:"reason,omitempty"`
	Msg    string `json:"msg,omitempty"`
	Src    string `json:"src,omitempty"`
}

// errorDedupWindow suppresses repeats of the same reconcile error.
const errorDedupWindow = time.Minute

// Trace collects the JSONL golden trace of one run.
type Trace struct {
	w       *World
	enabled bool
	buf     bytes.Buffer
	lines   int
	once    map[string]bool
	errSeen map[string]time.Time
}

func newTrace(w *World) *Trace {
	return &Trace{w: w, once: map[string]bool{}, errSeen: map[string]time.Time{}}
}

func (t *Trace) add(e Event) {
	if !t.enabled {
		return
	}
	e.T = fmtElapsed(t.w.Elapsed())
	enc := json.NewEncoder(&t.buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		panic(err)
	}
	t.lines++
}

// addOnce adds e only the first time its content is seen (when dedupe).
func (t *Trace) addOnce(e Event, dedupe bool) {
	if dedupe {
		k := e.Kind + "\x00" + e.Obj + "\x00" + e.Msg + "\x00" + e.Reason
		if t.once[k] {
			return
		}
		t.once[k] = true
	}
	t.add(e)
}

// errorf traces a reconcile/loop error, at most once per minute per text.
func (t *Trace) errorf(src, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	t.w.checkSecret("error", msg)
	k := src + "\x00" + msg
	if last, ok := t.errSeen[k]; ok && t.w.Now().Sub(last) < errorDedupWindow {
		return
	}
	t.errSeen[k] = t.w.Now()
	t.add(Event{Kind: "error", Src: src, Msg: t.abbrevAll(msg)})
}

// Bytes returns the trace.
func (t *Trace) Bytes() []byte { return t.buf.Bytes() }

var (
	podPathRE = regexp.MustCompile(regexp.QuoteMeta(KubeletRoot+"/pods/") + `([^/\s"]+)/volumes/kubernetes\.io~csi/([^/\s"]+)/mount`)
	gmPathRE  = regexp.MustCompile(regexp.QuoteMeta(KubeletRoot+"/plugins/kubernetes.io/csi/"+DriverName+"/") + `([0-9a-f]{64})/globalmount`)
)

// abbrev shortens a healer path: gm(<pv>), pod(<ns/pod or uid>,<pv>), q/<folder>.
func (t *Trace) abbrev(p string) string { return t.abbrevAll(p) }

// abbrevAll shortens every healer path inside s.
func (t *Trace) abbrevAll(s string) string {
	s = gmPathRE.ReplaceAllStringFunc(s, func(m string) string {
		hash := gmPathRE.FindStringSubmatch(m)[1]
		for _, name := range t.w.pvOrder {
			if kubelet.GlobalmountHash(t.w.pvs[name].Handle) == hash {
				return "gm(" + name + ")"
			}
		}
		return "gm(unknown:" + hash[:8] + ")"
	})
	s = podPathRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := podPathRE.FindStringSubmatch(m)
		who := sub[1]
		if n, ok := t.w.podNames[types.UID(sub[1])]; ok {
			who = n
		}
		return "pod(" + who + "," + sub[2] + ")"
	})
	return strings.ReplaceAll(s, QuarantineDir+"/", "q/")
}
