package smux

import (
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"time"
	"sort"

	"errors"
)

const (
	defaultAcceptBacklog = 1024
)

const (
	errBrokenPipe      = "broken pipe"
	errInvalidProtocol = "invalid protocol version"
	errGoAway          = "stream id overflows, should start a new connection"
)

type writeRequest struct {
	frame  Frame
	result chan writeResult
}

type writeResult struct {
	n   int
	err error
}

// Session defines a multiplexed connection for streams
type Session struct {
	conn io.ReadWriteCloser

	config           *Config
	nextStreamID     uint32 // next stream identifier
	nextStreamIDLock sync.Mutex

	bucket       int32         // token bucket
	bucketNotify chan struct{} // used for waiting for tokens

	streams    map[uint32]*Stream // all streams in this session
	streamLock sync.Mutex         // locks streams

	die       chan struct{} // flag session has died
	dieLock   sync.Mutex
	chAccepts chan *Stream

	dataReady int32 // flag data has arrived

	goAway int32 // flag id exhausted

	deadline atomic.Value

	writes chan writeRequest

	EnableStreamBuffer bool
	MaxReceiveBuffer int
	MaxStreamBuffer int
	BoostTimeout time.Duration

	WriteRequestQueueSize int

	rttSn uint32
	rttTest time.Time
	rtt time.Duration

	test bool
}

func newSession(config *Config, conn io.ReadWriteCloser, client bool) *Session {
	s := new(Session)
	s.die = make(chan struct{})
	s.conn = conn
	s.config = config
	s.streams = make(map[uint32]*Stream)
	s.chAccepts = make(chan *Stream, defaultAcceptBacklog)
	s.bucket = int32(config.MaxReceiveBuffer)
	s.bucketNotify = make(chan struct{}, 1)
	s.writes = make(chan writeRequest)

	s.MaxReceiveBuffer = config.MaxReceiveBuffer
	s.MaxStreamBuffer = config.MaxStreamBuffer
	s.BoostTimeout = config.BoostTimeout
	s.EnableStreamBuffer = config.EnableStreamBuffer

	s.WriteRequestQueueSize = config.WriteRequestQueueSize

	s.test = config.Test
//	s.test = true
//	s.test = false

	if client {
		s.nextStreamID = 1
	} else {
		s.nextStreamID = 0
	}
	go s.recvLoop()
	go s.sendLoop()
	go s.keepalive()
	return s
}

// OpenStream is used to create a new stream
func (s *Session) OpenStream() (*Stream, error) {
	if s.IsClosed() {
		return nil, errors.New(errBrokenPipe)
	}

	// generate stream id
	s.nextStreamIDLock.Lock()
	if s.goAway > 0 {
		s.nextStreamIDLock.Unlock()
		return nil, errors.New(errGoAway)
	}

	s.nextStreamID += 2
	sid := s.nextStreamID
	if sid == sid%2 { // stream-id overflows
		s.goAway = 1
		s.nextStreamIDLock.Unlock()
		return nil, errors.New(errGoAway)
	}
	s.nextStreamIDLock.Unlock()

	stream := newStream(sid, s.config.MaxFrameSize, s)

	if _, err := s.writeFrame(newFrame(cmdSYN, sid)); err != nil {
		return nil, errors.New("writeFrame: " + err.Error())
	}

	s.streamLock.Lock()
	s.streams[sid] = stream
	s.streamLock.Unlock()
	return stream, nil
}

// AcceptStream is used to block until the next available stream
// is ready to be accepted.
func (s *Session) AcceptStream() (*Stream, error) {
	var deadline <-chan time.Time
	if d, ok := s.deadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(d.Sub(time.Now()))
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case stream := <-s.chAccepts:
		return stream, nil
	case <-deadline:
		return nil, errTimeout
	case <-s.die:
		return nil, errors.New(errBrokenPipe)
	}
}

// Close is used to close the session and all streams.
func (s *Session) Close() (err error) {
	s.dieLock.Lock()

	select {
	case <-s.die:
		s.dieLock.Unlock()
		return errors.New(errBrokenPipe)
	default:
		close(s.die)
		s.dieLock.Unlock()
		s.streamLock.Lock()
		for k := range s.streams {
			s.streams[k].sessionClose()
		}
		s.streamLock.Unlock()
		s.notifyBucket()
		return s.conn.Close()
	}
}

// notifyBucket notifies recvLoop that bucket is available
func (s *Session) notifyBucket() {
	select {
	case s.bucketNotify <- struct{}{}:
	default:
	}
}

// IsClosed does a safe check to see if we have shutdown
func (s *Session) IsClosed() bool {
	select {
	case <-s.die:
		return true
	default:
		return false
	}
}

