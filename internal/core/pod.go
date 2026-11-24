package core

import "strings"

type Workload struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

type PodStatus struct {
	Namespace   string
	Name        string
	Node        string
	Workload    Workload
	Images      []string
	Phase       string
	Reason      string
	Message     string
	Ready       bool
	Scheduled   bool
	Terminating bool
	Containers  []ContainerStatus
}

type ContainerStatus struct {
	Name       string
	Ready      bool
	Restarts   int
	Waiting    string
	Terminated string
	ExitCode   int
	Message    string
}

func (p PodStatus) Runs(digest Digest) bool {
	if digest == "" {
		return false
	}
	suffix := "@" + string(digest)
	for _, image := range p.Images {
		if strings.HasSuffix(image, suffix) {
			return true
		}
	}
	return false
}

func (p PodStatus) Healthy() bool {
	return p.Ready && !p.Terminating
}
