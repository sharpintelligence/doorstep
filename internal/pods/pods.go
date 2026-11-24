package pods

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	MaxNamespaces   = 5
	MaxPods         = 200
	ListLimit       = 500
	MaxMessageBytes = 200

	conditionTrue = "True"
)

type Getter interface {
	Get(ctx context.Context, path string, query url.Values, out any) error
}

type Reader struct {
	client Getter
}

func New(client Getter) (*Reader, error) {
	if client == nil {
		return nil, errors.New("pods: a kubernetes client is required")
	}
	return &Reader{client: client}, nil
}

func (r *Reader) Pods(ctx context.Context, workloads []core.Workload) ([]core.PodStatus, error) {
	var out []core.PodStatus
	for _, namespace := range namespaces(workloads) {
		list, err := r.list(ctx, namespace)
		if err != nil {
			return nil, err
		}
		for _, p := range owned(workloads, list) {
			if len(out) == MaxPods {
				return out, nil
			}
			out = append(out, p)
		}
	}
	return out, nil
}

func (r *Reader) list(ctx context.Context, namespace string) ([]pod, error) {
	if !validSegment(namespace) {
		return nil, fmt.Errorf("pods: invalid namespace %q", namespace)
	}
	var list podList
	query := url.Values{"limit": {strconv.Itoa(ListLimit)}}
	if err := r.client.Get(ctx, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/pods", query, &list); err != nil {
		return nil, fmt.Errorf("list pods in %s: %w", namespace, err)
	}
	return list.Items, nil
}

func namespaces(workloads []core.Workload) []string {
	var out []string
	for _, w := range workloads {
		if w.Namespace != "" && !slices.Contains(out, w.Namespace) {
			out = append(out, w.Namespace)
		}
	}
	slices.Sort(out)
	if len(out) > MaxNamespaces {
		out = out[:MaxNamespaces]
	}
	return out
}

func validSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.Contains(s, "/")
}

func owned(workloads []core.Workload, pods []pod) []core.PodStatus {
	var out []core.PodStatus
	for _, p := range pods {
		owner, ok := p.controller()
		if !ok {
			continue
		}
		for _, w := range workloads {
			if w.Namespace == p.Metadata.Namespace && owns(w, owner, p.Metadata.Labels) {
				out = append(out, p.status(w))
				break
			}
		}
	}
	return out
}

func owns(w core.Workload, owner ownerReference, labels map[string]string) bool {
	switch w.Kind {
	case "StatefulSet", "DaemonSet":
		return owner.Kind == w.Kind && owner.Name == w.Name
	case "Deployment":
		hash := labels["pod-template-hash"]
		return owner.Kind == "ReplicaSet" && hash != "" && owner.Name == w.Name+"-"+hash
	case "Rollout":
		hash := labels["rollouts-pod-template-hash"]
		return owner.Kind == "ReplicaSet" && hash != "" && owner.Name == w.Name+"-"+hash
	default:
		return false
	}
}

type podList struct {
	Items []pod `json:"items"`
}

type pod struct {
	Metadata podMeta   `json:"metadata"`
	Spec     podSpec   `json:"spec"`
	Status   podStatus `json:"status"`
}

type podMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Labels            map[string]string `json:"labels"`
	OwnerReferences   []ownerReference  `json:"ownerReferences"`
	DeletionTimestamp string            `json:"deletionTimestamp"`
}

type ownerReference struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Controller *bool  `json:"controller"`
}

type podSpec struct {
	NodeName       string      `json:"nodeName"`
	Containers     []container `json:"containers"`
	InitContainers []container `json:"initContainers"`
}

type container struct {
	Image string `json:"image"`
}

type podStatus struct {
	Phase                 string            `json:"phase"`
	Reason                string            `json:"reason"`
	Message               string            `json:"message"`
	Conditions            []podCondition    `json:"conditions"`
	InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
	ContainerStatuses     []containerStatus `json:"containerStatuses"`
}

type podCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type containerStatus struct {
	Name         string         `json:"name"`
	Ready        bool           `json:"ready"`
	RestartCount int            `json:"restartCount"`
	Image        string         `json:"image"`
	ImageID      string         `json:"imageID"`
	State        containerState `json:"state"`
	LastState    containerState `json:"lastState"`
}

type containerState struct {
	Waiting    *stateDetail `json:"waiting"`
	Terminated *stateDetail `json:"terminated"`
}

type stateDetail struct {
	Reason   string `json:"reason"`
	Message  string `json:"message"`
	ExitCode int    `json:"exitCode"`
}

func (p pod) controller() (ownerReference, bool) {
	for _, o := range p.Metadata.OwnerReferences {
		if o.Controller != nil && *o.Controller {
			return o, true
		}
	}
	return ownerReference{}, false
}

func (p pod) status(w core.Workload) core.PodStatus {
	out := core.PodStatus{
		Namespace:   p.Metadata.Namespace,
		Name:        p.Metadata.Name,
		Node:        p.Spec.NodeName,
		Workload:    w,
		Phase:       p.Status.Phase,
		Reason:      p.Status.Reason,
		Message:     cut(p.Status.Message),
		Terminating: p.Metadata.DeletionTimestamp != "",
	}
	for _, c := range p.Status.Conditions {
		switch c.Type {
		case "Ready":
			out.Ready = c.Status == conditionTrue
		case "PodScheduled":
			out.Scheduled = c.Status == conditionTrue
			if !out.Scheduled && out.Message == "" {
				out.Message = cut(c.Message)
			}
		}
	}
	addImage := func(image string) {
		if image != "" && !slices.Contains(out.Images, image) {
			out.Images = append(out.Images, image)
		}
	}
	for _, c := range slices.Concat(p.Spec.InitContainers, p.Spec.Containers) {
		addImage(c.Image)
	}
	for _, c := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
		addImage(c.Image)
		addImage(c.ImageID)
		out.Containers = append(out.Containers, c.status())
	}
	return out
}

func (c containerStatus) status() core.ContainerStatus {
	out := core.ContainerStatus{Name: c.Name, Ready: c.Ready, Restarts: c.RestartCount}
	if w := c.State.Waiting; w != nil {
		out.Waiting = w.Reason
		out.Message = cut(w.Message)
	}
	terminated := c.State.Terminated
	if terminated == nil {
		terminated = c.LastState.Terminated
	}
	if terminated != nil {
		out.Terminated = terminated.Reason
		out.ExitCode = terminated.ExitCode
		if out.Message == "" {
			out.Message = cut(terminated.Message)
		}
	}
	return out
}

func cut(s string) string {
	s = strings.ToValidUTF8(strings.Join(strings.Fields(s), " "), "�")
	if len(s) <= MaxMessageBytes {
		return s
	}
	end := MaxMessageBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}
