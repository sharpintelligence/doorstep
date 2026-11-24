package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	shortSHALength    = 7
	shortDigestLength = 12
	timeLayout        = "2006-01-02 15:04 UTC"
)

type kindSet map[core.NotificationKind]bool

func newKindSet(kinds []core.NotificationKind) (kindSet, error) {
	if kinds == nil {
		kinds = core.DefaultNotificationKinds()
	}
	if len(kinds) == 0 {
		return nil, errors.New("at least one notification kind is required")
	}
	set := make(kindSet, len(kinds))
	for _, k := range kinds {
		if !k.Valid() {
			return nil, fmt.Errorf("%w: %q", core.ErrUnknownNotificationKind, k)
		}
		set[k] = true
	}
	return set, nil
}

func shortSHA(sha string) string {
	if len(sha) <= shortSHALength {
		return sha
	}
	return sha[:shortSHALength]
}

func shortDigest(d core.Digest) string {
	algorithm, hex, ok := strings.Cut(string(d), ":")
	if !ok || len(hex) <= shortDigestLength {
		return string(d)
	}
	return algorithm + ":" + hex[:shortDigestLength]
}

func truncateText(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit-1]) + "…"
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return "under 1s"
	}
	return d.Round(time.Second).String()
}

func upperFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func deploymentDuration(d *core.Deployment) (time.Duration, bool) {
	if d.StartedAt == nil || d.FinishedAt == nil || d.FinishedAt.Before(*d.StartedAt) {
		return 0, false
	}
	return d.FinishedAt.Sub(*d.StartedAt), true
}

func describeTarget(t core.Target) string {
	if t.Environment == "" || t.Environment == t.App {
		return t.App
	}
	return t.App + " (" + t.Environment + ")"
}

func deploymentNoun(d *core.Deployment) string {
	noun := "deploy"
	if d.Kind == core.KindRollback {
		noun = "rollback"
	}
	if d.ID != 0 {
		noun += " " + deploymentRef(d.ID)
	}
	return noun
}

func deploymentRef(id core.DeploymentID) string {
	return "#" + strconv.FormatInt(int64(id), 10)
}

func isRollbackFailure(n core.Notification) bool {
	return n.Kind == core.NotificationDeployFailed && n.Deployment != nil && n.Deployment.Kind == core.KindRollback
}

func summary(n core.Notification) string {
	return describeTarget(n.Target) + ": " + outcome(n)
}

func outcome(n core.Notification) string {
	switch n.Kind {
	case core.NotificationDeployQueued:
		return deploymentNoun(n.Deployment) + " queued"
	case core.NotificationDeployStarted:
		return deploymentNoun(n.Deployment) + " started"
	case core.NotificationDeploySucceeded:
		return deploymentNoun(n.Deployment) + " succeeded"
	case core.NotificationDeployFailed:
		if isRollbackFailure(n) {
			return deploymentNoun(n.Deployment) + " failed, manual action needed"
		}
		return deploymentNoun(n.Deployment) + " failed"
	case core.NotificationDeployRolledBack:
		return rolledBackNoun(n) + " rolled back"
	case core.NotificationDeployCancelled:
		return deploymentNoun(n.Deployment) + " cancelled"
	case core.NotificationDeploySuperseded:
		text := deploymentNoun(n.Deployment) + " superseded"
		if n.Deployment.SupersededByID != nil {
			text += " by " + deploymentRef(*n.Deployment.SupersededByID)
		}
		return text
	case core.NotificationLockChanged:
		return lockChange(n)
	case core.NotificationLockReminder:
		return "still " + lockVerb(n.Lock) + " by " + lockOwner(n.Lock)
	}
	return string(n.Kind)
}

func rolledBackNoun(n core.Notification) string {
	if n.RollbackOf != nil {
		return deploymentNoun(n.RollbackOf)
	}
	if n.Deployment.RollbackOfID != nil {
		return "deploy " + deploymentRef(*n.Deployment.RollbackOfID)
	}
	return "the previous deploy"
}

func rollbackOfID(n core.Notification) (core.DeploymentID, bool) {
	switch {
	case n.RollbackOf != nil && n.RollbackOf.ID != 0:
		return n.RollbackOf.ID, true
	case n.Deployment != nil && n.Deployment.RollbackOfID != nil:
		return *n.Deployment.RollbackOfID, true
	default:
		return 0, false
	}
}

func lockChange(n core.Notification) string {
	verb := lockVerb(n.Lock)
	if n.LockChange == core.LockReleased {
		verb = "un" + verb
	}
	by := n.Actor
	if by == "" && n.LockChange == core.LockAcquired {
		by = n.Lock.Owner
	}
	if by == "" {
		return verb
	}
	return verb + " by " + by
}

func lockVerb(l *core.Lock) string {
	if l.Kind == core.LockFreeze {
		return "frozen"
	}
	return "locked"
}

func lockOwner(l *core.Lock) string {
	if l.Owner == "" {
		return "unknown"
	}
	return l.Owner
}

func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
