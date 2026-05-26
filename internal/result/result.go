package result

type Result string

const (
	Succeeded Result = "succeeded"
	Failed    Result = "failed"
	Canceled  Result = "canceled"
	Skipped   Result = "skipped"
)

func (r Result) ExitCode() int {
	switch r {
	case Succeeded:
		return 0
	case Canceled:
		return 130
	default:
		return 1
	}
}
