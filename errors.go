package monty

import (
	"errors"
	"fmt"
	"strings"
)

// ErrClosed is returned when a Session or Runtime is used after Close, or
// after the sandbox worker died (see WorkerError).
var ErrClosed = errors.New("monty: closed")

// ErrNotHandled may be returned by an OSHandler to decline a call. The sandbox
// then raises the call's own default: PermissionError naming the path for
// filesystem calls, RuntimeError for the rest.
var ErrNotHandled = errors.New("monty: os call not handled")

// Exception is a Python exception.
//
// It is returned as the error from Session.Run when sandbox code raises, in
// which case Traceback and Frames are set. It is also the Go representation of
// an exception *value* (an exception stored in a variable, with no traceback),
// and host functions may return an *Exception as their error to raise that
// exact Python exception inside the sandbox.
type Exception struct {
	// Type is the Python exception type name, e.g. "ValueError".
	Type string
	// Message is the exception message; may be empty.
	Message string
	// Traceback is the rendered CPython-style traceback, ending with the
	// exception line. Empty for exception values.
	Traceback string
	// Frames lists the traceback frames, outermost first.
	Frames []Frame
	// Data carries structured detail for exception types that have more
	// than a message: *UnicodeErrorData or *JSONErrorData. nil otherwise.
	Data any
}

// UnicodeErrorData is the payload of UnicodeDecodeError / UnicodeEncodeError.
type UnicodeErrorData struct {
	// Encoding is the codec name as CPython reports it, e.g. "utf-8".
	Encoding string
	// Object is the input that failed: []byte for decode errors, string for
	// encode errors.
	Object any
	// Start and End bound the failing range; End is exclusive.
	Start, End uint64
	Reason     string
}

// JSONErrorData is the payload of json.JSONDecodeError.
type JSONErrorData struct {
	// Msg is the bare message without the ": line N column M" suffix.
	Msg string
	// Doc is the document being parsed; nil when it was too large to send.
	Doc *string
	// Pos is the character index of the error; Lineno and Colno are 1-based.
	Pos, Lineno, Colno uint64
}

// Raise builds an *Exception with the given Python type and message, for use
// as the error returned from a host function or OSHandler.
func Raise(pythonType, format string, args ...any) *Exception {
	return &Exception{Type: pythonType, Message: fmt.Sprintf(format, args...)}
}

func (e *Exception) Error() string {
	if e.Traceback != "" {
		return e.Traceback
	}
	if e.Message == "" {
		return e.Type
	}
	return e.Type + ": " + e.Message
}

// Frame is one Python traceback frame.
type Frame struct {
	Filename  string
	Line      int
	Column    int
	EndLine   int
	EndColumn int
	// Name is the function name; empty for module-level code.
	Name string
	// Preview is the source line shown in the traceback.
	Preview string
}

// TypingError is returned by Session.Run when the session type-checks
// snippets and the checker rejected the code. The code was not executed.
type TypingError struct {
	// Diagnostics is rendered in the session's TypeCheckFormat.
	Diagnostics string
}

func (e *TypingError) Error() string {
	return "monty: type check failed:\n" + strings.TrimRight(e.Diagnostics, "\n")
}

// WorkerError reports that the sandbox worker died: it hit its hard memory
// ceiling, overflowed the wasm stack, panicked, exceeded the context deadline,
// or broke the protocol. The Session is unusable afterwards; create a new one.
type WorkerError struct {
	// Message describes the failure.
	Message string
	// Stderr holds diagnostics the worker wrote before dying, if any.
	Stderr string
	// Err is the underlying cause, e.g. context.DeadlineExceeded.
	Err error
}

func (e *WorkerError) Error() string {
	msg := "monty: worker died: " + e.Message
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += "\n" + s
	}
	return msg
}

func (e *WorkerError) Unwrap() error { return e.Err }

// renderTraceback formats frames and the exception line the way CPython does.
func renderTraceback(frames []Frame, excType, message string) string {
	var b strings.Builder
	if len(frames) > 0 {
		b.WriteString("Traceback (most recent call last):\n")
		for _, f := range frames {
			fmt.Fprintf(&b, "  File %q, line %d", f.Filename, f.Line)
			if f.Name != "" {
				b.WriteString(", in " + f.Name)
			}
			b.WriteByte('\n')
			if f.Preview != "" {
				b.WriteString("    " + strings.TrimSpace(f.Preview) + "\n")
			}
		}
	}
	b.WriteString(excType)
	if message != "" {
		b.WriteString(": " + message)
	}
	return b.String()
}
