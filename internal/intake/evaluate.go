package intake

import (
	"cmp"
	"slices"
	"strconv"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
)

const notReported = "not reported"

type Snapshot struct {
	CheckRuns []github.CheckRun
	Statuses  []github.CommitStatus
	Suites    []github.CheckSuite
}

func Evaluate(required []string, s Snapshot) (core.ChecksState, []core.Check) {
	reported := reportedChecks(s)

	var checks []core.Check
	if len(required) > 0 {
		for _, name := range uniqueNames(required) {
			c, ok := reported[name]
			if !ok {
				c = core.Check{Name: name, State: core.ChecksPending, Detail: notReported}
			}
			checks = append(checks, c)
		}
	} else {
		for _, c := range reported {
			checks = append(checks, c)
		}
		checks = append(checks, activeSuites(s.Suites)...)
	}
	slices.SortFunc(checks, func(a, b core.Check) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Kind, b.Kind))
	})

	if len(required) == 0 && len(reported) == 0 {
		return core.ChecksPending, checks
	}
	overall := core.ChecksSuccess
	for _, c := range checks {
		overall = worst(overall, c.State)
	}
	return overall, checks
}

func reportedChecks(s Snapshot) map[string]core.Check {
	latest := map[string]github.CheckRun{}
	for _, run := range s.CheckRuns {
		if current, ok := latest[run.Name]; !ok || run.ID > current.ID {
			latest[run.Name] = run
		}
	}

	reported := map[string]core.Check{}
	add := func(c core.Check) {
		if current, ok := reported[c.Name]; ok && rank(current.State) >= rank(c.State) {
			return
		}
		reported[c.Name] = c
	}
	for _, run := range latest {
		add(checkRunCheck(run))
	}
	for _, st := range s.Statuses {
		if core.IsDoorstepStatusContext(st.Context) {
			continue
		}
		add(statusCheck(st))
	}
	return reported
}

func checkRunCheck(run github.CheckRun) core.Check {
	c := core.Check{Name: run.Name, Kind: core.CheckKindRun}
	switch {
	case run.Status != "completed":
		c.State, c.Detail = core.ChecksPending, run.Status
	case run.Conclusion == "success" || run.Conclusion == "neutral" || run.Conclusion == "skipped":
		c.State, c.Detail = core.ChecksSuccess, run.Conclusion
	default:
		c.State, c.Detail = core.ChecksFailure, cmp.Or(run.Conclusion, "no conclusion")
	}
	return c
}

func statusCheck(st github.CommitStatus) core.Check {
	c := core.Check{Name: st.Context, Kind: core.CheckKindStatus, Detail: st.State}
	switch st.State {
	case "success":
		c.State = core.ChecksSuccess
	case "pending":
		c.State = core.ChecksPending
	default:
		c.State = core.ChecksFailure
	}
	return c
}

func activeSuites(suites []github.CheckSuite) []core.Check {
	var checks []core.Check
	for _, suite := range suites {
		if suite.LatestCheckRunsCount == 0 || suite.Status == "completed" {
			continue
		}
		name := "check suite " + strconv.FormatInt(suite.ID, 10)
		if suite.App.Slug != "" {
			name += " (" + suite.App.Slug + ")"
		}
		checks = append(checks, core.Check{Name: name, Kind: core.CheckKindSuite, State: core.ChecksPending, Detail: suite.Status})
	}
	return checks
}

func uniqueNames(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func worst(a, b core.ChecksState) core.ChecksState {
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func rank(s core.ChecksState) int {
	switch s {
	case core.ChecksSuccess:
		return 0
	case core.ChecksPending:
		return 1
	case core.ChecksFailure:
		return 2
	}
	return 2
}
