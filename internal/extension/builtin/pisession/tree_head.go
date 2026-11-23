package pisession

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math/bits"
	"unicode/utf8"
)

type projectedTreeHead struct {
	kind, id, parent, role []byte
}

func projectTreeHead(line []byte) (projectedTreeHead, bool) {
	var head projectedTreeHead
	cursor := treeCursor{data: line}
	seenMessage := false
	ok := cursor.object(1, func(key []byte) bool {
		switch string(key) {
		case "type":
			return cursor.headString(&head.kind)
		case "id":
			return cursor.headString(&head.id)
		case "parentId":
			return cursor.headString(&head.parent)
		case "summary":
			return cursor.stringOrNull()
		case "message":
			if seenMessage {
				return false
			}
			seenMessage = true
			return cursor.message(&head.role)
		default:
			return !headKeyAlias(key, "type", "id", "parentId", "summary", "message") && cursor.value(1)
		}
	})
	cursor.space()
	return head, ok && cursor.pos == len(line)
}

type treeCursor struct {
	data []byte
	pos  int
}

func (cursor *treeCursor) message(role *[]byte) bool {
	if cursor.peek() == 'n' {
		return cursor.literal("null")
	}
	return cursor.object(2, func(key []byte) bool {
		switch string(key) {
		case "role":
			return cursor.headString(role)
		case "toolName", "model":
			return cursor.stringOrNull()
		default:
			return !headKeyAlias(key, "role", "toolName", "model") && cursor.value(2)
		}
	})
}

func headKeyAlias(key []byte, names ...string) bool {
	if bytes.IndexByte(key, '\\') >= 0 {
		return true
	}
	for _, name := range names {
		if bytes.EqualFold(key, []byte(name)) {
			return true
		}
	}
	return false
}

func (cursor *treeCursor) headString(field *[]byte) bool {
	if cursor.peek() == 'n' {
		return cursor.literal("null")
	}
	raw, ok := cursor.quoted()
	if !ok || bytes.IndexByte(raw, '\\') >= 0 || !utf8.Valid(raw) {
		return false
	}
	*field = raw
	return true
}

func (cursor *treeCursor) stringOrNull() bool {
	if cursor.peek() == 'n' {
		return cursor.literal("null")
	}
	_, ok := cursor.quoted()
	return ok
}

func (cursor *treeCursor) peek() byte {
	if cursor.pos == len(cursor.data) {
		return 0
	}
	return cursor.data[cursor.pos]
}

func (cursor *treeCursor) take(want byte) bool {
	if cursor.peek() != want {
		return false
	}
	cursor.pos++
	return true
}

func (cursor *treeCursor) space() {
	for {
		switch cursor.peek() {
		case ' ', '\t', '\n', '\r':
			cursor.pos++
		default:
			return
		}
	}
}

func (cursor *treeCursor) literal(text string) bool {
	if !bytes.HasPrefix(cursor.data[cursor.pos:], []byte(text)) {
		return false
	}
	cursor.pos += len(text)
	return true
}

func (cursor *treeCursor) object(depth int, visit func([]byte) bool) bool {
	if !cursor.take('{') {
		return false
	}
	cursor.space()
	if cursor.take('}') {
		return true
	}
	for {
		key, ok := cursor.quoted()
		if !ok {
			return false
		}
		cursor.space()
		if !cursor.take(':') {
			return false
		}
		cursor.space()
		if visit == nil {
			ok = cursor.value(depth)
		} else {
			ok = visit(key)
		}
		if !ok {
			return false
		}
		cursor.space()
		if cursor.take('}') {
			return true
		}
		if !cursor.take(',') {
			return false
		}
		cursor.space()
	}
}

func (cursor *treeCursor) array(depth int) bool {
	cursor.pos++
	cursor.space()
	if cursor.take(']') {
		return true
	}
	for cursor.value(depth) {
		cursor.space()
		if cursor.take(']') {
			return true
		}
		if !cursor.take(',') {
			return false
		}
		cursor.space()
	}
	return false
}

func (cursor *treeCursor) value(depth int) bool {
	switch cursor.peek() {
	case '"':
		_, ok := cursor.quoted()
		return ok
	case '{':
		return depth < 64 && cursor.object(depth+1, nil)
	case '[':
		return depth < 64 && cursor.array(depth+1)
	case 't':
		return cursor.literal("true")
	case 'f':
		return cursor.literal("false")
	case 'n':
		return cursor.literal("null")
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		start := cursor.pos
		for {
			ch := cursor.peek()
			if (ch < '0' || ch > '9') && ch != '-' && ch != '+' && ch != '.' && ch != 'e' && ch != 'E' {
				return json.Valid(cursor.data[start:cursor.pos])
			}
			cursor.pos++
		}
	default:
		return false
	}
}

func (cursor *treeCursor) quoted() ([]byte, bool) {
	if !cursor.take('"') {
		return nil, false
	}
	start := cursor.pos
	end := treeStringEnd(cursor.data, start)
	if end < 0 {
		return nil, false
	}
	cursor.pos = end + 1
	return cursor.data[start:end], true
}

func treeStringEnd(data []byte, pos int) int {
	nextBlock := pos
	const highBits = 0x8080808080808080
	for pos < len(data) {
		if pos >= nextBlock && len(data)-pos >= 8 {
			word := binary.LittleEndian.Uint64(data[pos:])
			quotes := word ^ 0x2222222222222222
			slashes := word ^ 0x5c5c5c5c5c5c5c5c
			controlBytes := (word - 0x2020202020202020) &^ word
			quoteBytes := (quotes - 0x0101010101010101) &^ quotes
			slashBytes := (slashes - 0x0101010101010101) &^ slashes
			special := (controlBytes | quoteBytes | slashBytes) & highBits
			nextBlock = pos + 8
			if special == 0 {
				pos = nextBlock
				continue
			}
			pos += bits.TrailingZeros64(special) / 8
		}
		ch := data[pos]
		switch {
		case ch == '"':
			return pos
		case ch == '\\':
			pos++
			if pos == len(data) {
				return -1
			}
			switch data[pos] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				if len(data)-pos < 5 {
					return -1
				}
				for _, digit := range data[pos+1 : pos+5] {
					lower := digit | 0x20
					if (digit < '0' || digit > '9') && (lower < 'a' || lower > 'f') {
						return -1
					}
				}
				pos += 4
			default:
				return -1
			}
		case ch < ' ':
			return -1
		}
		pos++
	}
	return -1
}