// NumStreams returns the number of currently open streams
func (s *Session) NumStreams() int {
	if s.IsClosed() {
		return 0
	}
	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	return len(s.streams)
}

// SetDeadline sets a deadline used by Accept* calls.
// A zero time value disables the deadline.
func (s *Session) SetDeadline(t time.Time) error {
	s.deadline.Store(t)
	return nil
}

// notify the session that a stream has closed
func (s *Session) streamClosed(sid uint32) {
	s.streamLock.Lock()
	if n := s.streams[sid].recycleTokens(); n > 0 { // return remaining tokens to the bucket
		if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
			s.notifyBucket()
		}
	}
	delete(s.streams, sid)
	s.streamLock.Unlock()
}

// returnTokens is called by stream to return token after read
func (s *Session) returnTokens(n int) {
	if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
		s.notifyBucket()
	}
}

// session read a frame from underlying connection
// it's data is pointed to the input buffer
func (s *Session) readFrame(buffer []byte) (f Frame, err error) {
	if _, err := io.ReadFull(s.conn, buffer[:headerSize]); err != nil {
		return f, errors.New("readFrame: " + err.Error())
	}

	dec := rawHeader(buffer)
	if dec.Version() != version {
		return f, errors.New(errInvalidProtocol)
	}

	f.ver = dec.Version()
	f.cmd = dec.Cmd()
	f.sid = dec.StreamID()
	if length := dec.Length(); length > 0 {
		if _, err := io.ReadFull(s.conn, buffer[headerSize:headerSize+length]); err != nil {
			return f, errors.New("readFrame: " + err.Error())
		}
		f.data = buffer[headerSize : headerSize+length]
	}
	return f, nil
}

// recvLoop keeps on reading from underlying connection if tokens are available
func (s *Session) recvLoop() {
	buffer := make([]byte, (1<<16)+headerSize)
	for {
		for atomic.LoadInt32(&s.bucket) <= 0 && !s.IsClosed() {
			<-s.bucketNotify
		}

		if f, err := s.readFrame(buffer); err == nil {
			atomic.StoreInt32(&s.dataReady, 1)

			switch f.cmd {
			case cmdNOP:
				if s.EnableStreamBuffer {
					s.writeFrame(newFrame(cmdACK, f.sid))
				}
			case cmdSYN:
				s.streamLock.Lock()
				if _, ok := s.streams[f.sid]; !ok {
					stream := newStream(f.sid, s.config.MaxFrameSize, s)
					s.streams[f.sid] = stream
					select {
					case s.chAccepts <- stream:
					case <-s.die:
					}
				}
				s.streamLock.Unlock()
			case cmdFIN:
				s.streamLock.Lock()
				if stream, ok := s.streams[f.sid]; ok {
					stream.markRST()
					stream.notifyReadEvent()
				}
				s.streamLock.Unlock()
			case cmdPSH:
				s.streamLock.Lock()
				if stream, ok := s.streams[f.sid]; ok {
					atomic.AddInt32(&s.bucket, -int32(len(f.data)))
					stream.pushBytes(f.data)
					stream.notifyReadEvent()
				}
				s.streamLock.Unlock()
			case cmdFUL:
				s.streamLock.Lock()
				if stream, ok := s.streams[f.sid]; ok {
					stream.pauseWrite()
				}
				s.streamLock.Unlock()
			case cmdEMP:
				s.streamLock.Lock()
				if stream, ok := s.streams[f.sid]; ok {
					stream.resumeWrite()
					stream.notifyReadEvent()
				}
				s.streamLock.Unlock()
			case cmdACK:
				if f.sid == atomic.LoadUint32(&s.rttSn) {
					s.rtt = time.Now().Sub(s.rttTest) + 1
				}
			default:
				s.Close()
				return
			}
		} else {
			s.Close()
			return
		}
	}
}

func (s *Session) keepalive() {
	tickerPing := time.NewTicker(s.config.KeepAliveInterval)
	tickerTimeout := time.NewTicker(s.config.KeepAliveTimeout)
	defer tickerPing.Stop()
	defer tickerTimeout.Stop()

	s.rttTest = time.Now()
	s.writeFrame(newFrame(cmdNOP, atomic.AddUint32(&s.rttSn, uint32(1))))

	for {
		select {
		case <-tickerPing.C:
			s.rttTest = time.Now()
			s.writeFrame(newFrame(cmdNOP, atomic.AddUint32(&s.rttSn, uint32(1))))
			s.notifyBucket() // force a signal to the recvLoop
		case <-tickerTimeout.C:
			if !atomic.CompareAndSwapInt32(&s.dataReady, 1, 0) {
				s.Close()
				return
			}
		case <-s.die:
			return
		}
	}
}

