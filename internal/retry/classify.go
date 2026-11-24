package retry

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/engine"
)

const (
	MaxFindings     = 10
	MaxNodes        = 10
	MaxEvidence     = 5
	maxDetailBytes  = 200
	maxSummaryRunes = 300
)

type Signal string

const (
	SignalUnschedulable     Signal = "unschedulable"
	SignalImagePull         Signal = "image_pull"
	SignalNodeLost          Signal = "node_lost"
	SignalEvicted           Signal = "evicted"
	SignalContainerCreating Signal = "container_creating"
	SignalNotReady          Signal = "not_ready"
	SignalOOMKilled         Signal = "oom_killed"
	SignalCrashLoop         Signal = "crash_loop"
	SignalBadImage          Signal = "bad_image"
)

func (s Signal) crash() bool {
	return s == SignalCrashLoop || s == SignalOOMKilled
}

type Finding struct {
	Signal   Signal `json:"signal"`
	Node     string `json:"node,omitempty"`
	Pods     int    `json:"pods"`
	Reason   string `json:"reason,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type Diagnosis struct {
	Verdict       core.FailureClass `json:"verdict"`
	Summary       string            `json:"summary"`
	Attempt       int               `json:"attempt"`
	Digest        core.Digest       `json:"digest,omitempty"`
	Findings      []Finding         `json:"findings,omitempty"`
	Nodes         []string          `json:"nodes,omitempty"`
	Evidence      []string          `json:"evidence,omitempty"`
	PodsInspected bool              `json:"pods_inspected"`
}

type Input struct {
	Failure       engine.Failure
	Pods          []core.PodStatus
	PodsInspected bool
	Earlier       []Diagnosis
	Recent        []Diagnosis
}

func Classify(in Input) Diagnosis {
	f := in.Failure
	d := Diagnosis{Attempt: max(f.Attempt, 1), Digest: f.Deployment.Digest, PodsInspected: in.PodsInspected}
	if !f.Retryable() {
		d.Verdict = orUnknown(f.Class)
		d.Summary = summarize(d.Verdict, cut(f.Reason))
		return d
	}
	sync := f.Sync
	switch {
	case configurationReason(*sync):
		d.Verdict = core.FailureConfiguration
		d.Summary = summarize(d.Verdict, string(sync.Reason)+": "+cut(sync.Message))
		return d
	case sync.Reason == core.SyncReasonFailed:
		d.Verdict = core.FailureVersion
		d.Summary = summarize(d.Verdict, "Argo CD sync failed: "+cut(sync.Message))
		return d
	}

	current, previous := splitPods(in.Pods, f.Deployment)
	findings := podFindings(current)
	if len(findings) == 0 {
		findings = textFindings(*sync)
	}
	d.Findings = capFindings(findings)
	d.Nodes = nodesOf(findings)
	if len(findings) == 0 {
		d.Verdict = orUnknown(sync.Class)
		if sync.Reason == core.SyncReasonDegraded {
			d.Verdict = core.FailureUnknown
		}
		d.Summary = summarize(d.Verdict, string(sync.Reason)+": "+cut(sync.Message))
		return d
	}

	verdict, evidence := judge(findings, previous, in)
	d.Verdict = verdict
	d.Evidence = evidence
	detail := describe(findings)
	if len(evidence) > 0 {
		detail += "; " + evidence[0]
	}
	d.Summary = summarize(verdict, detail)
	return d
}

func configurationReason(f core.SyncFailure) bool {
	if slices.Contains([]core.SyncFailureReason{core.SyncReasonNotFound, core.SyncReasonForbidden, core.SyncReasonUntracked}, f.Reason) {
		return true
	}
	return f.Reason == core.SyncReasonOutOfSync && f.Class == core.FailureConfiguration
}

func judge(findings []Finding, previous []core.PodStatus, in Input) (core.FailureClass, []string) {
	if slices.ContainsFunc(findings, func(f Finding) bool { return f.Signal == SignalBadImage }) {
		return core.FailureVersion, []string{"the image cannot be started"}
	}
	if !slices.ContainsFunc(findings, func(f Finding) bool { return f.Signal.crash() }) {
		return core.FailureTransient, nil
	}
	return crashEvidence(findings, previous, in)
}

func crashEvidence(findings []Finding, previous []core.PodStatus, in Input) (core.FailureClass, []string) {
	crashNodes := crashNodesOf(findings)
	recentNodes := map[string]bool{}
	for _, r := range in.Recent {
		for _, node := range r.Nodes {
			recentNodes[node] = true
		}
	}
	for _, node := range crashNodes {
		if slices.ContainsFunc(previous, func(p core.PodStatus) bool { return p.Node == node && !p.Healthy() }) {
			return core.FailureTransient, []string{"the previous version is also unhealthy on node " + node + " (hardware, not the release)"}
		}
		if recentNodes[node] {
			return core.FailureTransient, []string{"other versions failed on node " + node + " in the last 7 days (hardware, not the release)"}
		}
	}
	if key, nodes, ok := repeatedCrash(findings, in.Earlier); ok {
		return core.FailureVersion, []string{fmt.Sprintf("the same %s on %d nodes: %s", key, len(nodes), strings.Join(limit(nodes, 3), ", "))}
	}
	for _, node := range crashNodes {
		if slices.ContainsFunc(previous, func(p core.PodStatus) bool { return p.Node == node && p.Healthy() }) {
			return core.FailureVersion, []string{"the previous version runs fine on node " + node}
		}
	}
	return core.FailureUnknown, []string{"no evidence yet whether the node or the release is at fault"}
}

func repeatedCrash(findings []Finding, earlier []Diagnosis) (string, []string, bool) {
	nodes := map[string][]string{}
	add := func(f Finding) {
		if !f.Signal.crash() || f.Node == "" {
			return
		}
		key := crashKey(f)
		if !slices.Contains(nodes[key], f.Node) {
			nodes[key] = append(nodes[key], f.Node)
		}
	}
	for _, f := range findings {
		add(f)
	}
	for _, e := range earlier {
		for _, f := range e.Findings {
			add(f)
		}
	}
	keys := make([]string, 0, len(nodes))
	for key := range nodes {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if len(nodes[key]) >= 2 {
			found := slices.Clone(nodes[key])
			slices.Sort(found)
			return key, found, true
		}
	}
	return "", nil, false
}

func crashKey(f Finding) string {
	key := string(f.Signal)
	if f.Reason != "" {
		key += " (" + f.Reason
		if f.ExitCode != 0 {
			key += ", exit code " + strconv.Itoa(f.ExitCode)
		}
		key += ")"
	} else if f.ExitCode != 0 {
		key += " (exit code " + strconv.Itoa(f.ExitCode) + ")"
	}
	return key
}

func crashNodesOf(findings []Finding) []string {
	var nodes []string
	for _, f := range findings {
		if f.Signal.crash() && f.Node != "" && !slices.Contains(nodes, f.Node) {
			nodes = append(nodes, f.Node)
		}
	}
	slices.Sort(nodes)
	return nodes
}

func splitPods(pods []core.PodStatus, d core.Deployment) ([]core.PodStatus, []core.PodStatus) {
	var current, previous, unmatched []core.PodStatus
	for _, p := range pods {
		switch {
		case p.Runs(d.Digest):
			current = append(current, p)
		case d.PreviousDigest != d.Digest && p.Runs(d.PreviousDigest):
			previous = append(previous, p)
		default:
			unmatched = append(unmatched, p)
		}
	}
	if len(current) == 0 {
		current = unmatched
	}
	return current, previous
}

func podFindings(pods []core.PodStatus) []Finding {
	var findings []Finding
	for _, p := range pods {
		f, ok := podSignal(p)
		if !ok {
			continue
		}
		f.Node = p.Node
		f.Detail = cut(f.Detail)
		i := slices.IndexFunc(findings, func(other Finding) bool {
			return other.Signal == f.Signal && other.Node == f.Node && other.Reason == f.Reason && other.ExitCode == f.ExitCode
		})
		if i >= 0 {
			findings[i].Pods++
			continue
		}
		f.Pods = 1
		findings = append(findings, f)
	}
	slices.SortStableFunc(findings, func(a, b Finding) int {
		return cmp.Or(cmp.Compare(rank(a.Signal), rank(b.Signal)), cmp.Compare(a.Node, b.Node))
	})
	return findings
}

func rank(s Signal) int {
	order := []Signal{SignalBadImage, SignalOOMKilled, SignalCrashLoop, SignalImagePull, SignalUnschedulable, SignalNodeLost, SignalEvicted, SignalContainerCreating, SignalNotReady}
	if i := slices.Index(order, s); i >= 0 {
		return i
	}
	return len(order)
}

func podSignal(p core.PodStatus) (Finding, bool) {
	switch {
	case p.Reason == "Evicted":
		return Finding{Signal: SignalEvicted, Reason: p.Reason, Detail: p.Message}, true
	case p.Reason == "NodeLost" || (p.Terminating && p.Phase == "Unknown"):
		return Finding{Signal: SignalNodeLost, Reason: "NodeLost", Detail: p.Message}, true
	case p.Terminating || p.Healthy() || p.Phase == "Succeeded":
		return Finding{}, false
	case !p.Scheduled && p.Node == "":
		return Finding{Signal: SignalUnschedulable, Reason: "Unschedulable", Detail: p.Message}, true
	}
	var best Finding
	found := false
	for _, c := range p.Containers {
		f, ok := containerSignal(c)
		if ok && (!found || rank(f.Signal) < rank(best.Signal)) {
			best, found = f, true
		}
	}
	if found {
		return best, true
	}
	if p.Phase == "Running" && !slices.ContainsFunc(p.Containers, func(c core.ContainerStatus) bool { return c.Restarts > 0 }) {
		return Finding{Signal: SignalNotReady, Reason: "NotReady", Detail: p.Message}, true
	}
	return Finding{}, false
}

func containerSignal(c core.ContainerStatus) (Finding, bool) {
	switch c.Waiting {
	case "InvalidImageName", "RunContainerError":
		return Finding{Signal: SignalBadImage, Reason: c.Waiting, Detail: c.Message}, true
	case "ErrImagePull", "ImagePullBackOff":
		return Finding{Signal: SignalImagePull, Reason: c.Waiting, Detail: c.Message}, true
	case "ContainerCreating", "PodInitializing":
		return Finding{Signal: SignalContainerCreating, Reason: c.Waiting, Detail: c.Message}, true
	}
	if c.Terminated == "OOMKilled" {
		return Finding{Signal: SignalOOMKilled, Reason: c.Terminated, ExitCode: c.ExitCode, Detail: c.Message}, true
	}
	if c.Waiting == "CrashLoopBackOff" {
		return Finding{Signal: SignalCrashLoop, Reason: c.Terminated, ExitCode: c.ExitCode, Detail: c.Message}, true
	}
	return Finding{}, false
}

func textFindings(f core.SyncFailure) []Finding {
	texts := []string{f.Message}
	for _, r := range f.Resources {
		texts = append(texts, r.Message)
	}
	keywords := []struct {
		signal Signal
		words  []string
	}{
		{SignalBadImage, []string{"invalidimagename", "runcontainererror"}},
		{SignalOOMKilled, []string{"oomkilled"}},
		{SignalCrashLoop, []string{"crashloopbackoff"}},
		{SignalImagePull, []string{"imagepullbackoff", "errimagepull"}},
		{SignalUnschedulable, []string{"unschedulable", "insufficient cpu", "insufficient memory"}},
		{SignalNodeLost, []string{"nodelost", "node lost"}},
		{SignalEvicted, []string{"evicted"}},
		{SignalContainerCreating, []string{"containercreating"}},
		{SignalNotReady, []string{"readiness probe failed", "progress deadline"}},
	}
	var findings []Finding
	for _, k := range keywords {
		for _, text := range texts {
			lower := strings.ToLower(text)
			if slices.ContainsFunc(k.words, func(w string) bool { return strings.Contains(lower, w) }) {
				findings = append(findings, Finding{Signal: k.signal, Detail: cut(text)})
				break
			}
		}
	}
	return findings
}

func capFindings(findings []Finding) []Finding {
	if len(findings) == 0 {
		return nil
	}
	return slices.Clone(limit(findings, MaxFindings))
}

func nodesOf(findings []Finding) []string {
	var nodes []string
	for _, f := range findings {
		if f.Node != "" && !slices.Contains(nodes, f.Node) {
			nodes = append(nodes, f.Node)
		}
	}
	slices.Sort(nodes)
	return limit(nodes, MaxNodes)
}

func describe(findings []Finding) string {
	var signals []Signal
	nodes := map[Signal][]string{}
	exitCodes := map[Signal]int{}
	for _, f := range findings {
		if !slices.Contains(signals, f.Signal) {
			signals = append(signals, f.Signal)
			exitCodes[f.Signal] = f.ExitCode
		}
		if f.Node != "" && !slices.Contains(nodes[f.Signal], f.Node) {
			nodes[f.Signal] = append(nodes[f.Signal], f.Node)
		}
	}
	parts := make([]string, 0, 2)
	for _, s := range limit(signals, 2) {
		text := signalText(s, exitCodes[s])
		switch n := nodes[s]; len(n) {
		case 0:
		case 1:
			text += " on node " + n[0]
		default:
			slices.Sort(n)
			text += " on nodes " + strings.Join(limit(n, 3), ", ")
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "; ")
}

func signalText(s Signal, exitCode int) string {
	switch s {
	case SignalUnschedulable:
		return "pods cannot be scheduled"
	case SignalImagePull:
		return "image pull failing"
	case SignalNodeLost:
		return "node lost"
	case SignalEvicted:
		return "pods evicted"
	case SignalContainerCreating:
		return "containers stuck creating"
	case SignalNotReady:
		return "pods running but not ready"
	case SignalOOMKilled:
		return "out of memory (OOMKilled)"
	case SignalCrashLoop:
		if exitCode != 0 {
			return "crash loop (exit code " + strconv.Itoa(exitCode) + ")"
		}
		return "crash loop"
	case SignalBadImage:
		return "image cannot be started"
	}
	return string(s)
}

func summarize(verdict core.FailureClass, detail string) string {
	text := string(verdict) + ": " + detail
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= maxSummaryRunes {
		return text
	}
	return string([]rune(text)[:maxSummaryRunes-1]) + "…"
}

func orUnknown(c core.FailureClass) core.FailureClass {
	if c == "" {
		return core.FailureUnknown
	}
	return c
}

func cut(s string) string {
	s = strings.ToValidUTF8(strings.Join(strings.Fields(s), " "), "�")
	if len(s) <= maxDetailBytes {
		return s
	}
	end := maxDetailBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

func limit[T any](items []T, n int) []T {
	if len(items) <= n {
		return items
	}
	return items[:n]
}
