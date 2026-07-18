package sourceextsdk

import (
	"errors"
	"fmt"
	"time"

	pb "github.com/Diviance/Source-SDK/gen/sourceext/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Error struct {
	GRPCCode       codes.Code
	Code           string
	Message        string
	SourceID       string
	Retryable      bool
	RetryAfter     time.Duration
	UpstreamStatus int
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) GRPCStatus() *status.Status {
	code := e.GRPCCode
	if code == codes.OK {
		code = codes.Unknown
	}
	st := status.New(code, e.Message)
	withDetails, err := st.WithDetails(&pb.ErrorDetail{
		SourceId:       e.SourceID,
		Retryable:      e.Retryable,
		RetryAfterMs:   e.RetryAfter.Milliseconds(),
		UpstreamStatus: int32(e.UpstreamStatus),
		Code:           e.Code,
	})
	if err == nil {
		return withDetails
	}
	return st
}

func ParseError(err error) *Error {
	if err == nil {
		return nil
	}
	var sourceErr *Error
	if errors.As(err, &sourceErr) {
		return sourceErr
	}
	st, ok := status.FromError(err)
	if !ok {
		return &Error{GRPCCode: codes.Unknown, Message: err.Error()}
	}
	parsed := &Error{GRPCCode: st.Code(), Message: st.Message()}
	for _, detail := range st.Details() {
		if d, ok := detail.(*pb.ErrorDetail); ok {
			parsed.Code = d.Code
			parsed.SourceID = d.SourceId
			parsed.Retryable = d.Retryable
			parsed.RetryAfter = time.Duration(d.RetryAfterMs) * time.Millisecond
			parsed.UpstreamStatus = int(d.UpstreamStatus)
			break
		}
	}
	return parsed
}

func Unimplemented(operation string) error {
	return &Error{
		GRPCCode: codes.Unimplemented,
		Code:     "unsupported_operation",
		Message:  operation + " is not supported",
	}
}