func (s *Session) sendLoop() {
	buf := make([]byte, (1<<16)+headerSize)

	var queueLock sync.Mutex
	QueueSize := s.WriteRequestQueueSize
	streamQueues := make(map[uint32](chan writeRequest))
	writeNotify := make(chan struct{}, 1)
	var reqCount int32 = 0
	writes := make(chan writeRequest)
if !s.test {
	writes = make(chan writeRequest, 32)
	go func() {
		for {
			select {
			case <-s.die:
				return
			case request, ok := <-writes:
				if !ok {
					continue
				}

				buf[0] = request.frame.ver
				buf[1] = request.frame.cmd
				binary.LittleEndian.PutUint16(buf[2:], uint16(len(request.frame.data)))
				binary.LittleEndian.PutUint32(buf[4:], request.frame.sid)
				copy(buf[headerSize:], request.frame.data)
				n, err := s.conn.Write(buf[:headerSize+len(request.frame.data)])

				n -= headerSize
				if n < 0 {
					n = 0
				}

				result := writeResult{
					n:   n,
					err: err,
				}

				request.result <- result
				close(request.result)
			}
		}
	}()

	go func() {
		for {
			select {
			case <-s.die:
				return
			case <-writeNotify:
				for atomic.LoadInt32(&reqCount) > 0 {
					sids := make([]uint32, 0)
					queueLock.Lock()
					for sid, _ := range streamQueues {
						sids = append(sids, sid)
					}
					queueLock.Unlock()

					sort.Slice(sids, func(i, j int) bool { return sids[i] < sids[j] })

					for _, sid := range sids {
						queueLock.Lock()
						if queue, ok := streamQueues[sid]; ok {
							queueLock.Unlock()

							select {
							case request := <-queue:
								if request.frame.cmd == cmdFIN {
									queueLock.Lock()
									delete(streamQueues, sid)
									queueLock.Unlock()
								}
								writes <- request
								atomic.AddInt32(&reqCount, -1)
							default:
							}
						} else {
							queueLock.Unlock()
						}
					}
				}
			}
		}
	}()
}

	for {
		var request writeRequest
		var ok bool
		select {
		case <-s.die:
			return
		case request, ok = <-s.writes:
			if !ok {
				continue
			}
			if s.test {
				buf[0] = request.frame.ver
				buf[1] = request.frame.cmd
				binary.LittleEndian.PutUint16(buf[2:], uint16(len(request.frame.data)))
				binary.LittleEndian.PutUint32(buf[4:], request.frame.sid)
				copy(buf[headerSize:], request.frame.data)
				n, err := s.conn.Write(buf[:headerSize+len(request.frame.data)])

				n -= headerSize
				if n < 0 {
					n = 0
				}

				result := writeResult{
					n:   n,
					err: err,
				}

				request.result <- result
				close(request.result)
				continue
			}

			f := request.frame
			switch f.cmd {
			case cmdSYN:
				queueLock.Lock()
				queue, ok := streamQueues[f.sid]
				if !ok {
					queue = make(chan writeRequest, QueueSize)
					streamQueues[f.sid] = queue
				}
				queueLock.Unlock()

				queue <- request
				atomic.AddInt32(&reqCount, 1)

			case cmdFIN:
				queueLock.Lock()
				if queue, ok := streamQueues[f.sid]; ok {
					queueLock.Unlock()

					select {
					case queue <- request:
						atomic.AddInt32(&reqCount, 1)
					default:
						// queue full
						request2 := <-queue
						queue <- request
						writes <- request2
					}
				} else {
					queueLock.Unlock()
					writes <- request
				}

			case cmdPSH:
				queueLock.Lock()
				queue, ok := streamQueues[f.sid]
				if !ok {
					queue = make(chan writeRequest, QueueSize)
					streamQueues[f.sid] = queue
				}
				queueLock.Unlock()

				select {
				case queue <- request:
					atomic.AddInt32(&reqCount, 1)
				default:
					// queue full
					request2 := <-queue
					queue <- request
					writes <- request2
				}

			default:
				writes <- request
				continue
			}

			select {
			case writeNotify <- struct{}{}:
			default:
			}

		}

	}
}

// writeFrame writes the frame to the underlying connection
// and returns the number of bytes written if successful
func (s *Session) writeFrame(f Frame) (n int, err error) {
	req := writeRequest{
		frame:  f,
		result: make(chan writeResult, 1),
	}
	select {
	case <-s.die:
		return 0, errors.New(errBrokenPipe)
	case s.writes <- req:
	}

	result := <-req.result
	return result.n, result.err
}

func (s *Session) WriteCustomCMD(cmd byte, bts []byte) (n int, err error) {
	if s.IsClosed() {
		return 0, errors.New(errBrokenPipe)
	}
	f := newFrame(cmd, 0)
	f.data = bts

	return s.writeFrame(f)
}

