package argocd

import "encoding/json"

type application struct {
	Metadata objectMeta `json:"metadata"`
	Spec     appSpec    `json:"spec"`
	Status   appStatus  `json:"status"`
}

type objectMeta struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	ResourceVersion string `json:"resourceVersion"`
}

type appSpec struct {
	Source     *appSource  `json:"source"`
	Sources    []appSource `json:"sources"`
	SyncPolicy *syncPolicy `json:"syncPolicy"`
}

type appSource struct {
	RepoURL string `json:"repoURL"`
}

type syncPolicy struct {
	Automated *automatedPolicy `json:"automated"`
}

type automatedPolicy struct {
	Enabled *bool `json:"enabled"`
}

type appStatus struct {
	Sync           syncInfo         `json:"sync"`
	Health         healthInfo       `json:"health"`
	OperationState *operationState  `json:"operationState"`
	Conditions     []condition      `json:"conditions"`
	Resources      []resourceStatus `json:"resources"`
	ReconciledAt   string           `json:"reconciledAt"`
}

type syncInfo struct {
	Status    string   `json:"status"`
	Revision  string   `json:"revision"`
	Revisions []string `json:"revisions"`
}

type healthInfo struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type operationState struct {
	Phase      string      `json:"phase"`
	Message    string      `json:"message"`
	Operation  operation   `json:"operation"`
	SyncResult *syncResult `json:"syncResult"`
	FinishedAt string      `json:"finishedAt"`
}

type operation struct {
	Sync *syncOperation `json:"sync"`
}

type syncOperation struct {
	Revision  string   `json:"revision"`
	Revisions []string `json:"revisions"`
}

type syncResult struct {
	Revision  string           `json:"revision"`
	Revisions []string         `json:"revisions"`
	Resources []resourceResult `json:"resources"`
}

type resourceResult struct {
	Group     string `json:"group"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	HookPhase string `json:"hookPhase"`
}

type condition struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type resourceStatus struct {
	Group     string      `json:"group"`
	Kind      string      `json:"kind"`
	Namespace string      `json:"namespace"`
	Name      string      `json:"name"`
	Status    string      `json:"status"`
	Health    *healthInfo `json:"health"`
}

type watchEvent struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

func (a application) multiSource() bool {
	return len(a.Spec.Sources) > 0
}

func (a application) revisions() []string {
	if a.multiSource() {
		return a.Status.Sync.Revisions
	}
	if a.Status.Sync.Revision == "" {
		return nil
	}
	return []string{a.Status.Sync.Revision}
}

func (a application) automatedSync() bool {
	p := a.Spec.SyncPolicy
	return p != nil && p.Automated != nil && (p.Automated.Enabled == nil || *p.Automated.Enabled)
}

func (o operationState) revisions(multiSource bool) []string {
	revision, revisions := "", []string(nil)
	switch {
	case o.SyncResult != nil:
		revision, revisions = o.SyncResult.Revision, o.SyncResult.Revisions
	case o.Operation.Sync != nil:
		revision, revisions = o.Operation.Sync.Revision, o.Operation.Sync.Revisions
	}
	if multiSource {
		return revisions
	}
	if revision == "" {
		return nil
	}
	return []string{revision}
}
