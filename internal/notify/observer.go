package notify

type Result string

const (
	ResultSent    Result = "sent"
	ResultFailed  Result = "failed"
	ResultDropped Result = "dropped"
)

type Observer interface {
	Delivered(sinkType string, result Result)
}

type nopObserver struct{}

func (nopObserver) Delivered(string, Result) {}
