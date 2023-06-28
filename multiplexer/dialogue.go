package multiplexer

import (
	"errors"

	"github.com/singchia/geminio/packet"
)

var (
	ErrOperationOnClosedMultiplexer = errors.New("operation on closed multiplexer")
	ErrDialogueNotFound             = errors.New("dialogue not found")
)

// dialogue manager
type Multiplexer interface {
	OpenDialogue(meta []byte) (Dialogue, error)
	AcceptDialogue(Dialogue, error)
}

// dialogue
type Reader interface {
	Read() (packet.Packet, error)
}

type Writer interface {
	Write(pkt packet.Packet) error
}

type Closer interface {
	Close()
}

type Side int

const (
	ClientSide Side = 0
	ServerSide Side = 1
)

type DialogueDescriber interface {
	DialogueID() uint64
	Meta() []byte
	Side() Side
}

type Dialogue interface {
	Reader
	Writer
	Closer

	// meta
	DialogueID() uint64
	Meta() []byte
	Side() Side
}
