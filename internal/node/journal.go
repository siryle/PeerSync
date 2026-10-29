package node

import (
	"time"

	"github.com/netcraft/peersync/internal/syncer"
)

// Event is one line of the sync log written into the status file.
type Event struct {
	Time    time.Time `json:"time"`
	ShareID string    `json:"share_id,omitempty"`
	Level   string    `json:"level"`
	Text    string    `json:"text"`
}

const eventLimit = 200

func (n *Node) addEvent(shareID, level, text string) {
	if text == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if len(n.events) > 0 {
		last := n.events[len(n.events)-1]
		// shortcut: identical line within 3s is dropped, keep both if a retry must show twice
		if last.ShareID == shareID && last.Text == text && now.Sub(last.Time) < 3*time.Second {
			return
		}
	}
	n.events = append(n.events, Event{Time: now, ShareID: shareID, Level: level, Text: text})
	if len(n.events) > eventLimit {
		n.events = append([]Event(nil), n.events[len(n.events)-eventLimit:]...)
	}
	select {
	case n.eventPing <- struct{}{}:
	default:
	}
}

func (n *Node) recordSync(shareID, peer string, note syncer.Note) {
	level, text := syncer.Describe(note)
	if peer != "" {
		text += " · " + peer
	}
	n.addEvent(shareID, level, text)
}

func peerLabel(name, id string) string {
	if name != "" {
		return name
	}
	if len(id) > 12 {
		return id[:6] + "..." + id[len(id)-4:]
	}
	return id
}
