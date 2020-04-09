package main

import (
	"bytes"
	"github.com/flier/gohs/hyperscan"
	"github.com/google/gopacket/tcpassembly"
	log "github.com/sirupsen/logrus"
	"time"
)

const MaxDocumentSize = 1024 * 1024
const InitialBlockCount = 1024
const InitialPatternSliceSize = 8

// IMPORTANT:  If you use a StreamHandler, you MUST read ALL BYTES from it,
// quickly.  Not reading available bytes will block TCP stream reassembly.  It's
// a common pattern to do this by starting a goroutine in the factory's New
// method:
type StreamHandler struct {
	connection      ConnectionHandler
	streamKey       StreamKey
	buffer          *bytes.Buffer
	indexes         []int
	timestamps      []time.Time
	lossBlocks      []bool
	currentIndex    int
	firstPacketSeen time.Time
	lastPacketSeen  time.Time
	documentsKeys   []RowID
	streamLength    int
	patternStream   hyperscan.Stream
	patternMatches  map[uint][]PatternSlice
}

// NewReaderStream returns a new StreamHandler object.
func NewStreamHandler(connection ConnectionHandler, key StreamKey, scratch *hyperscan.Scratch) StreamHandler {
	handler := StreamHandler{
		connection:     connection,
		streamKey:      key,
		buffer:         new(bytes.Buffer),
		indexes:        make([]int, 0, InitialBlockCount),
		timestamps:     make([]time.Time, 0, InitialBlockCount),
		lossBlocks:     make([]bool, 0, InitialBlockCount),
		documentsKeys:  make([]RowID, 0, 1),               // most of the time the stream fit in one document
		patternMatches: make(map[uint][]PatternSlice, 10), // TODO: change with exactly value
	}

	stream, err := connection.Patterns().Open(0, scratch, handler.onMatch, nil)
	if err != nil {
		log.WithField("streamKey", key).WithError(err).Error("failed to create a stream")
	}
	handler.patternStream = stream

	return handler
}

// Reassembled implements tcpassembly.Stream's Reassembled function.
func (sh *StreamHandler) Reassembled(reassembly []tcpassembly.Reassembly) {
	for _, r := range reassembly {
		skip := r.Skip
		if r.Start {
			skip = 0
			sh.firstPacketSeen = r.Seen
		}
		if r.End {
			sh.lastPacketSeen = r.Seen
		}

		reassemblyLen := len(r.Bytes)
		if reassemblyLen == 0 {
			continue
		}

		if sh.buffer.Len()+len(r.Bytes)-skip > MaxDocumentSize {
			sh.storageCurrentDocument()
			sh.resetCurrentDocument()
		}
		n, err := sh.buffer.Write(r.Bytes[skip:])
		if err != nil {
			log.WithError(err).Error("failed to copy bytes from a Reassemble")
			return
		}
		sh.indexes = append(sh.indexes, sh.currentIndex)
		sh.timestamps = append(sh.timestamps, r.Seen)
		sh.lossBlocks = append(sh.lossBlocks, skip != 0)
		sh.currentIndex += n
		sh.streamLength += n

		err = sh.patternStream.Scan(r.Bytes)
		if err != nil {
			log.WithError(err).Error("failed to scan packet buffer")
		}
	}
}

// ReassemblyComplete implements tcpassembly.Stream's ReassemblyComplete function.
func (sh *StreamHandler) ReassemblyComplete() {
	err := sh.patternStream.Close()
	if err != nil {
		log.WithError(err).Error("failed to close pattern stream")
	}

	if sh.currentIndex > 0 {
		sh.storageCurrentDocument()
	}
	sh.connection.Complete(sh)
}

func (sh *StreamHandler) resetCurrentDocument() {
	sh.buffer.Reset()
	sh.indexes = sh.indexes[:0]
	sh.timestamps = sh.timestamps[:0]
	sh.lossBlocks = sh.lossBlocks[:0]
	sh.currentIndex = 0

	for _, val := range sh.patternMatches {
		val = val[:0]
	}
}

func (sh *StreamHandler) onMatch(id uint, from uint64, to uint64, flags uint, context interface{}) error {
	patternSlices, isPresent := sh.patternMatches[id]
	if isPresent {
		if len(patternSlices) > 0 {
			lastElement := &patternSlices[len(patternSlices)-1]
			if lastElement[0] == from { // make the regex greedy to match the maximum number of chars
				lastElement[1] = to
				return nil
			}
		}
		// new from == new match
		sh.patternMatches[id] = append(patternSlices, PatternSlice{from, to})
	} else {
		patternSlices = make([]PatternSlice, 1, InitialPatternSliceSize)
		patternSlices[0] = PatternSlice{from, to}
		sh.patternMatches[id] = patternSlices
	}

	return nil
}

func (sh *StreamHandler) storageCurrentDocument() {
	payload := (sh.streamKey[0].FastHash()^sh.streamKey[1].FastHash()^sh.streamKey[2].FastHash()^
		sh.streamKey[3].FastHash())&uint64(0xffffffffffffff00) | uint64(len(sh.documentsKeys)) // LOL
	streamKey := sh.connection.Storage().NewCustomRowID(payload, sh.firstPacketSeen)

	_, err := sh.connection.Storage().Insert(ConnectionStreams).
		Context(sh.connection.Context()).
		One(ConnectionStream{
			ID:               streamKey,
			ConnectionID:     ZeroRowID,
			DocumentIndex:    len(sh.documentsKeys),
			Payload:          sh.buffer.Bytes(),
			BlocksIndexes:    sh.indexes,
			BlocksTimestamps: sh.timestamps,
			BlocksLoss:       sh.lossBlocks,
			PatternMatches:   sh.patternMatches,
		})

	if err != nil {
		log.WithError(err).Error("failed to insert connection stream")
	}

	sh.documentsKeys = append(sh.documentsKeys, streamKey)
}
