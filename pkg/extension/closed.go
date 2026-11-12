package extension

const maxClosedFrames = 1024

type closedFrame struct {
	id     string
	seq    uint64
	parent string
}

type recentFrames struct {
	byID  map[string]*closedFrame
	order [maxClosedFrames]*closedFrame
	next  int
}

func (recent *recentFrames) remember(id string, seq uint64, parent string) {
	if recent.byID == nil {
		recent.byID = make(map[string]*closedFrame)
	}
	if previous := recent.order[recent.next]; previous != nil && recent.byID[previous.id] == previous {
		delete(recent.byID, previous.id)
	}
	frame := &closedFrame{id: id, seq: seq, parent: parent}
	recent.byID[id] = frame
	recent.order[recent.next] = frame
	recent.next = (recent.next + 1) % len(recent.order)
}

func (recent *recentFrames) consume(id string, seq uint64, parent string) bool {
	frame := recent.byID[id]
	if frame == nil || frame.seq != seq || frame.parent != parent {
		return false
	}
	delete(recent.byID, id)
	return true
}
