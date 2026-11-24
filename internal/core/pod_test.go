package core

import "testing"

func TestPodStatusRuns(t *testing.T) {
	t.Parallel()

	const digest = Digest("sha256:2222222222222222222222222222222222222222222222222222222222222222")
	tests := []struct {
		name   string
		images []string
		digest Digest
		want   bool
	}{
		{name: "pinned by digest", images: []string{"ghcr.io/example-user/blog@" + string(digest)}, digest: digest, want: true},
		{name: "tag and digest", images: []string{"ghcr.io/example-user/blog:v1@" + string(digest)}, digest: digest, want: true},
		{name: "runtime image id", images: []string{"busybox:1", "docker-pullable://ghcr.io/example-user/blog@" + string(digest)}, digest: digest, want: true},
		{name: "other digest", images: []string{"ghcr.io/example-user/blog@sha256:1111111111111111111111111111111111111111111111111111111111111111"}, digest: digest},
		{name: "tag only", images: []string{"ghcr.io/example-user/blog:2222"}, digest: digest},
		{name: "no images", digest: digest},
		{name: "no digest", images: []string{"ghcr.io/example-user/blog@"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := (PodStatus{Images: tt.images}).Runs(tt.digest); got != tt.want {
				t.Errorf("Runs(%s) = %v, want %v", tt.digest, got, tt.want)
			}
		})
	}
}

func TestPodStatusHealthy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pod  PodStatus
		want bool
	}{
		{name: "ready", pod: PodStatus{Ready: true}, want: true},
		{name: "not ready", pod: PodStatus{}},
		{name: "ready but terminating", pod: PodStatus{Ready: true, Terminating: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.pod.Healthy(); got != tt.want {
				t.Errorf("Healthy() = %v, want %v", got, tt.want)
			}
		})
	}
}
