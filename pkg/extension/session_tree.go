package extension

// SessionTreeCodec reads the records of a linked, branch-based session format.
// The host owns file access, ancestry selection and the consumer's limits.
type SessionTreeCodec struct {
	Inspect func(line []byte, spokenOnly bool) (SessionTreeNode, bool)
	Decode  func(line []byte, spokenOnly bool) (SessionTreeRecord, error)
}

// SessionTreeNode is the lightweight projection used to select a branch.
type SessionTreeNode struct {
	ID, ParentID string
	HasContent   bool
}

// SessionTreeRecord owns the decoded record independently of the input buffer.
// Parts is non-nil on successful Decode and decodes the content only when
// the host visits the record's branch.
type SessionTreeRecord struct {
	ID, ParentID string
	Parts        func() []SessionSourcePart
}
