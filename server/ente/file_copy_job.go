package ente

type CopyJobStatus string

const (
	CopyJobPending   CopyJobStatus = "pending"
	CopyJobRunning   CopyJobStatus = "running"
	CopyJobCompleted CopyJobStatus = "completed"
	CopyJobFailed    CopyJobStatus = "failed"
)

type CopyJobResponse struct {
	JobID int64 `json:"jobID"`
}

type CopyJobError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

type CopyJobStatusResponse struct {
	JobID  int64         `json:"jobID"`
	Status CopyJobStatus `json:"status"`
	// On failure, the files copied before it: a batch isn't atomic.
	OldToNewFileIDMap map[int64]int64 `json:"oldToNewFileIDMap,omitempty"`
	Error             *CopyJobError   `json:"error,omitempty"`
}
