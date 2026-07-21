package provider

import "fmt"

type Error struct {
	ErrorCode    string
	Message      string
	Retryable    bool
	KnownNoWrite bool
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.ErrorCode
}

func (e *Error) Code() string { return e.ErrorCode }

func fail(code, message string, retryable bool) error {
	return &Error{ErrorCode: code, Message: message, Retryable: retryable}
}

func wrap(code, message string, err error, retryable bool) error {
	return &Error{ErrorCode: code, Message: fmt.Sprintf("%s: %v", message, err), Retryable: retryable}
}
