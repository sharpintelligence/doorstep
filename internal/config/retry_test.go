package config

import (
	"testing"
	"time"
)

func TestAppRetryPolicy(t *testing.T) {
	t.Parallel()

	defaults := RetryPolicy{Enabled: true, MaxAttempts: 3, Backoff: time.Minute, MaxBackoff: 10 * time.Minute}
	tests := []struct {
		name  string
		retry *Retry
		want  RetryPolicy
	}{
		{name: "omitted", want: defaults},
		{name: "empty block", retry: &Retry{}, want: defaults},
		{name: "disabled", retry: &Retry{Enabled: new(false)}, want: RetryPolicy{MaxAttempts: 3, Backoff: time.Minute, MaxBackoff: 10 * time.Minute}},
		{
			name:  "everything set",
			retry: &Retry{Enabled: new(true), MaxAttempts: intPtr(5), Backoff: durPtr(30 * time.Second), MaxBackoff: durPtr(2 * time.Minute)},
			want:  RetryPolicy{Enabled: true, MaxAttempts: 5, Backoff: 30 * time.Second, MaxBackoff: 2 * time.Minute},
		},
		{
			name:  "long backoff raises the default maximum",
			retry: &Retry{Backoff: durPtr(20 * time.Minute)},
			want:  RetryPolicy{Enabled: true, MaxAttempts: 3, Backoff: 20 * time.Minute, MaxBackoff: 20 * time.Minute},
		},
		{
			name:  "explicit maximum is kept for validation",
			retry: &Retry{Backoff: durPtr(20 * time.Minute), MaxBackoff: durPtr(time.Minute)},
			want:  RetryPolicy{Enabled: true, MaxAttempts: 3, Backoff: 20 * time.Minute, MaxBackoff: time.Minute},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := (App{Retry: tt.retry}).RetryPolicy(); got != tt.want {
				t.Errorf("RetryPolicy() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
