package quic

import (
	"errors"
	"fmt"
	"sync"

	"github.com/lucas-clemente/quic-go/protocol"
	"github.com/lucas-clemente/quic-go/qerr"
	"github.com/lucas-clemente/quic-go/utils"
)

type streamsMap struct {
	mutex sync.RWMutex

	streams     map[protocol.StreamID]*stream
	openStreams []protocol.StreamID

	highestStreamOpenedByClient          protocol.StreamID
	streamsOpenedAfterLastGarbageCollect int

	newStream     newStreamLambda
	maxNumStreams int

	roundRobinIndex int
}

type streamLambda func(*stream) (bool, error)
type newStreamLambda func(protocol.StreamID) (*stream, error)

var (
	errMapAccess = errors.New("streamsMap: Error accessing the streams map")
)

func newStreamsMap(newStream newStreamLambda) *streamsMap {
	maxNumStreams := utils.Max(int(float32(protocol.MaxIncomingDynamicStreams)*protocol.MaxStreamsMultiplier), int(protocol.MaxIncomingDynamicStreams))

	return &streamsMap{
		streams:       map[protocol.StreamID]*stream{},
		openStreams:   make([]protocol.StreamID, 0, maxNumStreams),
		newStream:     newStream,
		maxNumStreams: maxNumStreams,
	}
}

// GetOrOpenStream either returns an existing stream, a newly opened stream, or nil if a stream with the provided ID is already closed.
// Newly opened streams should only originate from the client. To open a stream from the server, OpenStream should be used.
func (m *streamsMap) GetOrOpenStream(id protocol.StreamID) (*stream, error) {
	m.mutex.RLock()
	s, ok := m.streams[id]
	m.mutex.RUnlock()
	if ok {
		return s, nil // s may be nil
	}

	// ... we don't have an existing stream, try opening a new one
	m.mutex.Lock()
	defer m.mutex.Unlock()
	// We need to check whether another invocation has already created a stream (between RUnlock() and Lock()).
	s, ok = m.streams[id]
	if ok {
		return s, nil
	}
	if len(m.openStreams) == m.maxNumStreams {
		return nil, qerr.TooManyOpenStreams
	}
	if id%2 == 0 {
		return nil, qerr.Error(qerr.InvalidStreamID, fmt.Sprintf("attempted to open stream %d from client-side", id))
	}
	if id+protocol.MaxNewStreamIDDelta < m.highestStreamOpenedByClient {
		return nil, qerr.Error(qerr.InvalidStreamID, fmt.Sprintf("attempted to open stream %d, which is a lot smaller than the highest opened stream, %d", id, m.highestStreamOpenedByClient))
	}

	s, err := m.newStream(id)
	if err != nil {
		return nil, err
	}

	if id > m.highestStreamOpenedByClient {
		m.highestStreamOpenedByClient = id
	}

	m.streamsOpenedAfterLastGarbageCollect++
	if m.streamsOpenedAfterLastGarbageCollect%protocol.MaxNewStreamIDDelta == 0 {
		m.garbageCollectClosedStreams()
	}

	m.putStream(s)
	return s, nil
}

// OpenStream opens a stream from the server's side
func (m *streamsMap) OpenStream(id protocol.StreamID) (*stream, error) {
	panic("OpenStream: not implemented")
}

func (m *streamsMap) Iterate(fn streamLambda) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	for _, streamID := range m.openStreams {
		cont, err := m.iterateFunc(streamID, fn)
		if err != nil {
			return err
		}
		if !cont {
			break
		}
	}
	return nil
}

// RoundRobinIterate executes the streamLambda for every open stream, until the streamLambda returns false
// It uses a round-robin-like scheduling to ensure that every stream is considered fairly
// It prioritizes the crypto- and the header-stream (StreamIDs 1 and 3)
func (m *streamsMap) RoundRobinIterate(fn streamLambda) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	numStreams := len(m.openStreams)
	startIndex := m.roundRobinIndex

	for _, i := range []protocol.StreamID{1, 3} {
		cont, err := m.iterateFunc(i, fn)
		if err != nil && err != errMapAccess {
			return err
		}
		if !cont {
			return nil
		}
	}

	for i := 0; i < numStreams; i++ {
		streamID := m.openStreams[(i+startIndex)%numStreams]

		if streamID == 1 || streamID == 3 {
			continue
		}

		cont, err := m.iterateFunc(streamID, fn)
		if err != nil {
			return err
		}
		m.roundRobinIndex = (m.roundRobinIndex + 1) % numStreams
		if !cont {
			break
		}
	}
	return nil
}

func (m *streamsMap) iterateFunc(streamID protocol.StreamID, fn streamLambda) (bool, error) {
	str, ok := m.streams[streamID]
	if !ok {
		return true, errMapAccess
	}
	if str == nil {
		return false, fmt.Errorf("BUG: Stream %d is closed, but still in openStreams map", streamID)
	}
	return fn(str)
}

func (m *streamsMap) putStream(s *stream) error {
	id := s.StreamID()
	if _, ok := m.streams[id]; ok {
		return fmt.Errorf("a stream with ID %d already exists", id)
	}

	m.streams[id] = s
	m.openStreams = append(m.openStreams, id)

	return nil
}

// Attention: this function must only be called if a mutex has been acquired previously
func (m *streamsMap) RemoveStream(id protocol.StreamID) error {
	s, ok := m.streams[id]
	if !ok || s == nil {
		return fmt.Errorf("attempted to remove non-existing stream: %d", id)
	}

	m.streams[id] = nil

	for i, s := range m.openStreams {
		if s == id {
			// delete the streamID from the openStreams slice
			m.openStreams = m.openStreams[:i+copy(m.openStreams[i:], m.openStreams[i+1:])]
			// adjust round-robin index, if necessary
			if i < m.roundRobinIndex {
				m.roundRobinIndex--
			}
			break
		}
	}

	return nil
}

// NumberOfStreams gets the number of open streams
func (m *streamsMap) NumberOfStreams() int {
	m.mutex.RLock()
	n := len(m.openStreams)
	m.mutex.RUnlock()
	return n
}

// garbageCollectClosedStreams deletes nil values in the streams if they are smaller than protocol.MaxNewStreamIDDelta than the highest stream opened by the client
// note that this garbage collection is relatively expensive, since it iterates over the whole streams map. It should not be called every time a stream is openend or closed
func (m *streamsMap) garbageCollectClosedStreams() {
	for id, str := range m.streams {
		if str != nil {
			continue
		}
		if id+protocol.MaxNewStreamIDDelta <= m.highestStreamOpenedByClient {
			delete(m.streams, id)
		}
	}
	m.streamsOpenedAfterLastGarbageCollect = 0
}
