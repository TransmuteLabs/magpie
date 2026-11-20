package extension

import (
	"context"
	"io"
	"time"
)

// SessionConverter implements session.convert for a linked module. The host
// supplies the decoded source and publishes the output. The wire transport
// continues to carry documents in SessionRequest and SessionResult.
type SessionConverter struct {
	Descriptor SessionDescriptor
	Convert    func(context.Context, SessionConversionInput) (SessionConversionOutput, error)
}

// SessionConversionInput contains the selected source and target settings.
// Source is borrowed for the duration of Convert.
type SessionConversionInput struct {
	Source         io.Reader
	Cwd            string
	Now            time.Time
	TargetVersion  string
	TargetProvider string
}

// SessionConversionOutput describes one new document. Path is relative to the
// target store declared by Descriptor. Write runs before publication and must
// return write and context errors to the host.
type SessionConversionOutput struct {
	ID    string
	Path  string
	Write func(context.Context, io.Writer) error
}
