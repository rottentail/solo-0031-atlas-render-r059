package model

import "fmt"

type Error struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *Error) Error() string { return e.Message }
func Invalid(format string, args ...any) error {
	return &Error{Status: 400, Code: "invalid", Message: fmt.Sprintf(format, args...)}
}
func Conflict(message string) error { return &Error{Status: 409, Code: "conflict", Message: message} }
func Missing(message string) error  { return &Error{Status: 404, Code: "missing", Message: message} }
func Gone(message string) error     { return &Error{Status: 410, Code: "gone", Message: message} }
func Busy(message string) error     { return &Error{Status: 429, Code: "capacity", Message: message} }
func ConflictDetails(message string, details map[string]any) error {
	return &Error{Status: 409, Code: "conflict", Message: message, Details: details}
}
func GoneDetails(message string, details map[string]any) error {
	return &Error{Status: 410, Code: "gone", Message: message, Details: details}
}
